# Grafana

## Dashboard

[`dashboards/serverscom.json`](dashboards/serverscom.json) is a Grafana
**dashboard v2** resource (`apiVersion: dashboard.grafana.app/v2`) with tabs:

| Tab | Contents |
|---|---|
| Overview | counters of servers, cost, powered off, not operational, power feeds down, split uplinks, public OOB access, scheduled releases, rescue mode; **Needs attention**, one row per problem, critical first; servers by model, rack and label |
| Racks | one panel per rack with a row per server, see below |
| Servers | one row per server: state, uplinks (`LAG` aggregated, `SPL` split, `SGL` single link), public OOB access, cost, then inventory |
| Costs | total of the current billing period (tax included), cost over time, by server model and service type, per server and per service |
| Power & OOB | power feed states, public OOB access |
| Network | a tile per L2 segment with its members among the selected servers, uplinks (`LAG` / `SPL` / `SGL`), L2 segment members, server networks |
| Drives | drive slots: populated or empty, media (HDD, SSD, NVMe), model, capacity |
| Exporter | budget and account rate limit, rate limit forecast, interval stretch, stale data, requests and demand per hour, refresh loops with configured and effective intervals and data age, errors |

Variables:

| Variable | Filters |
|---|---|
| Filters | ad hoc filters for the extra labels the exporter puts on every metric (`--label`, e.g. `account=prod`) |
| Show | `Problems only` hides servers in order on Racks and Servers, and racks without problems |
| Location, Rack | location and rack; the rack list shows the number of servers |
| Label, Value | a Servers.com label (`label_*` of `serverscom_host_labels`) and its values |
| Server | servers that pass the filters above; `All` means all of them. `All` is `.*` and the label filter is part of every query, so queries do not grow with the number of servers |

**Racks** shows one panel per rack, titled with the number of its servers,
with a row per server: problems first, then the rest. Rows are not rack
positions: the API does not report rack units. A panel fits 8 servers; a
rack with more scrolls. The row is colored by state:

| Color | State |
|---|---|
| green | OK |
| orange | degraded: a power feed is down or a redundant uplink is not aggregated |
| red | not operational |
| dark red | powered off |

Clicking a server filters the whole dashboard by it.

**Renamed servers.** Variables come from instant queries, so they list only
current names, not names seen earlier in the time range. The server variable
selects by `host_id` and shows the title, so a selection survives a rename,
and all panels filter by `host_id`.

Requirements:

- Grafana 13 or newer (tabs need the new dashboard layouts, enabled by default
  since Grafana 13);
- Prometheus or VictoriaMetrics as the data source; the queries avoid
  `!~` on labels that may be missing, which VictoriaMetrics treats
  differently;
- exporter 0.2.0 or newer: the dashboard uses `serverscom_host_service_price`,
  `serverscom_exporter_refresh_interval_stretch_factor`,
  `serverscom_exporter_refresh_interval_configured_seconds` and
  `serverscom_api_ratelimit_paused`.

### Importing

Use one of:

- file provisioning: a `dashboards:` provider of `type: file` whose path
  holds the file (Grafana 13 loads v2 resources from files), or a Grafana
  sidecar that writes it there from a ConfigMap;
- UI: *Dashboards → New → Import*, paste or upload the file;
- API: `POST /apis/dashboard.grafana.app/v2/namespaces/default/dashboards`
  with the file as the body (namespace `default` is org 1);
- Git Sync, if it is enabled in your Grafana.

### Rate limit forecast

The Overview tab shows whether the exporter fits into the account-wide API
rate limit until the current window resets (`X-RateLimit-Reset`):

```
headroom = X-RateLimit-Remaining - reserve
         - min(exporter request rate over 15m, budget) * seconds until reset
```

Below zero the exporter will hit its reserve and pause before the window
resets. Requests of other API users of the account are not projected.

## Alerts

[`alerts/`](alerts/) holds the same 21 alert rules in three formats. Use
**one** of them: Prometheus/vmalert or Grafana alerting, not both, or every
alert is sent twice.

