# serverscom-exporter

Prometheus exporter for the [Servers.com Public API](https://developers.servers.com/api-documentation/v1/).
It exposes Enterprise Bare Metal inventory and state: servers with their labels,
power and OOB state, drives, switch ports, networks, L2 segments and uplink
aggregation.

## How it works

The API allows 2,000 requests per hour **per account**, shared with Terraform,
the CLI and everything else using the same account. The exporter therefore
never calls the API during a scrape. Background loops keep an in-memory cache,
and `/metrics` only renders the current snapshot.

| Loop          | Data                                                        | Default interval |
|---------------|-------------------------------------------------------------|------------------|
| hosts         | server list; new and changed servers are downloaded fully   | `2m`             |
| features, power-feeds | split bonding, OOB public access, rescue mode; power feeds | `15m`      |
| l2-segments   | L2 segments, members, networks                              | `1h`             |
| reference     | locations and racks                                         | `6h`             |
| configuration, drives, ports, networks, services, uplinks | inventory, prices, uplink catalogues | `24h` |

Each is set with `--refresh.<name>-interval`. When the intervals need more
than 80% of `--api.requests-per-hour`, every loop but the server list is
stretched by the same factor (the list too if it does not fit on its own),
see `serverscom_exporter_refresh_interval_stretch_factor`. A pass of a
per-server collector refreshes every server whose data is older than the
interval, one request after another.

### Request limits

Two limits apply, and the exporter never delays requests while both have room:

- **Its own budget**, `--api.requests-per-hour` (default 1000). The window
  follows the account window (`X-RateLimit-Reset`) and starts over when it
  resets. When the window is used up, the exporter logs a warning, sets
  `serverscom_api_budget_exhausted` and waits for the reset.
- **The account limit** (`X-RateLimit-*`). When `X-RateLimit-Remaining` drops
  to `--api.ratelimit-reserve` (default 300), the exporter waits until the
  account window resets, leaving the rest to Terraform, the CLI and others.
  A 429 response pauses it as well (`serverscom_api_ratelimit_paused`). The
  last state, with the requests spent in the budget window, is kept in the
  cache file, so a restart neither sends a request into a used-up window nor
  gets a fresh budget in the same window.

### Demand estimate and recommendations

From the number of servers, segments and racks and the configured intervals,
the exporter estimates how many requests per hour it needs, how far the
intervals are stretched and how many requests a full download costs (e.g.
after a start without a cache file). It exposes the estimate as metrics and
logs it whenever it changes:

```
level=WARN msg="Refresh intervals do not fit into the request budget and are stretched; raise the budget to keep them"
  stretch=1.3 requests_per_hour=400 configured_requests_per_hour=520 budget_per_hour=500 ... recommended="--api.requests-per-hour=650"
```

The recommended budget fits the configured intervals into 80% of it, covers
one full download, and is at most the account limit minus the reserve.
`--estimate` prints the same for given flags and exits (JSON, or a table with
`--log.format=logfmt`); with a token it reads the sizes from the API with a
few requests, otherwise from `--cache.file` or the `--estimate.*` flags.

With `--cache.file` the cache is written to disk. After a restart the exporter
serves full metrics immediately and only refetches what is due.

Run a **single replica**: every replica spends the same account limit.

## Usage

```sh
export SERVERSCOM_API_TOKEN=...
serverscom-exporter --cache.file=/var/lib/serverscom-exporter/cache.json
```

The token can also be read from a file (`--api.token-file`, handy for Kubernetes
secrets). TLS and basic auth are configured with `--web.config.file`
([exporter-toolkit](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)).
Run `--help` for all flags.

## Metrics

Every `serverscom_host_*` metric has `host_id`, `title`, `location` and `rack`
labels, so dashboards can filter by location and rack without joins.

| Metric | Description |
|--------|-------------|
| `serverscom_host_info` | Server attributes: rack_id, type, configuration, IPs (public/private/OOB), lease start |
| `serverscom_host_labels` | Servers.com labels as `label_*` (select with `--collector.host-labels`) |
| `serverscom_host_status`, `_operational_status`, `_power_status` | Raw API statuses (`status` label, value 1) |
| `serverscom_host_powered_on` | 1 if `power_status` is `powered_on` |
| `serverscom_host_scheduled_release_timestamp_seconds` | Scheduled release time, if any |
| `serverscom_host_configuration_info` | Server model, OS, uplink and bandwidth names |
| `serverscom_host_ram_size` | `configuration_details.ram_size` as reported by the API |
| `serverscom_host_drive_slot_info` | Drive slots: interface, form factor, populated, drive model, media type |
| `serverscom_host_drive_capacity` | `drive_model.capacity` of populated slots, as reported by the API |
| `serverscom_host_connection_info` / `_connections` | Switch ports (port, type, MAC) / port count by type |
| `serverscom_host_network_info` | Networks attached to the server |
| `serverscom_host_feature_status` / `_feature_enabled` | Server features (e.g. `oob_public_access`, `disaggregated_*_ports`) |
| `serverscom_host_power_feed_status` / `_power_feed_up` | Power feeds |
| `serverscom_host_service_price`, `_service_subtotal`, `_service_tax` | Billed services of the current billing period by `service`, `type`, `currency`; services with the same name and type are summed |
| `serverscom_host_uplink_info` / `_uplink_redundant` | Uplink model, speed, redundancy |
| `serverscom_host_uplink_aggregated` | 1 if the uplink is redundant and not disaggregated, i.e. aggregated on the switch side |
| `serverscom_host_details_last_refresh_timestamp_seconds` | When a per-server collector was last fetched, by `collector` |
| `serverscom_l2_segment_info`, `_member_info`, `_network_info` | L2 segments, attached servers (mode, VLAN), networks |
| `serverscom_location_info`, `serverscom_rack_info` | Reference data for dashboard variables |
| `serverscom_exporter_last_refresh_success_timestamp_seconds` | Cache freshness by resource |
| `serverscom_exporter_refresh_errors_total` | Failed refreshes by resource |
| `serverscom_api_requests_total`, `_request_duration_seconds` | Exporter API usage by endpoint |
| `serverscom_api_ratelimit_limit`, `_remaining`, `_reset_timestamp_seconds` | Account-wide rate limit state |
| `serverscom_api_ratelimit_reserve`, `serverscom_api_requests_budget_per_hour` | Configured reserve and request budget, for rate limit forecasts |
| `serverscom_api_ratelimit_paused` | 1 while the exporter waits because the account reached the reserve |
| `serverscom_api_budget_used`, `_remaining`, `_reset_timestamp_seconds` | Exporter's own hourly budget window (`--api.requests-per-hour`) |
| `serverscom_api_budget_exhausted` / `_exhausted_total` | 1 while the current budget window is used up / number of used up windows |
| `serverscom_exporter_requests_per_hour_estimated` | Requests per hour of each `loop` at its effective interval |
| `serverscom_exporter_full_refresh_requests_estimated` | Requests to download all information once |
| `serverscom_exporter_refresh_interval_seconds` / `_configured_seconds` | Effective / configured refresh interval, by `loop` |
| `serverscom_exporter_refresh_interval_stretch_factor` | How far the intervals are stretched to fit the budget; 1 when they fit |
| `serverscom_api_requests_per_hour_recommended` | Recommended `--api.requests-per-hour` |

Grafana variables for location and rack:

```
label_values(serverscom_host_info, location)
label_values(serverscom_host_info{location=~"$location"}, rack)
```

## To verify against the live API

- The full set of feature `status` values. `feature_enabled` and `uplink_aggregated`
  assume an enabled feature reports `activated`.
- Units of `ram_size` and drive `capacity` (most likely GB).
- Whether `updated_at` changes when drives or ports change. If it does not,
  lower `--refresh.drives-interval` / `--refresh.ports-interval` (`24h` by
  default). Toggling a feature, e.g. port disaggregation, does **not** change
  it; `--refresh.features-interval` (`15m`) covers that.
- The field types of `/hosts/dedicated_servers/{id}/services` (only an example
  is documented) and whether `configuration_details` is in the server list.

## Development

All commands run from `src/`:

```sh
go test -race ./...
go build ./cmd/serverscom-exporter
```

`make build` and `make image` in the repository root add the version
information from git; see the [Makefile](../Makefile).

## Docker image

The [Dockerfile](Dockerfile) builds a static binary and ships it in
`gcr.io/distroless/static-debian13:nonroot` (no shell, uid 65532). Version
information is injected at build time and exposed via `--version`, the landing
page and `serverscom_exporter_build_info`:

```sh
docker build -t serverscom-exporter \
  --build-arg VERSION=$(git describe --tags --match 'v*' --always) \
  --build-arg REVISION=$(git rev-parse HEAD) \
  --build-arg BRANCH=$(git rev-parse --abbrev-ref HEAD) \
  --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  .
```

Multi-arch builds work with `docker buildx build --platform linux/amd64,linux/arm64`.
To download modules through your own proxy (Athens, Nexus, ...), pass
`--build-arg GOPROXY=https://...`; checksums are still verified against `go.sum`.

To persist the cache across restarts, mount a writable volume and point
`--cache.file` at it:

```sh
docker run -v serverscom-cache:/data -e SERVERSCOM_API_TOKEN=... \
  serverscom-exporter --cache.file=/data/cache.json
```
