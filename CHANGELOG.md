# Changelog

The exporter image (`vX.Y.Z` tags) and the Helm chart (`chart-X.Y.Z` tags)
are versioned independently; see [CI and releases](README.md#ci-and-releases).

## 0.2.0 (unreleased): image v0.2.0, chart 0.2.0

### Breaking changes

- **Refresh flags.** `--refresh.details-interval` and `--refresh.l2-interval`
  are removed. Every collector has its own `--refresh.<collector>-interval`
  instead (see below); `--refresh.l2-interval` becomes
  `--refresh.l2-segments-interval`. The exporter does not start with the old
  flags.
- **Helm values.** `config.refresh.detailsInterval` and
  `config.refresh.l2Interval` are removed; use
  `config.refresh.<collector>Interval`, e.g. `drivesInterval` or
  `l2SegmentsInterval`.
- **Resource and loop labels.** `serverscom_exporter_last_refresh_success_timestamp_seconds{resource}`,
  `serverscom_exporter_refresh_errors_total{resource}` and the
  `loop` label of the demand metrics now use collector names, and both labels
  match, so `ServerscomExporterDataStale` covers every collector:
  - `host_features` → `features`, `l2_segments` → `l2-segments`,
    `uplink_models` → `uplinks`;
  - `locations` and `racks` → `reference`;
  - `host_details` now means the download of new and changed servers only;
  - new: `configuration`, `drives`, `ports`, `networks`, `power-feeds`,
    `services`.
- **`serverscom_host_details_last_refresh_timestamp_seconds`** has a new
  `collector` label: one series per server and per-server collector.
- **Default intervals.** `features` and `power-feeds` every `15m` (was `1h`
  for features), `l2-segments` `1h`, `reference` `6h`, everything else
  `24h`. Intervals must be positive; disable a collector with
  `--no-collector.<name>`.
- **JSON logs by default.** `--log.format` defaults to `json`; use
  `--log.format=logfmt` for the previous format.
- **`serverscom_exporter_refresh_interval_recommended_seconds`** is removed;
  see `serverscom_exporter_refresh_interval_stretch_factor` and
  `_configured_seconds` below. Alerts `ServerscomExporterDemandExceedsBudget`
  and `ServerscomExporterIntervalsTooShort` are rewritten accordingly.
- **The cache file must be writable.** With `--cache.file` the exporter does
  not start if it cannot create files next to it.
- **`--api.page-size`** must be between 1 and 100, the API maximum.
- **Cache file format 2.** A cache file of 0.1.0 is ignored: the first start
  downloads everything once (a full download costs about 7 requests per
  server).

### Added

- **Prices**: the `services` collector fetches the billed services of every
  server (`GET /hosts/dedicated_servers/{id}/services`) and exposes
  `serverscom_host_service_price` (tax included), `_service_subtotal` and
  `_service_tax` by `service`, `type` and `currency`, for the current billing
  period. Default interval `24h`: the API updates prices once a day.
- **Interval stretching**: when the configured intervals need more than 80%
  of `--api.requests-per-hour`, every loop but the server list is stretched
  by the same factor (the list too if it alone does not fit), and the
  intervals go back when the budget allows.
  `serverscom_exporter_refresh_interval_stretch_factor` (1 when they fit) and
  `serverscom_exporter_refresh_interval_configured_seconds{loop}` show it.
- **`--estimate`** prints the intervals, the request demand and the
  recommended budget for the given flags and exits, as JSON or, with
  `--log.format=logfmt`, as a table. Sizes come from the API (a few
  requests), the cache file or `--estimate.hosts`, `--estimate.l2-segments`,
  `--estimate.uplink-catalogues`.
- **Budget window follows the account window**: the exporter's budget ends at
  `X-RateLimit-Reset` and starts over when the account limit resets.
- **Rate limit state in the cache file**: a restart while the account is at
  its reserve waits for the reset instead of sending a request first, and
  the requests already spent in the budget window still count.
  `serverscom_api_ratelimit_paused` is 1 while the exporter waits for it.
- **Budget priority**: when 20% or less of the budget window is left, the
  refresh loops pause until the reset, while the server list and new or
  changed servers keep going.
- **Binaries**: releases attach static binaries for linux and darwin, amd64
  and arm64 (`serverscom-exporter-X.Y.Z.<os>-<arch>.tar.gz`,
  `sha256sums.txt`); dev builds keep them as workflow artifacts. A
  `Makefile` builds the binary, the archives, the image and the chart
  locally. CI builds all platforms and smoke-tests the linux/amd64 binary.
- **Extra labels**: `--label=<name>=<value>` (repeatable; Helm:
  `config.extraLabels`) adds labels to every metric, e.g. the account.
  Names that clash with the exporter's own labels are rejected at start.
- **Optional collectors**: `--[no-]collector.<name>` for `drives`, `ports`,
  `networks`, `features`, `power-feeds`, `services`, `uplinks` and
  `l2-segments` (Helm: `config.collectors.<name>: false`). A disabled
  collector is neither fetched nor exposed, and its data is dropped from the
  cache file on start. Without `features`,
  `serverscom_host_uplink_aggregated` is not exposed.
- **Per-collector refresh intervals**: `--refresh.<collector>-interval` for
  `configuration`, `drives`, `ports`, `networks`, `features`, `power-feeds`,
  `services`, `uplinks`, `l2-segments` and `reference`.
- Dashboard: **Overview** with problem counters (including public OOB access,
  scheduled releases and rescue mode) and a **Needs attention** list;
  **Racks**, a panel per rack titled with its number of servers, problems
  first; **Costs**; the **Servers** table with state, short uplink and OOB
  columns and cost; filters for a Servers.com label, the `--label` labels
  (ad hoc) and **Show: Problems only**; the **Exporter** tab shows the
  budget, the interval stretch and configured vs effective intervals.
- Mock API: `POST /-/rename?id=<id>&title=<title>` renames a server at
  runtime; `--servers`, `--racks`, `--rack-growth` (rack N holds K×N servers
  with a fixed pattern of problems), and a real rate limit window with `429`
  (`--ratelimit`, `--ratelimit-window`).

### Fixed

- Dashboard: renamed servers (and renamed racks or locations) no longer show
  up twice in the variables. Variables use instant queries instead of
  `label_values` over the time range; the server variable selects by
  `host_id` and shows the title, and panels filter by `host_id`. Links with
  `var-server=<title>` from 0.1.0 no longer match.
- Alert `ServerscomHostDrivesMissing` fired for a day after a server was
  renamed or moved to another rack ("0 drives"). Drives are now counted per
  `host_id`; title, location and rack come from the current server list.
- After a cold start, collectors fetched with the new servers count as
  refreshed, so their data age and `ServerscomExporterDataStale` do not wait
  for their own first pass.
- A server released while a collector pass runs (404) no longer gets
  requested again and again until the next server list refresh.

### Changed

- `serverscom_exporter_refresh_interval_seconds` reports the effective
  interval, including stretching; `serverscom_exporter_requests_per_hour_estimated`
  is the demand at the effective intervals.
- A pass of a per-server collector fetches every due server right away, one
  request after another, instead of one server every `interval / servers`.
- Recommendations (`serverscom_api_requests_per_hour_recommended`, the
  `recommended=` log field) fit the configured intervals into 80% of the
  budget and cover one full download.
- After a `429` without rate limit headers the exporter assumes the
  documented account limit of 2000.
- A collector that keeps failing for a server (e.g. 403) keeps its cached
  data and is retried on its next turn in the queue, not in a loop.

### Tests

- The smoke test compares every exposed series (names and labels) with
  `src/test/golden/metrics-shape.txt`, checks disabled collectors and fixed
  intervals, and, with `PREVIOUS`, upgrades from the previous release and
  requires the same inventory. CI runs the upgrade from the last stable
  release image.
- Unit tests for interval stretching, the budget window following the
  account window, the rate limit state in the cache file, `--estimate` and
  service prices.

### Upgrade

1. Replace `--refresh.details-interval` and `--refresh.l2-interval` (Helm:
   `detailsInterval`, `l2Interval`) with per-collector intervals, or drop
   them to use the defaults.
2. Update queries and alerts that filter on the old `resource` or `loop`
   values listed above, and log pipelines that parse logfmt (or set
   `--log.format=logfmt`).
3. Check the new defaults against your budget with `--estimate`, and expect
   one full download on the first start
   (`serverscom_exporter_full_refresh_requests_estimated`).
4. Make sure the cache volume is writable by uid 65532.

## 0.1.0: image v0.1.0, chart 0.1.0

First release.
