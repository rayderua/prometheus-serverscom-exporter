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
  - new: `configuration`, `drives`, `ports`, `networks`, `power-feeds`.
- **`serverscom_host_details_last_refresh_timestamp_seconds`** has a new
  `collector` label: one series per server and per-server collector.
- **`--refresh.features-interval`** defaults to `auto` instead of `1h` and no
  longer accepts `0`; disable features with `--no-collector.features`.
- **`--api.page-size`** must be between 1 and 100, the API maximum.
- **Cache file format 2.** A cache file of 0.1.0 is ignored: the first start
  downloads everything once (a full download costs about 6 requests per
  server).

### Added

- **Extra labels**: `--label=<name>=<value>` (repeatable; Helm:
  `config.extraLabels`) adds labels to every metric, e.g. the account.
  Names that clash with the exporter's own labels are rejected at start.

- **Optional collectors**: `--[no-]collector.<name>` for `drives`, `ports`,
  `networks`, `features`, `power-feeds`, `uplinks` and `l2-segments`
  (Helm: `config.collectors.<name>: false`). A disabled collector is neither
  fetched nor exposed, and its data is dropped from the cache file on start.
  Without `features`, `serverscom_host_uplink_aggregated` is not exposed.
- **Per-collector refresh intervals**: `--refresh.<collector>-interval` for
  `configuration`, `drives`, `ports`, `networks`, `features`, `power-feeds`,
  `uplinks`, `l2-segments` and `reference`: a fixed duration or `auto`
  (default).
- **Auto intervals**: auto collectors share one interval, computed from the
  budget left after the fixed ones:
  `auto requests per run / (budget × 80% − fixed requests per hour)`, rounded
  up to 5 minutes, within `--refresh.auto-min-interval` (`5m`) and
  `--refresh.auto-max-interval` (`24h`). It is recomputed when the number of
  servers or segments changes.
- **Budget priority**: when 20% or less of the budget window is left, auto
  collectors pause until the reset, while fixed collectors and new or changed
  servers keep going.
- **Even spreading**: per-server collectors refresh the server with the
  oldest data first, one server every `interval / servers`, instead of in
  bursts.
- `serverscom_exporter_refresh_interval_auto{loop}`: whether a loop's
  interval is auto.
- Dashboard: the *Refresh intervals* table shows whether each interval is
  auto or fixed.

### Changed

- `serverscom_exporter_refresh_interval_seconds` reports the effective
  interval, including the computed auto interval.
- Recommendations (`serverscom_api_requests_per_hour_recommended`, the
  `recommended=` log field) fit the fixed loops and the auto ones at the
  maximum interval into 80% of the budget, and only recommend changing fixed
  intervals.
- A collector that keeps failing for a server (e.g. 403) keeps its cached
  data and is retried on its next turn in the queue, not in a loop.

### Tests

- The smoke test compares every exposed series (names and labels) with
  `src/test/golden/metrics-shape.txt`, checks disabled collectors and fixed
  intervals, and, with `PREVIOUS`, upgrades from the previous release and
  requires the same inventory. CI runs the upgrade from the last stable
  release image.

### Upgrade

1. Replace `--refresh.details-interval` and `--refresh.l2-interval` (Helm:
   `detailsInterval`, `l2Interval`) with per-collector intervals, or drop
   them to use `auto`.
2. Update queries and alerts that filter on the old `resource` or `loop`
   values listed above.
3. Expect one full download on the first start; check that it fits into
   `--api.requests-per-hour` (`serverscom_exporter_full_refresh_requests_estimated`).

## 0.1.0: image v0.1.0, chart 0.1.0

First release.
