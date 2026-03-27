# networkReliability — TODOS

## Build vs. Wrap Blackbox Exporter

**What:** Evaluate whether to continue the custom probe binary vs. wrapping Prometheus Blackbox Exporter as scope grows.

**Why:** Blackbox Exporter already does HTTP/DNS/TCP probing, proper Histograms, RCODE checking, and config reload. The current codebase is building toward the same functionality. The custom approach is justified today because of the probe label architecture, SIGHUP config reload, and full ownership of the data pipeline — but this decision should be revisited if scope expands to ICMP, mTLS cert monitoring, or multi-step HTTP flows.

**Pros of staying custom:** Full control, no Blackbox config format lock-in, can add enterprise-specific metrics (e.g., per-segment comparison labels, internal DNS resolver tracking).

**Cons:** Maintaining correctness across probe types is non-trivial (DNS RCODE gap, TCP RTT accuracy, etc.). Blackbox Exporter has 8+ years of production hardening.

**Context:** Raised during /plan-eng-review 2026-03-27. Current approach is defensible. Revisit when adding a 4th probe type.

**Depends on:** None — informational.

---

## ICMP/Ping Monitor

**What:** Add a 4th monitor type: ICMP ping to measure raw latency between probe appliance and target hosts.

**Why:** TCP connect time approximates latency but requires an open port and includes TCP stack overhead. ICMP ping is a cleaner latency measurement and works to any host. "What is the latency between subnet A and B" is a stated use case.

**Pros:** True network-layer latency without application noise. Standard enterprise monitoring metric.

**Cons:** Requires `CAP_NET_RAW` capability in Docker (or setuid binary). `docker-compose.yml` needs `cap_add: [NET_RAW]`. Use the `go-ping` library (`github.com/go-ping/ping`).

**Context:** Flagged in /office-hours and /plan-eng-review 2026-03-27. Deliberately deferred.

**Depends on:** Complete current bug fixes + probe label + test coverage first.
