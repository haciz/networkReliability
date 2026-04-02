# Network Reliability Monitor

A lightweight, single-binary network reliability monitoring tool that probes TCP, DNS, HTTP, ICMP, synthetic multi-step flows, and traceroute targets. Results are exposed as Prometheus metrics and visualised in an auto-provisioned Grafana dashboard.

## Features

- **TCP** — connection time (3-way handshake), success/failure rate, error classification (timeout / refused / other)
- **DNS** — resolution time, response size, failure rate; **multi-resolver comparison** emits a disagreement metric when answers differ across resolvers (split-brain detection)
- **HTTP** — full request waterfall (DNS lookup → TCP connect → TLS handshake → TTFB → total), response size, 4xx/5xx error rate; optional **auth headers** (`Authorization`, `X-API-Key`, etc.)
- **ICMP / Ping** — round-trip time (p50/p95) and packet loss ratio per host
- **Synthetic flows** — ordered multi-step HTTP sequences; values extracted from one step (e.g. a login token) are injected into subsequent steps via `${VAR}` substitution
- **Traceroute** — periodic path trace with per-hop RTT and total hop count; exposes IP info as a Prometheus info metric for Grafana annotations
- **Probe label** — every metric carries a `probe` label so multiple instances can be compared in one dashboard
- **YAML config** with `${ENV_VAR}` substitution and strict validation; legacy `.txt` format still accepted
- **Hot reload** — send `SIGHUP` to reload all target files without restarting the process
- **Graceful shutdown** — drains in-flight probes on `SIGTERM`/`SIGINT` with a 10-second timeout
- **Structured logging** — JSON by default (`--log-format=text` for local development)
- **Auto-provisioned Grafana dashboard** — probe selector, health summary, SLO/availability panels, per-monitor latency panels

## Requirements

- Go 1.25+
- Docker + Docker Compose (for the full stack)
- `CAP_NET_RAW` capability for ICMP and traceroute raw sockets (granted automatically via `docker-compose.yml`)

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

Targets are defined in YAML files mounted at runtime via the Docker volume `./config:/app/config`. All values support `${ENV_VAR}` substitution.

### DNS (`config/dns_targets.yaml`)

```yaml
targets:
  # Single-resolver probe
  - domain: example.com
    record_type: A
    resolver: 8.8.8.8

  # Multi-resolver comparison — emits monitoring_dns_resolver_disagreement=1
  # if answers differ (split-brain DNS, propagation issues, etc.)
  - domain: example.com
    record_type: A
    resolvers:
      - 8.8.8.8
      - 1.1.1.1
      - 9.9.9.9
```

`resolver` and `resolvers` are mutually exclusive. `resolvers` requires at least 2 entries.

### TCP (`config/tcp_targets.yaml`)

```yaml
targets:
  - host: example.com
    port: 443
  - host: db.internal
    port: 5432
```

`connect_time_seconds` measures the time for the TCP 3-way handshake to complete. If a hostname is used (rather than a bare IP), DNS resolution time is included.

### HTTP (`config/http_targets.yaml`)

```yaml
targets:
  - url: https://example.com
    method: GET

  # Optional auth headers — use env vars for secrets
  - url: https://api.example.com/health
    method: GET
    headers:
      Authorization: "Bearer ${API_TOKEN}"
      X-Probe-Source: "network-monitor"
```

Supported methods: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`.

### ICMP (`config/icmp_targets.yaml`)

```yaml
targets:
  - host: 8.8.8.8
    count: 5       # pings per cycle; defaults to 5 if omitted
  - host: example.com
    count: 5
```

### Synthetic Flows (`config/synthetic_flows.yaml`)

Synthetic flows execute an ordered sequence of HTTP steps. Values extracted from one step can be used in subsequent steps.

```yaml
flows:
  - name: api-auth-flow
    steps:
      - name: login
        url: "https://api.example.com/auth/token"
        method: POST
        body: '{"username": "${TEST_USER}", "password": "${TEST_PASS}"}'
        body_type: json        # sets Content-Type: application/json
        expected_status: 200
        extract:
          - name: token        # store extracted value as ${token}
            json_key: access_token   # dot-notation: "data.token" also works

      - name: get-profile
        url: "https://api.example.com/me"
        method: GET
        headers:
          Authorization: "Bearer ${token}"   # uses value extracted above
        expected_status: 200
        body_contains: "email"               # optional substring assertion
```

**Variable resolution order:** extracted values from previous steps take precedence over environment variables with the same name.

**Supported `extract.json_key` paths:** dot-notation for nested objects, e.g. `data.user.id`.

**Failure reasons** (appear in `monitoring_synthetic_flow_failure_total` `reason` label):

| Reason | Meaning |
|---|---|
| `transport` | TCP / TLS / HTTP error |
| `wrong_status` | Response status didn't match `expected_status` |
| `body_mismatch` | `body_contains` string not found in response |
| `extract_failed` | JSON key not present or body not valid JSON |

### Traceroute (`config/traceroute_targets.yaml`)

```yaml
targets:
  - host: 8.8.8.8
    max_hops: 30       # default 30, max 64
    probes_per_hop: 3  # ICMP probes per TTL level, default 3, max 10
```

Traceroute uses ICMP echo with incrementing TTL values, same as the `traceroute` / `mtr` CLI tools. It requires `CAP_NET_RAW`. Results are updated on every probe interval.

**Grafana usage:** combine `monitoring_traceroute_hop_rtt_seconds` with `monitoring_traceroute_hop_ip_info` using `group_left(hop_ip)` to see both the RTT trend and the IP address for each hop:

```promql
monitoring_traceroute_hop_rtt_seconds * 1000
  * on(probe, target, hop) group_left(hop_ip)
  monitoring_traceroute_hop_ip_info