| File | For |
|------|-----|
| [`serverscom.rules.yml`](alerts/serverscom.rules.yml) | Prometheus `rule_files`, `vmalert -rule=` |
| [`serverscom.vmrule.yaml`](alerts/serverscom.vmrule.yaml) | VictoriaMetrics operator (victoria-metrics-k8s-stack): `kubectl apply -f` |
| [`grafana-alert-rules.yaml`](alerts/grafana-alert-rules.yaml) | Grafana alerting provisioning (`provisioning/alerting/`) |

The Grafana file reads the data source UID from the environment variable
`SERVERSCOM_ALERTS_DATASOURCE_UID` of the Grafana process. Grafana expands
environment variables in provisioning files, so a literal `$` is written as
`$$` there.

Host alerts are skipped for servers with the Servers.com label
`monitoring=off` (e.g. servers that are powered off on purpose). The exporter
must expose that label, which it does with the default
`--collector.host-labels=*`.

| Alert | Severity | For | Summary |
|---|---|---|---|
| `ServerscomHostPoweredOff` | critical | 10m | Server <title> is powered off |
| `ServerscomHostNotOperational` | critical | 15m | Server <title> is <status> |
| `ServerscomHostNotActive` | info | 1h | Server <title> is <status> |
| `ServerscomHostReleaseScheduled` | warning | — | Server <title> is scheduled to be released |
| `ServerscomHostRemoved` | warning | — | Server <title> disappeared from the account |
| `ServerscomHostDrivesMissing` | warning | — | Server <title> has fewer drives than a day ago |
| `ServerscomPowerFeedDown` | warning | 5m | Power feed <feed> of <title> is down |
| `ServerscomUplinkDisaggregated` | warning | 15m | <uplink> uplink of <title> is not aggregated |
| `ServerscomFeatureStuck` | warning | 30m | Feature <feature> of <title> is stuck in <status> |
| `ServerscomOOBPublicAccessEnabled` | warning | — | Public OOB access is enabled on <title> |
| `ServerscomL2SegmentNotActive` | warning | 15m | L2 segment <segment> is <status> |
| `ServerscomL2MemberNotActive` | warning | 15m | <title> is <status> in L2 segment <segment> |
| `ServerscomExporterAbsent` | critical | 10m | Servers.com exporter metrics are missing |
| `ServerscomExporterDataStale` | warning | 10m | Servers.com <loop> data is stale |
| `ServerscomExporterRefreshErrors` | warning | 30m | Servers.com exporter fails to refresh <resource> |
| `ServerscomAPIAuthFailed` | critical | — | Servers.com API rejects the exporter token |
| `ServerscomAPIServerErrors` | warning | 15m | Servers.com API requests are failing |
| `ServerscomExporterBudgetExhausted` | warning | — | Servers.com exporter used up its hourly request budget |
| `ServerscomExporterDemandExceedsBudget` | warning | 30m | Servers.com exporter refresh intervals are stretched to fit its budget |
| `ServerscomExporterIntervalsTooShort` | warning | 30m | Servers.com exporter refresh intervals are too short for the account limit |
| `ServerscomAPIRateLimitLow` | warning | 5m | Servers.com account is close to its API rate limit |

Alerts built on the metrics of a disabled collector (`--no-collector.<name>`)
never fire, because the metrics are not exposed:

| Collector | Silent alerts |
|---|---|
| `drives` | `ServerscomHostDrivesMissing` |
| `features` | `ServerscomUplinkDisaggregated` (it needs `serverscom_host_uplink_aggregated`), `ServerscomFeatureStuck`, `ServerscomOOBPublicAccessEnabled` |
| `uplinks` | `ServerscomUplinkDisaggregated` |
| `power-feeds` | `ServerscomPowerFeedDown` |
| `l2-segments` | `ServerscomL2SegmentNotActive`, `ServerscomL2MemberNotActive` |

How fast the host alerts fire also depends on the collector's refresh
interval, which is stretched when the configured intervals do not fit into
the request budget (`ServerscomExporterDemandExceedsBudget`). To catch a split
port aggregation within a known time, keep `--refresh.features-interval`
short and the budget large enough for it.

`ServerscomFeatureStuck` treats statuses ending in `ation` (`activation`,
`deactivation`) as transitional; only `activation` is confirmed (by the official
Go client fixtures), the rest is assumed.

Unit tests:

```sh
promtool test rules grafana/alerts/tests/serverscom.rules.test.yml
```
