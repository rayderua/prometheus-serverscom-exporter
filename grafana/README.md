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