```

### Reloading targets at runtime

```bash
# Docker
docker kill --signal=HUP network-monitor

# Direct process
kill -HUP <pid>
```

All seven monitors reload atomically. In-flight probes complete before the new target list takes effect.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--probe-name` | `default` | Label on every metric — use to distinguish probe locations |
| `--dns-targets` | `config/dns_targets.yaml` | DNS targets file |
| `--tcp-targets` | `config/tcp_targets.yaml` | TCP targets file |
| `--http-targets` | `config/http_targets.yaml` | HTTP targets file |
| `--icmp-targets` | `config/icmp_targets.yaml` | ICMP targets file |
| `--synthetic-flows` | `config/synthetic_flows.yaml` | Synthetic flow definitions |
| `--traceroute-targets` | `config/traceroute_targets.yaml` | Traceroute targets file |
| `--interval` | `10s` | Probe interval for all monitors |
| `--prom-port` | `2112` | Prometheus metrics port |
| `--log-format` | `json` | Log format: `json` or `text` |

## Metrics Reference

All metrics are prefixed with `monitoring_` and carry a `probe` label.

### TCP

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_tcp_connect_time_seconds` | histogram | probe, host, port | 3-way handshake duration (includes DNS if hostname used) |
| `monitoring_tcp_connection_success_total` | counter | probe, host, port | Successful connections |
| `monitoring_tcp_connection_failure_total` | counter | probe, host, port, error_type | Failed connections; `error_type`: timeout / refused / other |

### DNS

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_dns_resolution_time_seconds` | histogram | probe, domain, record_type, resolver | Resolution latency |
| `monitoring_dns_resolution_success_total` | counter | probe, domain, record_type, resolver | Successful resolutions |
| `monitoring_dns_resolution_failure_total` | counter | probe, domain, record_type, resolver, error_type | Failed resolutions; `error_type`: timeout / refused / nxdomain / servfail |
| `monitoring_dns_response_size_bytes` | histogram | probe, domain, record_type, resolver | DNS response size |
| `monitoring_dns_resolver_disagreement` | gauge | probe, domain, record_type | 1 when multi-resolver answers differ, 0 when consistent |

### HTTP

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_http_request_duration_seconds` | histogram | probe, url, method, status_code | Total request duration |
| `monitoring_http_dns_lookup_duration_seconds` | histogram | probe, url | DNS lookup phase |
| `monitoring_http_connect_duration_seconds` | histogram | probe, url | TCP connect phase |
| `monitoring_http_tls_handshake_duration_seconds` | histogram | probe, url | TLS handshake phase (HTTPS only) |
| `monitoring_http_first_byte_duration_seconds` | histogram | probe, url | Time to first response byte |
| `monitoring_http_response_size_bytes` | histogram | probe, url, method, status_code | Response body size |
| `monitoring_http_request_errors_total` | counter | probe, url, method, error_type | Transport-level errors |
| `monitoring_http_response_errors_total` | counter | probe, url, method, status_code | HTTP 4xx / 5xx responses |

### ICMP

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_icmp_rtt_seconds` | histogram | probe, host | Round-trip time per successful ping |
| `monitoring_icmp_packet_loss_ratio` | gauge | probe, host | Packet loss ratio per cycle (0.0 = no loss, 1.0 = total loss) |

### Synthetic Flows

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_synthetic_flow_duration_seconds` | histogram | probe, flow | Total duration of a complete flow |
| `monitoring_synthetic_flow_success_total` | counter | probe, flow | Fully successful flow executions |
| `monitoring_synthetic_flow_failure_total` | counter | probe, flow, step, reason | Failed executions; `reason`: transport / wrong_status / body_mismatch / extract_failed |
| `monitoring_synthetic_step_duration_seconds` | histogram | probe, flow, step | Duration of a single step |

### Traceroute

| Metric | Type | Labels | Description |
|---|---|---|---|
| `monitoring_traceroute_hop_rtt_seconds` | gauge | probe, target, hop | Average RTT to this hop in the most recent trace |
| `monitoring_traceroute_hop_ip_info` | gauge (info) | probe, target, hop, hop_ip | Always 1 — binds hop number to current IP address |
| `monitoring_traceroute_hops_total` | gauge | probe, target | Total hops to reach destination |

## Grafana Dashboard

Auto-provisioned from `grafana/dashboard.json`. The `probe` dropdown at the top filters all panels to one or more probe instances.

| Section | Panels |
|---|---|
| Health Summary | TCP/DNS/HTTP failure rate stat panels |
| TCP | Connect time p50/p95, failure rate by error type |
| DNS | Resolution time p50/p95, failure rate by error type |
| HTTP | Request duration, TTFB, waterfall breakdown, response errors, body size |
| ICMP | RTT p50/p95, packet loss ratio |
| SLO / Availability | HTTP 7-day availability (excl. 5xx), TCP 7-day availability |
| Synthetic Flows | Flow duration p50/p95, failures by step and reason |
| Traceroute | Per-hop RTT over time, total hop count |

## Docker Compose

```yaml
services:
  monitor:    # custom image, port 2112
  prometheus: # prom/prometheus:latest, port 9090
  grafana:    # grafana/grafana:latest, port 3000
```

The monitor service runs with `cap_add: NET_RAW` (required for ICMP and traceroute raw sockets) and `no-new-privileges: true`.

```bash
# Full rebuild after code changes
docker compose up --build -d

# Reload targets without restart
docker kill --signal=HUP network-monitor
```
