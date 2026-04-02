# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [0.1.0.0] - 2026-04-02

### Added
- **Synthetic flow monitor** — probe multi-step HTTP sequences where extracted values (tokens, IDs) from one step are injected into subsequent steps via `${VAR}` substitution. Supports `body_contains` assertions, JSON extraction with dot-notation paths, and per-step failure classification (`transport` / `wrong_status` / `body_mismatch` / `extract_failed`).
- **Traceroute monitor** — periodic ICMP TTL-based path tracing. Reports per-hop RTT and the IP at each hop as a Prometheus info metric, enabling PromQL `group_left(hop_ip)` joins for annotated RTT graphs. Requires `CAP_NET_RAW`.
- **Multi-resolver DNS comparison** — query the same domain across multiple resolvers simultaneously and emit `monitoring_dns_resolver_disagreement=1` when answer sets differ (split-brain detection, propagation lag).
- **HTTP auth headers** — targets can specify arbitrary request headers (e.g. `Authorization: Bearer ${TOKEN}`) with `${ENV_VAR}` substitution for secrets.
- **SLO / Availability panels** — Grafana dashboard sections for HTTP 7-day availability (excluding 5xx) and TCP 7-day availability.
- **Grafana panels** — Synthetic Flows row (duration p50/p95, failures by step and reason) and Traceroute row (per-hop RTT, total hop count).
- **Test coverage** — added tests for multi-resolver DNS comparison, HTTP headers, synthetic flow engine (expandVars, extractJSONKey, loadFlows, runFlow failure modes), and traceroute config loading.

### Changed
- README rewritten to document all Tier 1–3 features: configuration examples for every monitor type, CLI flags table, full metrics reference, Grafana dashboard section breakdown.
- `cmd/monitor/main.go` wired with `--synthetic-flows` and `--traceroute-targets` flags; both monitors included in SIGHUP reload and graceful shutdown.
