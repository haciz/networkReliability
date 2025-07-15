# Network Monitoring Application

This application monitors TCP, DNS, and HTTP services using Prometheus metrics, which can be visualized in Grafana.

## Features

### TCP Monitoring
- 3-way handshake latency measurement
- Connection success/failure tracking
- Round-trip time (RTT) approximation
- Connection reset detection

### DNS Monitoring
- Resolution time measurement
- Success/failure tracking
- Response size tracking
- Support for different DNS record types

### HTTP Monitoring
- Full request/response timing
- DNS lookup duration for HTTP requests
- Connection establishment time
- TLS handshake duration
- Time to first byte
- Response size tracking

## Requirements

- Go 1.23 or higher
- Prometheus server for metrics storage
- Grafana for visualization

## Installation

```bash
# Clone the repository
git clone https://github.com/yourusername/network-monitoring.git
cd network-monitoring

# Build the application
go build -o bin/monitor cmd/monitor/main.go
```

## Configuration

Configure your targets in the following files:

- `config/tcp_targets.txt`: TCP endpoints to monitor
- `config/dns_targets.txt`: DNS queries to perform
- `config/http_targets.txt`: HTTP endpoints to monitor

## Usage

```bash
# Start the monitoring application
./bin/monitor
```

Command-line options:

```
  -dns-targets string
        File with DNS targets to monitor (default "config/dns_targets.txt")
  -http-targets string
        File with HTTP targets to monitor (default "config/http_targets.txt")
  -interval duration
        Monitoring interval (default 10s)
  -prom-port string
        Prometheus metrics port (default "2112")
  -tcp-targets string
        File with TCP targets to monitor (default "config/tcp_targets.txt")
```

## Prometheus Configuration

Add the following to your Prometheus configuration to scrape metrics from this application:

```yaml
scrape_configs:
  - job_name: 'network-monitor'
    scrape_interval: 15s
    static_configs:
      - targets: ['localhost:2112']
```

## Grafana Dashboard

A sample Grafana dashboard is available in `grafana/dashboard.json`. Import this into your Grafana instance to visualize the collected metrics.

## Metrics

### TCP Metrics

#### Basic Metrics
- `monitoring_tcp_connect_time_seconds`: Time to establish a TCP connection
- `monitoring_tcp_handshake_time_seconds`: Time for TCP 3-way handshake
- `monitoring_tcp_rtt_seconds`: TCP Round Trip Time
- `monitoring_tcp_connection_success_total`: Successful TCP connection count
- `monitoring_tcp_connection_failure_total`: Failed TCP connection count
- `monitoring_tcp_connection_resets_total`: TCP connection reset count

#### Connection Quality Metrics
- `monitoring_tcp_connection_jitter_seconds`: Jitter in TCP connection establishment times
- `monitoring_tcp_packet_loss_percentage`: Estimated packet loss percentage

#### Connection Persistence and Stability
- `monitoring_tcp_connection_duration_seconds`: Duration of TCP connections
- `monitoring_tcp_graceful_close_total`: Number of gracefully closed connections

#### Advanced TCP State Monitoring
- `monitoring_tcp_state_changes_total`: TCP state transitions (SYN, FIN, RST, etc.)
- `monitoring_tcp_socket_buffer_usage_bytes`: TCP socket buffer usage

#### Network Path Analysis
- `monitoring_tcp_network_path_changes_total`: Network path changes
- `monitoring_tcp_network_hops_count`: Number of network hops to target
- `monitoring_tcp_mtu_bytes`: Maximum Transmission Unit in bytes

#### Error Classification and Recovery
- `monitoring_tcp_error_types_total`: Categorized TCP error types
- `monitoring_tcp_recovery_time_seconds`: Time to recover from failures

#### Performance Under Load
- `monitoring_tcp_concurrent_connections`: Number of concurrent connections
- `monitoring_tcp_connection_rate_per_second`: Connection establishment rate

#### Security and Compliance
- `monitoring_tcp_security_events_total`: Security-related events

#### Service-Level Metrics
- `monitoring_tcp_service_availability_percentage`: Service availability percentage
- `monitoring_tcp_mtbf_seconds`: Mean Time Between Failures
- `monitoring_tcp_mttr_seconds`: Mean Time To Recovery
- `monitoring_tcp_sla_compliance_percentage`: SLA compliance percentage

### DNS Metrics
- `monitoring_dns_resolution_time_seconds`: Time to resolve DNS query
- `monitoring_dns_resolution_success_total`: Successful DNS resolution count
- `monitoring_dns_resolution_failure_total`: Failed DNS resolution count
- `monitoring_dns_response_size_bytes`: Size of DNS response

### HTTP Metrics
- `monitoring_http_request_duration_seconds`: Total HTTP request duration
- `monitoring_http_dns_lookup_duration_seconds`: DNS lookup time for HTTP requests
- `monitoring_http_connect_duration_seconds`: Connection time for HTTP requests
- `monitoring_http_tls_handshake_duration_seconds`: TLS handshake time
- `monitoring_http_first_byte_duration_seconds`: Time to first response byte
- `monitoring_http_response_size_bytes`: HTTP response size
- `monitoring_http_request_errors_total`: HTTP request error count
