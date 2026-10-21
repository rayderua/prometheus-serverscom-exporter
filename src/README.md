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

| Loop      | Data                                                         | Default interval                              |
|-----------|--------------------------------------------------------------|-----------------------------------------------|
| hosts     | server list: statuses, power, labels, `updated_at`            | `--refresh.hosts-interval=2m`                 |
| details   | drives, ports, networks, features, power feeds, configuration | `--refresh.details-interval=12h`, or as soon as `updated_at` changes |
| features  | server features only (port aggregation, OOB public access)    | `--refresh.features-interval=1h`, one request per server |
| l2        | L2 segments, members, networks                                | `--refresh.l2-interval=15m`                   |
| reference | locations, racks, uplink model catalogues                     | `--refresh.reference-interval=24h`            |

### Request limits

Two limits apply, and the exporter never delays requests while both have room:

- **Its own budget**, `--api.requests-per-hour` (default 1000). Counted like
  the API limit: a window opens with the first request and resets an hour
  later. When the window is used up, the exporter logs a warning, sets
  `serverscom_api_budget_exhausted` and waits for the reset.
- **The account limit** (`X-RateLimit-*`). When `X-RateLimit-Remaining` drops
  to `--api.ratelimit-reserve` (default 300), the exporter waits until the
  account window resets, leaving the rest to Terraform, the CLI and others.
  A 429 response pauses it as well.

### Demand estimate and recommendations

From the number of servers, segments and racks and the configured intervals,
the exporter estimates how many requests per hour it needs and how many a full
download costs (e.g. after a start without a cache file). It exposes the
estimate as metrics and, whenever the recommendation changes, logs whether the
configuration fits, with the flags to change if not:

```
level=WARN msg="API demand does not fit into the request budget; the exporter will wait for budget resets"
  requests_per_hour=538 full_refresh_requests=243 budget_per_hour=500 ... recommended="--api.requests-per-hour=700"
```

The recommended budget is the estimate with 25% headroom, at least one full
download, and at most the account limit minus the reserve. If even that is not
enough, the recommended refresh intervals are raised proportionally.

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
| `serverscom_host_uplink_info` / `_uplink_redundant` | Uplink model, speed, redundancy |
| `serverscom_host_uplink_aggregated` | 1 if the uplink is redundant and not disaggregated, i.e. aggregated on the switch side |
| `serverscom_host_details_last_refresh_timestamp_seconds` | When the server details were fetched |
| `serverscom_l2_segment_info`, `_member_info`, `_network_info` | L2 segments, attached servers (mode, VLAN), networks |
| `serverscom_location_info`, `serverscom_rack_info` | Reference data for dashboard variables |
| `serverscom_exporter_last_refresh_success_timestamp_seconds` | Cache freshness by resource |
| `serverscom_exporter_refresh_errors_total` | Failed refreshes by resource |
| `serverscom_api_requests_total`, `_request_duration_seconds` | Exporter API usage by endpoint |
| `serverscom_api_ratelimit_limit`, `_remaining`, `_reset_timestamp_seconds` | Account-wide rate limit state |
| `serverscom_api_ratelimit_reserve`, `serverscom_api_requests_budget_per_hour` | Configured reserve and request budget, for rate limit forecasts |
| `serverscom_api_budget_used`, `_remaining`, `_reset_timestamp_seconds` | Exporter's own hourly budget window (`--api.requests-per-hour`) |
| `serverscom_api_budget_exhausted` / `_exhausted_total` | 1 while the current budget window is used up / number of used up windows |
| `serverscom_exporter_requests_per_hour_estimated` | Requests per hour the configuration needs, by `loop` |
| `serverscom_exporter_full_refresh_requests_estimated` | Requests to download all information once |
| `serverscom_exporter_refresh_interval_seconds` / `_recommended_seconds` | Configured / recommended refresh interval, by `loop` |
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
  lower `--refresh.details-interval`. (Toggling a feature, e.g. port
  disaggregation, does **not** change it; the features loop covers that.)
- The maximum `per_page` value.

## Development

All commands run from `src/`:

```sh
go test -race ./...
go build ./cmd/serverscom-exporter
```

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
