# serverscom-exporter

Prometheus monitoring for [Servers.com](https://www.servers.com/) Enterprise Bare
Metal: an exporter for the Servers.com API, its Helm chart, and Grafana
dashboards and alerts.

## Layout

| Path       | Contents |
|------------|----------|
| `src/`     | The exporter (Go) and its Dockerfile, see [src/README.md](src/README.md) |
| `charts/`  | Helm chart (planned) |
| `grafana/` | Grafana dashboard (v2, tabs), see [grafana/README.md](grafana/README.md); alert rules planned |

## Quick start

```sh
docker build -t serverscom-exporter src/
docker run --rm -p 9920:9920 -e SERVERSCOM_API_TOKEN=... serverscom-exporter
```
