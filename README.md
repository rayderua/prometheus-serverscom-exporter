# serverscom-exporter

Prometheus monitoring for [Servers.com](https://www.servers.com/) Enterprise Bare
Metal: an exporter for the [Servers.com Public API](https://developers.servers.com/api-documentation/v1/),
a Helm chart with alert rules, and a Grafana dashboard.

The exporter turns the inventory and state of your dedicated servers into
metrics: power and operational status, labels, drives, switch ports, networks,
power feeds, server features (port aggregation, OOB public access), uplinks,
L2 segments, locations and racks. It is built around the API rate limit:
scrapes never call the API, and the exporter keeps its own request budget so
that Terraform, the CLI and other users of the account are not starved.

## Contents

- [Repository layout](#repository-layout)
- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [API rate limits and sizing](#api-rate-limits-and-sizing)
- [Cache persistence](#cache-persistence)
- [Metrics](#metrics)
- [Kubernetes](#kubernetes)
- [Grafana dashboard](#grafana-dashboard)
- [Alerts](#alerts)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [CI and releases](#ci-and-releases)
- [Known gaps](#known-gaps)
- [Changelog](CHANGELOG.md)

## Repository layout

| Path | Contents |
|------|----------|
| [`src/`](src/README.md) | The exporter (Go), its Dockerfile and the full metric reference |
| [`charts/serverscom-exporter/`](charts/serverscom-exporter/README.md) | Helm chart: Deployment, cache PVC, ServiceMonitor, PrometheusRule, NetworkPolicy |
| [`grafana/`](grafana/README.md) | Grafana dashboard (v2 resource with tabs) |
| [`.github/workflows/`](.github/workflows) | CI and release workflows |

## How it works

```mermaid
flowchart LR
    API[(Servers.com API<br/>2000 req/h per account)]
    subgraph exporter [serverscom-exporter]
        L[Refresh loops<br/>hosts · one per collector]
        B{Request budget<br/>and account reserve}
        C[(In-memory cache)]
        F[/cache.json/]
        M[/metrics/]
    end
    P[Prometheus]

    L --> B --> API
    API --> L --> C
    C <--> F
    C --> M
    P -- scrape --> M
```

- **Refresh loops** fetch data in the background, each at its own interval.
  A scrape of `/metrics` only renders the current cache and never calls the
  API, so scrape frequency does not affect API usage.
- **Request limits.** Every API request passes two checks: the exporter's own
  hourly budget (`--api.requests-per-hour`) and the account-wide limit
  reported in `X-RateLimit-*` headers, of which the exporter leaves a reserve
  (`--api.ratelimit-reserve`) to other users. Requests are not delayed while
  both have room. See [API rate limits and sizing](#api-rate-limits-and-sizing).
- **Cache file.** With `--cache.file` the cache is written to disk. After a
  restart the exporter serves full metrics immediately and only refetches
  what is due.
- **Single instance.** Run one exporter per account: every replica spends the
  same account limit.

Every collector (API endpoint) has its own interval,
`--refresh.<collector>-interval`: a fixed duration or `auto`.

| Loop | Data | Default interval | Cost per run |
|------|------|------------------|--------------|
| hosts | server list: statuses, power, labels, `updated_at`; then all collectors of new servers and servers whose `updated_at` changed | `2m`, always fixed | 1 request per 100 servers (page size) |
| configuration | server model, RAM, uplinks (`GET /hosts/dedicated_servers/{id}`) | `auto` | 1 request per server |
| drives | drive slots | `auto` | 1 request per server |
| ports | switch port connections | `auto` | 1 request per server |
| networks | IP networks | `auto` | 1 request per server |
| features | port aggregation, OOB public access | `auto` | 1 request per server |
| power-feeds | power feeds | `auto` | 1 request per server |
| uplinks | uplink model catalogues of the server models in use | `auto` | 1 request per model and location |
| l2-segments | L2 segments, members, networks | `auto` | 1 page + 2 requests per segment |
| reference | locations, racks | `auto` | 2 requests |

Per-server collectors refresh the server with the oldest data first and
spread requests evenly over the interval (one server every
`interval / servers`), so there are no bursts. Toggling a feature (e.g.
splitting the port aggregation) does not change `updated_at`; if you need to
catch it within a fixed time, set `--refresh.features-interval`, e.g. `15m`.

### Auto intervals

All `auto` collectors share one interval, computed from the budget left after
the fixed ones:

```
auto interval = auto requests per run / (budget × 80% − fixed requests per hour)
```

It is rounded up to 5 minutes, kept within `--refresh.auto-min-interval`
(`5m`) and `--refresh.auto-max-interval` (`24h`), and recomputed when the
number of servers or segments changes. The other 20% of the budget is left
for new and changed servers and retries. When the budget window has 20% or
less left, auto collectors pause until the reset and fixed ones keep running,
so a critical collector such as `features` is not starved by the others.

The effective intervals are logged (`Refresh intervals computed
auto_interval=...`) and exposed as `serverscom_exporter_refresh_interval_seconds`
and `serverscom_exporter_refresh_interval_auto`.

## Quick start

You need a Servers.com API token, created in the Servers.com customer portal.
The exporter only sends `GET` requests.

### Docker

```sh
docker run -d --name serverscom-exporter -p 9920:9920 \
  -e SERVERSCOM_API_TOKEN=... \
  -v serverscom-cache:/data \
  ghcr.io/rayderua/prometheus-serverscom-exporter:<version> \
  --cache.file=/data/cache.json

curl -s localhost:9920/metrics | grep serverscom_host_info | head
```

The image is based on `gcr.io/distroless/static-debian13:nonroot` (no shell,
uid 65532) and is published for linux/amd64 and linux/arm64. Image tags:
`X.Y.Z`, `X.Y` and `latest`.

### Kubernetes (Helm)

```sh
kubectl create namespace monitoring
kubectl -n monitoring create secret generic serverscom-api-token \
  --from-literal=token=$SERVERSCOM_API_TOKEN

helm install serverscom-exporter oci://ghcr.io/rayderua/charts/serverscom-exporter \
  --version <chart-version> -n monitoring \
  --set apiToken.existingSecret=serverscom-api-token \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

The chart is also available from a classic Helm repository:

```sh
helm repo add serverscom https://rayderua.github.io/prometheus-serverscom-exporter
helm install serverscom-exporter serverscom/serverscom-exporter -n monitoring ...
```

See [Kubernetes](#kubernetes) and the [chart README](charts/serverscom-exporter/README.md).

### From source

```sh
cd src
go build ./cmd/serverscom-exporter
SERVERSCOM_API_TOKEN=... ./serverscom-exporter --cache.file=./cache.json
```

### Prometheus scrape config

Without the Prometheus Operator:

```yaml
scrape_configs:
  - job_name: serverscom
    scrape_interval: 1m   # metrics change with the refresh loops, not faster
    scrape_timeout: 30s
    static_configs:
      - targets: ["serverscom-exporter:9920"]
```

## Configuration

All settings are command-line flags; run `serverscom-exporter --help` for the
complete list.

### API token

Set exactly one of:

| Source | Example |
|--------|---------|
| File (recommended, e.g. a mounted Kubernetes Secret) | `--api.token-file=/etc/serverscom/token` or `SERVERSCOM_API_TOKEN_FILE` |
| Environment variable | `SERVERSCOM_API_TOKEN=...` |
| Flag (visible in the process list, avoid) | `--api.token=...` |

The exporter exits at startup if no token is set or both a token and a token
file are given.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api.url` (`SERVERSCOM_API_URL`) | `https://api.servers.com/v1` | API base URL |
| `--api.timeout` | `30s` | Timeout of a single request |
| `--api.page-size` | `100` | `per_page` for collection endpoints, 1–100 (the API maximum) |
| `--api.requests-per-hour` | `1000` | Exporter's own hourly request budget |
| `--api.ratelimit-reserve` | `300` | Pause when the account-wide remaining requests drop to this value |
| `--refresh.hosts-interval` | `2m` | Server list; new and changed servers are downloaded on the same schedule |
| `--refresh.<collector>-interval` | `auto` | Interval of a collector: a duration or `auto`, see [How it works](#how-it-works) |
| `--refresh.auto-min-interval` | `5m` | Lower bound of the auto interval |
| `--refresh.auto-max-interval` | `24h` | Upper bound of the auto interval |
| `--cache.file` | empty (off) | File to persist the cache to |
| `--cache.save-interval` | `1m` | How often to write the cache file if it changed |
| `--collector.host-labels` | `*` | Servers.com label keys exposed in `serverscom_host_labels`, comma-separated; `*` for all |
| `--label=<name>=<value>` | none | Extra label added to every metric; repeatable, see [Extra labels](#extra-labels) |
| `--[no-]collector.<name>` | on | Optional collectors, see [Collectors](#collectors) |
| `--web.listen-address` | `:9920` | Listen address; repeatable |
| `--web.telemetry-path` | `/metrics` | Metrics path |
| `--web.config.file` | empty | TLS and basic auth, see below |
| `--log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `--log.format` | `logfmt` | `logfmt` or `json` |

### Extra labels

`--label=<name>=<value>` adds a label to every metric of the exporter,
e.g. the account a token belongs to (the API does not expose account
details):

```sh
--label=account=prod --label=env=production
```

In the Helm chart: `config.extraLabels: {account: prod, env: production}`.
Names must be valid Prometheus label names, must not start with `__` or
`label_` (used for [host labels](#host-labels)) and must not clash with the
exporter's own labels such as `host_id`, `location`, `rack`, `loop` or
`endpoint`; the exporter refuses to start otherwise. If Prometheus adds a
target label with the same name, it keeps the exporter's value as
`exported_<name>` unless `honor_labels` is set.

### Collectors

Optional data can be turned off with `--no-collector.<name>`. A disabled
collector is neither fetched from the API nor exposed, which saves requests;
its data is dropped from the cache file on start.

| Collector | Metrics | API requests |
|-----------|---------|--------------|
| `drives` | `serverscom_host_drive_slot_info`, `_drive_capacity` | one per server and details refresh |
| `ports` | `serverscom_host_connection_info`, `_connections` | one per server and details refresh |
| `networks` | `serverscom_host_network_info` | one per server and details refresh |
| `features` | `serverscom_host_feature_status`, `_feature_enabled`; also the features loop | one per server and details refresh, plus the features loop |
| `power-feeds` | `serverscom_host_power_feed_status`, `_power_feed_up` | one per server and details refresh |
| `uplinks` | `serverscom_host_uplink_info`, `_uplink_redundant`, `_uplink_aggregated` | one per server model and location, daily |
| `l2-segments` | `serverscom_l2_segment_*` | the L2 loop |

`serverscom_host_uplink_aggregated` also needs `features`; without it the
metric is not exposed. When a collector is turned on again, servers whose
cached details lack it are refreshed on the next check.

```sh
--no-collector.drives --no-collector.features --no-collector.l2-segments
```

In the Helm chart: `config.collectors.<name>: false`.

### HTTP endpoints

| Path | Description |
|------|-------------|
| `/metrics` | Metrics (OpenMetrics supported) |
| `/-/healthy` | Returns `OK` while the process runs |
| `/` | Landing page with version and links |

### TLS and basic auth

`--web.config.file` takes an
[exporter-toolkit web config](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md):

```yaml
tls_server_config:
  cert_file: /etc/serverscom-exporter/tls/tls.crt
  key_file: /etc/serverscom-exporter/tls/tls.key
basic_auth_users:
  prometheus: $2y$10$...   # bcrypt hash, e.g. htpasswd -nBC 10 "" | tr -d ':\n'
```

Basic auth covers every path, including `/-/healthy`.

### Host labels

Servers.com labels of a server become `label_<key>` labels of
`serverscom_host_labels`. Keys are sanitized to valid Prometheus label names.
Limit them to what you use to keep series small:

```sh
--collector.host-labels=env,team,role
```

## API rate limits and sizing

The Servers.com API allows **2000 requests per hour per account**, shared by
everything that uses the account. The exporter applies two limits:

1. **Its own budget**, `--api.requests-per-hour` (default 1000). The window
   opens with the first request and resets an hour later, like the API's.
   When it is used up, the exporter logs a warning, sets
   `serverscom_api_budget_exhausted` and waits for the reset.
2. **The account reserve.** When `X-RateLimit-Remaining` drops to
   `--api.ratelimit-reserve` (default 300), the exporter waits until the
   account window resets. A `429` response pauses it as well.

Interrupted loops resume where they stopped; cached data is kept meanwhile.

### Demand estimate

From the number of servers, segments and racks the exporter computes the
auto interval, how many requests per hour each loop needs and how many a full
download costs (a start without a cache file). It exposes the estimate as
metrics and logs it whenever it changes. A warning with flags to change is
logged when the fixed intervals, or the auto collectors even at
`--refresh.auto-max-interval`, do not fit:

```
level=WARN msg="Auto collectors do not fit into the request budget even at the maximum interval; raise the budget or the fixed intervals"
  auto_interval=24h fixed_requests_per_hour=780 budget_per_hour=1000 ... recommended="--api.requests-per-hour=1100"
```

The recommended budget fits the fixed loops and the auto ones at the maximum
interval into 80% of it, covers one full download, and is at most the account
limit minus the reserve. If even that does not fit, the recommended fixed
intervals are raised proportionally.

### Example

100 servers, 10 L2 segments, budget 1000, default intervals:

| Loop | Interval | Requests per run | Requests per hour |
|------|----------|------------------|-------------------|
| hosts | `2m` | 1 | 30 |
| configuration, drives, ports, networks, features, power-feeds | auto: `50m` | 6 × 100 | 720 |
| l2-segments, uplinks, reference | auto: `50m` | ~26 | ~31 |
| **Total** | | full download ≈ 630 | ≈ 780 |

The hosts loop takes 30 requests per hour, leaving 1000 × 80% − 30 = 770 for
auto collectors; their 626 requests per run then take 49 minutes, rounded up
to `50m`. With 200 servers it is about `1h40m`.

With `--refresh.features-interval=15m` features become fixed at 400 requests
per hour, leaving 370 for the rest: the auto interval grows to `1h30m`. With
200 servers features every `15m` alone take 800 requests per hour, all of the
80% share: auto collectors fall back to `--refresh.auto-max-interval` and the
exporter warns and recommends a larger budget or a longer features interval.

Ways to lower the demand, by impact:

- turn off collectors you do not use (`--no-collector.<name>`): every
  per-server collector costs one request per server and refresh;
- raise the fixed intervals, `--refresh.features-interval` first: they are
  paid before auto collectors get anything;
- keep `--cache.file` so restarts do not download everything again.

Useful queries:

```promql
# requests the exporter needs per hour, by loop
serverscom_exporter_requests_per_hour_estimated

# configured vs recommended budget
serverscom_api_requests_budget_per_hour
serverscom_api_requests_per_hour_recommended

# actual request rate per hour, by endpoint and status
sum by (endpoint, code) (rate(serverscom_api_requests_total[15m])) * 3600

# account-wide requests left until the window resets
serverscom_api_ratelimit_remaining
```

## Cache persistence

With `--cache.file=/data/cache.json` the cache is saved every
`--cache.save-interval` (if it changed) and on shutdown, via a temporary file
and rename. On start it is loaded and each loop waits until its data is due.
A missing, unreadable or outdated-format file is ignored and the exporter
starts empty.

The file holds inventory data (hostnames, IPs, labels) but no token. In the
Docker image `/data` is writable by the `nonroot` user; the Helm chart puts it
on a PVC.

## Metrics

All server metrics carry `host_id`, `title`, `location` and `rack`, so
dashboards filter by location and rack without joins. The full list is in
[src/README.md](src/README.md#metrics). Groups:

| Group | Examples |
|-------|----------|
| Servers | `serverscom_host_info`, `_labels`, `_status`, `_operational_status`, `_power_status`, `_powered_on`, `_scheduled_release_timestamp_seconds` |
| Hardware | `serverscom_host_configuration_info`, `_ram_size`, `_drive_slot_info`, `_drive_capacity` |
| Network | `serverscom_host_connection_info`, `_connections`, `_network_info`, `_uplink_info`, `_uplink_redundant`, `_uplink_aggregated` |
| Features and power | `serverscom_host_feature_status`, `_feature_enabled`, `_power_feed_status`, `_power_feed_up` |
| L2 segments | `serverscom_l2_segment_info`, `_member_info`, `_network_info` |
| Reference | `serverscom_location_info`, `serverscom_rack_info` |
| Exporter | `serverscom_exporter_last_refresh_success_timestamp_seconds`, `_refresh_errors_total`, `_build_info`, demand estimate |
| API | `serverscom_api_requests_total`, `_request_duration_seconds`, `_ratelimit_*`, `_budget_*` |

Example queries:

```promql
# servers that are powered off
serverscom_host_powered_on == 0

# servers by location and power status
count by (location, status) (serverscom_host_power_status)

# redundant uplinks that are not aggregated on the switch side
serverscom_host_uplink_redundant == 1 unless on (host_id) serverscom_host_uplink_aggregated == 1

# servers with OOB public access enabled
serverscom_host_feature_enabled{feature="oob_public_access"} == 1

# servers scheduled for release within 7 days
serverscom_host_scheduled_release_timestamp_seconds - time() < 7 * 86400

# age of cached data, by resource
time() - serverscom_exporter_last_refresh_success_timestamp_seconds
```

## Kubernetes

The [Helm chart](charts/serverscom-exporter/README.md) runs the exporter the
way it is meant to run:

- one replica with the `Recreate` strategy, so two pods never share the
  account limit;
- the token mounted from a Secret as a file (`apiToken.existingSecret`, or
  `apiToken.value` to let the chart create it);
- the cache on a PVC (`persistence.*`, kept on `helm uninstall`);
- non-root, read-only root filesystem, no capabilities, no service account
  token;
- optional ServiceMonitor, PrometheusRule with alerts, NetworkPolicy, and
  TLS/basic auth via `webConfig`.

Exporter flags are set under `config` in the values, other flags in
`extraArgs`:

```yaml
apiToken:
  existingSecret: serverscom-api-token
config:
  requestsPerHour: 1200
  hostLabels: env,team,role
  refresh:
    featuresInterval: 30m
    drivesInterval: 24h
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack   # match your Prometheus serviceMonitorSelector
prometheusRule:
  enabled: true
  labels:
    release: kube-prometheus-stack
```

## Grafana dashboard

[`grafana/dashboards/serverscom.json`](grafana/dashboards/serverscom.json) has
tabs for Overview (including a rate limit forecast), Racks (a map of racks
with their servers colored by state), Servers, Power & OOB, Network, Drives and
Exporter, with variables for data source, location, rack and server. Renamed
servers do not show up twice; see [grafana/README.md](grafana/README.md).

It is a Grafana **dashboard v2** resource and needs Grafana 13 or newer. v2
dashboards cannot be provisioned from files or ConfigMaps (the Grafana
sidecar); import it in the UI, through the API or Git Sync. Details in
[grafana/README.md](grafana/README.md).

## Alerts

The Helm chart ships these rules (`prometheusRule.enabled=true`):

| Alert | Severity | Condition |
|-------|----------|-----------|
| `ServerscomExporterDown` | warning | target down for 5m |
| `ServerscomRefreshStale` | warning | server list older than 1h (`prometheusRule.staleThreshold`) |
| `ServerscomRefreshErrors` | warning | more than 3 failed refreshes of a resource in 30m |
| `ServerscomBudgetExhausted` | info | own hourly budget used up for 15m |
| `ServerscomRateLimitLow` | warning | account-wide remaining at or below the reserve for 5m |

Outside Kubernetes, copy the expressions from
[`templates/prometheusrule.yaml`](charts/serverscom-exporter/templates/prometheusrule.yaml)
and replace the `namespace`/`service` selectors with your `job`.

## Troubleshooting

Start with the logs; every refresh, pause and recommendation is logged.
`--log.level=debug` logs every API request with its status and the remaining
budget and account limit.

| Symptom | Cause and fix |
|---------|---------------|
| Exits with `Cannot read API token` | No token, an unreadable token file, or both a token and a token file are set. |
| `serverscom_api_requests_total{code="401"}` grows, refreshes fail | Invalid or revoked token. |
| `/metrics` has only exporter and API metrics right after start | The first refresh is still running. Without a cache file a full download can take one or more budget windows; watch `Host details processed` progress in the logs. |
| `Exporter request budget is used up, waiting for the hourly reset` | Demand exceeds `--api.requests-per-hour`. Follow the `recommended=` flags in the warning or see [sizing](#api-rate-limits-and-sizing). Can happen once on a start without a cache with many servers. |
| `Account rate limit reserve reached, pausing API requests` | Other API users (Terraform, CLI, another exporter) use most of the account limit. Check for a second exporter replica; lower the exporter's demand or the reserve. |
| `serverscom_api_requests_total{code="429"}` | The account limit was hit; the exporter pauses until the reset. |
| Drives or ports change late | Servers are downloaded again when `updated_at` changes, otherwise on their collector's interval. Check `serverscom_exporter_refresh_interval_seconds`; set a fixed `--refresh.<collector>-interval` or raise the budget. |
| Port aggregation / OOB access changes late | Feature changes do not bump `updated_at`; they are picked up on the features interval (see `serverscom_exporter_refresh_interval_seconds{loop="features"}`). Set a fixed `--refresh.features-interval` to bound the delay. |
| `Cannot save cache` | The cache directory is not writable by uid 65532. Fix the volume permissions (the chart sets `fsGroup`). |
| Kubernetes probes fail with basic auth | Basic auth also covers `/-/healthy`; set `probes.type: tcpSocket` in the chart. |

## Development

Requirements: Go (version from [`src/go.mod`](src/go.mod), latest patch
release), Docker with buildx, Helm 3.8+ or 4, and for some checks
[golangci-lint](https://golangci-lint.run/), the
[helm-unittest](https://github.com/helm-unittest/helm-unittest) plugin and
`promtool` from Prometheus.

### Tests

| What | Command | Where |
|------|---------|-------|
| Go unit tests | `go test -race ./...` | `src/` |
| Go linters | `golangci-lint run ./...`, `go vet ./...` | `src/` |
| Known vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | `src/` |
| Image smoke test | `test/smoke.sh <image>` | `src/` |
| Binary smoke test | `EXPORTER_BIN=./serverscom-exporter test/smoke.sh` | `src/` |
| Upgrade test | `PREVIOUS=<previous image or binary>` with either of the above | `src/` |
| Update the golden series list | `UPDATE_GOLDEN=1` with either of the above | `src/` |
| Chart lint | `helm lint --strict charts/serverscom-exporter --set apiToken.value=test` | repo root |
| Chart unit tests | `helm unittest charts/serverscom-exporter` | repo root |
| Alert rule tests | `promtool test rules grafana/alerts/tests/*.test.yml` | repo root |

The **smoke test** starts [`src/test/mockapi`](src/test/mockapi/main.go), a
fake Servers.com API with two servers and one L2 segment, runs the exporter
image (or a local binary) against it with `--api.page-size=1` to exercise
pagination, and checks:

1. `/-/healthy`, the main metrics and the absence of API and refresh errors;
2. **regressions in the exposed series**: every metric name with its labels
   (values dropped, see [`test/metrics-shape.sh`](src/test/metrics-shape.sh))
   must match [`test/golden/metrics-shape.txt`](src/test/golden/metrics-shape.txt).
   An intended change of metrics or labels updates the file with
   `UPDATE_GOLDEN=1`, and the diff shows up in review;
3. after a restart with the API down, the same data is served from the cache
   file;
4. with `--no-collector.drives`, `features` and `l2-segments` and a fixed
   `--refresh.ports-interval`, their metrics and refresh timestamps are gone,
   `uplink_aggregated` is not exposed, the rest is served.

With `PREVIOUS` it first runs the previous release, which fills the cache
file, and then requires the new version to serve exactly the same inventory
(server, L2 and reference series with values) after the upgrade. CI passes
the image of the last stable `v*` tag.

The mock is handy for local development as well:

```sh
cd src
go run ./test/mockapi --listen=:8080 --token=test &
go run ./cmd/serverscom-exporter --api.url=http://localhost:8080/v1 --api.token=test
```

Chart unit tests live in
[`charts/serverscom-exporter/tests/`](charts/serverscom-exporter/tests) and are
not packaged. With Helm 4, install the plugin with
`helm plugin install https://github.com/helm-unittest/helm-unittest --verify=false`.

### Build

```sh
docker build -t prometheus-serverscom-exporter \
  --build-arg VERSION=$(git describe --tags --match 'v*' --always) \
  --build-arg REVISION=$(git rev-parse HEAD) \
  src/
```

Code layout under `src/internal/`:

| Package | Responsibility |
|---------|----------------|
| `api` | HTTP client: pagination, request budget, account rate limit, client metrics |
| `cache` | Snapshot of all data, the store and the cache file |
| `refresher` | Refresh loops, change detection, demand estimate and recommendations |
| `collector` | Renders the snapshot as Prometheus metrics |

Branches: work happens on `development`; releases are made from `main`.

## CI and releases

GitHub Actions ([`.github/workflows`](.github/workflows)):

- **CI** (every push and pull request), jobs:
  - *Go tests*: `go test -race` with a per-package coverage table in the job
    summary and `coverage.out` as an artifact;
  - *Go lint*: `gofmt`, `go vet`, golangci-lint, govulncheck;
  - *Docker image*: linux/amd64 and linux/arm64 build without push, then the
    smoke test of the amd64 image against the mock API;
  - *Helm chart*: version bump check, `helm lint`, helm-unittest, kubeconform,
    `promtool check rules` on the rendered PrometheusRule;
  - *Alert rules*: `promtool check rules` and `promtool test rules` for
    [`grafana/alerts`](grafana/alerts).

  Go jobs use the latest Go 1.27 patch release, so govulncheck does not
  report standard library issues fixed since the version in `go.mod`.
- **Dev builds** of the image and the chart from `development`, see
  [Dev builds](#dev-builds).
- **Releases** are made from tags on commits of `main`; tags elsewhere fail.
  The exporter image and the chart are versioned independently.

| Tag | Releases | Version |
|-----|----------|---------|
| `vX.Y.Z`, `vX.Y.Z-rc.N` | image `ghcr.io/rayderua/prometheus-serverscom-exporter` (`X.Y.Z`, `X.Y`, `latest`; pre-releases only their own tag), linux/amd64 and linux/arm64 | from the tag |
| `chart-X.Y.Z` | chart `oci://ghcr.io/rayderua/charts/serverscom-exporter` and the Helm repository `https://rayderua.github.io/prometheus-serverscom-exporter` | `version` in `Chart.yaml`, must match the tag |

Each tag also creates a GitHub release; only image releases are marked
"Latest". An image is pushed only after it passes the smoke test. For the
Helm repository, the release workflow adds the chart to `index.yaml` on the
`gh-pages` branch; the entry points to the `.tgz` attached to the GitHub
release. GitHub Pages must be enabled for the `gh-pages` branch.

New exporter version:

```sh
git tag v1.4.0 && git push origin v1.4.0       # on main: releases the image
# PR to main: appVersion: "1.4.0" and a new version in Chart.yaml
git tag chart-0.3.1 && git push origin chart-0.3.1
```

Chart-only changes need just the `version` bump and a `chart-*` tag. The chart
release fails if the image of its `appVersion` is not published yet.

### Dev builds

Every push to `development` that passes CI publishes a dev build (workflow
*Dev build*, can also be started by hand on `development`):

| What | Where | Version |
|------|-------|---------|
| Image, linux/amd64 and linux/arm64 | `ghcr.io/rayderua/prometheus-serverscom-exporter` | `X.Y.Z-dev.N`, `dev` (latest dev build), `sha-<commit>` |
| Chart, OCI only | `oci://ghcr.io/rayderua/charts/serverscom-exporter` | `X.Y.Z-dev.N`, `appVersion` `X.Y.Z-dev.N` |

`X.Y.Z` comes from `version` and `appVersion` in `Chart.yaml`, `N` is the
run number; the job summary shows the exact versions. Dev versions are SemVer
pre-releases: `helm install` without `--version` or `--devel` keeps installing
the latest release, and dev builds never reach the Helm repository on
gh-pages, GitHub releases or the `latest` image tag.

```sh
# latest dev build
helm upgrade --install serverscom-exporter \
  oci://ghcr.io/rayderua/charts/serverscom-exporter --devel -f values.yaml
# a specific one
helm upgrade --install serverscom-exporter \
  oci://ghcr.io/rayderua/charts/serverscom-exporter --version 0.2.0-dev.17 -f values.yaml
```

Dev builds accumulate in ghcr; delete old versions in the package settings
when needed.

## Known gaps

Some API details are not yet verified against the live API (full list in
[src/README.md](src/README.md#to-verify-against-the-live-api)):

- the full set of feature `status` values (`feature_enabled` and
  `uplink_aggregated` assume `activated`);
- units of `ram_size` and drive `capacity` (most likely GB);
- whether `updated_at` changes when drives or ports change.
