# Grafana

## Dashboard

[`dashboards/serverscom.json`](dashboards/serverscom.json) is a Grafana
**dashboard v2** resource (`apiVersion: dashboard.grafana.app/v2`) with tabs:
Overview, Servers, Power & OOB, Network, Drives and Exporter. Variables:
data source, location, rack and server.

Requirements:

- Grafana 13 or newer (tabs need the new dashboard layouts, enabled by default
  since Grafana 13);
- an exporter version that exposes `serverscom_api_ratelimit_reserve` and
  `serverscom_api_requests_budget_per_hour` (used by the rate limit forecast).

### Importing

v2 dashboards are **not** loaded by file provisioning (provisioning
`dashboards:` providers, Grafana sidecars reading ConfigMaps): those read the
classic dashboard JSON only. Use one of:

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
| `ServerscomExporterDemandExceedsBudget` | warning | 30m | Servers.com exporter configuration needs more requests than its budget |
| `ServerscomExporterIntervalsTooShort` | warning | 30m | Servers.com <loop> refresh interval is too short for the account limit |
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
interval: with `auto` it follows the request budget. To catch a split port
aggregation within a known time, set a fixed `--refresh.features-interval`.

`ServerscomFeatureStuck` treats statuses ending in `ation` (`activation`,
`deactivation`) as transitional; only `activation` is confirmed (by the official
Go client fixtures), the rest is assumed.

Unit tests:

```sh
promtool test rules grafana/alerts/tests/serverscom.rules.test.yml
```
