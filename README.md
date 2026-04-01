# Network Reliability Monitor

A lightweight, single-binary network reliability monitoring tool that probes TCP, DNS, HTTP, and ICMP targets and exposes results as Prometheus metrics. Designed to run as a Docker container alongside a Prometheus + Grafana stack.

## Features

- **TCP** — connection time, success/failure rate, error classification (timeout / refused / other)
- **DNS** — resolution time, response size, failure rate by error type (timeout / refused / nxdomain / servfail)
- **HTTP** — full request waterfall (DNS lookup → TCP connect → TLS handshake → TTFB → total), response size, 4xx/5xx error rate
- **ICMP / Ping** — round-trip time (p50/p95) and packet loss ratio per host
- **Probe label** — every metric carries a `probe` label so multiple instances can be compared in one dashboard
- **YAML config** with `${ENV_VAR}` substitution and strict validation; legacy `.txt` format still accepted
- **Hot reload** — send `SIGHUP` to reload target files without restarting the process
- **Graceful shutdown** — drains in-flight probes on `SIGTERM`/`SIGINT` with a 10-second timeout
- **Structured logging** — JSON by default (`--log-format=text` for local development)
- **Auto-provisioned Grafana dashboard** — probe selector, health summary, latency panels with threshold lines

## Requirements

- Go 1.25+
- Docker + Docker Compose (for the full stack)
- `CAP_NET_RAW` capability for ICMP probing (granted automatically via `docker-compose.yml`)

## Quick Start

```bash
git clone https://github.com/haciz/networkReliability.git
cd networkReliability
docker compose up --build
```

- Metrics: http://localhost:2112/metrics
- Prometheus: http://localhost:9090
- Grafana: http://localhost:3000 (admin / admin)

## Building from Source

```bash
GO111MODULE=on go build -o bin/monitor cmd/monitor/main.go
```

## Configuration

Targets are defined in YAML files mounted at runtime via the Docker volume `./config:/app/config`.

### DNS (`config/dns_targets.yaml`)

```yaml
targets:
  - domain: example.com
    record_type: A
    resolver: 8.8.8.8
  - domain: example.com
    record_type: MX
    resolver: 1.1.1.1
```

### TCP (`config/tcp_targets.yaml`)

```yaml
targets:
  - host: example.com
    port: 443
  - host: db.internal
    port: 5432
```

### HTTP (`config/http_targets.yaml`)

```yaml
targets:
  - url: https://example.com
    method: GET
  - url: https://api.example.com/health
    method: GET
```

Environment variables are expanded inline:

```yaml
targets:
  - url: https://api.example.com/health
    method: GET
  - url: ${INTERNAL_API_URL}
    method: POST
```

### ICMP (`config/icmp_targets.yaml`)

```yaml
targets:
  - host: 8.8.8.8
    count: 5       # pings per cycle; defaults to 5 if omitted
  - host: example.com
    count: 5
```

### Reloading targets at runtime

```bash
# Docker
docker kill --signal=HUP network-monitor

# Direct process
kill -HUP <pid>
```

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--probe-name` | `default` | Label applied to every metric — use to distinguish probe locations |
| `--dns-targets` | `config/dns_targets.yaml` | DNS targets file |
| `--tcp-targets` | `config/tcp_targets.yaml` | TCP targets file |
| `--http-targets` | `config/http_targets.yaml` | HTTP targets file |
| `--icmp-targets` | `config/icmp_targets.yaml` | ICMP targets file |
| `--interval` | `10s` | Probe interval |
| `--prom-port` | `2112` | Prometheus metrics port |
| `--log-format` | `json` | Log format: `json` or `text` |

## Metrics Reference

All metrics are prefixed with `monitoring_` and carry a `probe` label.

### TCP

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_tcp_connect_time_seconds` | histogram | probe, host, port | Time to establish a TCP connection |
| `monitoring_tcp_connection_success_total` | counter | probe, host, port | Successful connections |
| `monitoring_tcp_connection_failure_total` | counter | probe, host, port, error_type | Failed connections; `error_type`: timeout / refused / other |

### DNS

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_dns_resolution_time_seconds` | histogram | probe, domain, record_type, resolver | Resolution latency |
| `monitoring_dns_resolution_success_total` | counter | probe, domain, record_type, resolver | Successful resolutions |
| `monitoring_dns_resolution_failure_total` | counter | probe, domain, record_type, resolver, error_type | Failed resolutions; `error_type`: timeout / refused / nxdomain / servfail / other |
| `monitoring_dns_response_size_bytes` | histogram | probe, domain, record_type, resolver | DNS response size |

### HTTP

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_http_request_duration_seconds` | histogram | probe, url, method, status_code | Total request duration |
| `monitoring_http_dns_lookup_duration_seconds` | histogram | probe, url | DNS lookup phase |
| `monitoring_http_connect_duration_seconds` | histogram | probe, url | TCP connect phase |
| `monitoring_http_tls_handshake_duration_seconds` | histogram | probe, url | TLS handshake phase (HTTPS only) |
| `monitoring_http_first_byte_duration_seconds` | histogram | probe, url | Time to first response byte |
| `monitoring_http_response_size_bytes` | histogram | probe, url, method, status_code | Response body size |
| `monitoring_http_request_errors_total` | counter | probe, url, method, error_type | Transport-level errors; `error_type`: timeout / refused / other |
| `monitoring_http_response_errors_total` | counter | probe, url, method, status_code | HTTP 4xx / 5xx responses |

### ICMP

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_icmp_rtt_seconds` | histogram | probe, host | Round-trip time per successful ping |
| `monitoring_icmp_packet_loss_ratio` | gauge | probe, host | Packet loss ratio per cycle (0.0 = no loss, 1.0 = total loss) |

## Grafana Dashboard

The dashboard is auto-provisioned from `grafana/dashboard.json` — no manual import needed.

Panels:
- **Health Summary** — green/red stat panels for TCP failures, DNS failures, HTTP transport errors, HTTP 4xx/5xx rate
- **TCP** — connect time p50/p95 with threshold lines (100ms warn / 500ms crit), failure rate by error type
- **DNS** — resolution time p50/p95 with threshold lines (50ms warn / 200ms crit), failure rate by error type
- **HTTP** — request duration p95, TTFB p95, waterfall breakdown (DNS / TCP / TLS / TTFB phases), response errors, response body size
- **ICMP** — RTT p50/p95, packet loss ratio with threshold lines

The `probe` dropdown at the top filters all panels to one or more probe instances.

## Docker Compose

```yaml
services:
  monitor:   # custom image, port 2112
  prometheus: # prom/prometheus:latest, port 9090
  grafana:    # grafana/grafana:latest, port 3000
```

The monitor service runs with `cap_add: NET_RAW` (required for ICMP raw sockets) and `no-new-privileges:true`.

To rebuild after a code change:

```bash
docker compose up --build -d
```

To reload targets without restarting:

```bash
docker kill --signal=HUP network-monitor
```
