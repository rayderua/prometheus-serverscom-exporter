# serverscom-exporter Helm chart

Deploys [serverscom-exporter](../../src/README.md), a Prometheus exporter for
Servers.com Enterprise Bare Metal.

The chart always runs **one replica** with the `Recreate` strategy: every
replica would spend the same account-wide API limit (2000 requests/hour).

## Install

```sh
kubectl -n monitoring create secret generic serverscom-api-token \
  --from-literal=token=$SERVERSCOM_API_TOKEN

helm install serverscom-exporter oci://ghcr.io/rayderua/charts/serverscom-exporter \
  --version <version> -n monitoring \
  --set apiToken.existingSecret=serverscom-api-token \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

Or from the Helm repository on GitHub Pages:

```sh
helm repo add serverscom https://rayderua.github.io/prometheus-serverscom-exporter
helm repo update
helm install serverscom-exporter serverscom/serverscom-exporter -n monitoring \
  --set apiToken.existingSecret=serverscom-api-token
```

From a checkout, use `charts/serverscom-exporter` instead of the OCI reference.
The chart is versioned independently of the exporter. The image tag defaults
to the chart's `appVersion`
(`ghcr.io/rayderua/prometheus-serverscom-exporter:<appVersion>`); see
`helm show chart` for it.

Any change to the chart needs a new `version` in `Chart.yaml` (CI checks it);
it is released with the tag `chart-<version>` on `main`.

`apiToken.value` creates the Secret from the chart instead, but puts the token
into the Helm release.

## What it creates

| Resource | When |
|----------|------|
| Deployment, Service, ServiceAccount | always |
| Secret with the API token | `apiToken.existingSecret` is empty |
| PersistentVolumeClaim for the cache | `persistence.enabled` and no `persistence.existingClaim` |
| Secret with `web-config.yml` | `webConfig.enabled` and no `webConfig.existingSecret` |
| ServiceMonitor | `serviceMonitor.enabled` |
| PrometheusRule | `prometheusRule.enabled` |
| NetworkPolicy | `networkPolicy.enabled` |

The token is mounted as a file (`--api.token-file`), the cache is written to
`/data/cache.json` (`--cache.file`). With persistence the exporter serves full
metrics right after a restart and only refetches what is due. The PVC has
`helm.sh/resource-policy: keep` and survives `helm uninstall`.

## Configuration

Exporter flags are set under `config`; empty values keep the exporter
defaults. Other flags go to `extraArgs`.

| Value | Flag |
|-------|------|
| `config.requestsPerHour` | `--api.requests-per-hour` |
| `config.ratelimitReserve` | `--api.ratelimit-reserve` |
| `config.refresh.hostsInterval` | `--refresh.hosts-interval` |
| `config.refresh.<collector>Interval` | `--refresh.<collector>-interval`: a duration or `auto` (configuration, drives, ports, networks, features, powerFeeds, uplinks, l2Segments, reference) |
| `config.refresh.autoMinInterval`, `autoMaxInterval` | `--refresh.auto-min-interval`, `--refresh.auto-max-interval` |
| `config.refresh.referenceInterval` | `--refresh.reference-interval` |
| `config.hostLabels` | `--collector.host-labels` |
| `config.extraLabels` | `--label=<name>=<value>` for each entry |
| `config.collectors.<name>` | `--no-collector.<name>` when `false` (drives, ports, networks, features, power-feeds, uplinks, l2-segments) |
| `config.apiUrl`, `apiTimeout`, `apiPageSize` | `--api.url`, `--api.timeout`, `--api.page-size` |
| `config.cacheSaveInterval` | `--cache.save-interval` |
| `config.metricsPath` | `--web.telemetry-path` |
| `config.logLevel`, `logFormat` | `--log.level`, `--log.format` |

The exporter logs whether its request budget fits the number of servers and
which flags to change if not (see the
[exporter README](../../src/README.md#demand-estimate-and-recommendations)).

### TLS and basic auth

```yaml
webConfig:
  enabled: true
  config:
    basic_auth_users:
      prometheus: $2y$10$...   # bcrypt hash
    # tls_server_config:
    #   cert_file: /etc/serverscom-exporter/tls/tls.crt
    #   key_file: /etc/serverscom-exporter/tls/tls.key
  # tlsSecrets:
  #   - name: serverscom-exporter-tls
  #     mountPath: /etc/serverscom-exporter/tls
probes:
  type: tcpSocket   # basic auth also protects /-/healthy
  # scheme: HTTPS   # with httpGet probes and TLS only
serviceMonitor:
  basicAuth:
    username: {name: scrape-auth, key: username}
    password: {name: scrape-auth, key: password}
```

Basic auth also protects `/-/healthy`, so use `probes.type: tcpSocket` with it.
With TLS only, keep `httpGet` probes and set `probes.scheme: HTTPS`.

### Alerts

`prometheusRule.enabled` adds:

| Alert | Condition |
|-------|-----------|
| `ServerscomExporterDown` | target down for 5m |
| `ServerscomRefreshStale` | server list older than `prometheusRule.staleThreshold` (1h) |
| `ServerscomRefreshErrors` | more than 3 failed refreshes of a resource in 30m |
| `ServerscomBudgetExhausted` | own hourly budget used up for 15m (severity `info`) |
| `ServerscomRateLimitLow` | account-wide remaining at or below the reserve for 5m |

The expressions select the target by the `namespace` and `service` labels that
the ServiceMonitor adds. Disable single alerts with
`prometheusRule.alerts.<name>: false`, add your own with `prometheusRule.rules`.

## Tests

```sh
helm lint --strict charts/serverscom-exporter --set apiToken.value=test
helm unittest charts/serverscom-exporter
```

Unit tests are in [`tests/`](tests) (excluded from the package). CI also
validates the rendered manifests with kubeconform and the alert expressions
with `promtool check rules`.

## Grafana dashboard

The dashboard in [`grafana/`](../../grafana/README.md) is a v2 resource and
cannot be provisioned with ConfigMaps / the Grafana sidecar, so the chart does
not ship it. Import it as described there.
