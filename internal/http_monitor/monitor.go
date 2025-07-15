package http_monitor

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Prometheus metrics for HTTP monitoring
	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "Time taken to complete HTTP request",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"url", "method", "status_code"},
	)

	httpDNSLookupDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "dns_lookup_duration_seconds",
			Help:      "Time taken for DNS lookup during HTTP request",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"url"},
	)

	httpConnectDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "connect_duration_seconds",
			Help:      "Time taken to establish connection during HTTP request",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"url"},
	)

	httpTLSHandshakeDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "tls_handshake_duration_seconds",
			Help:      "Time taken for TLS handshake during HTTP request",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"url"},
	)

	httpFirstByteDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "first_byte_duration_seconds",
			Help:      "Time taken to receive first byte of response",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"url"},
	)

	httpResponseSize = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "response_size_bytes",
			Help:      "Size of HTTP response in bytes",
		},
		[]string{"url", "method", "status_code"},
	)

	httpRequestErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "request_errors_total",
			Help:      "Total number of HTTP request errors",
		},
		[]string{"url", "method", "error"},
	)
)

// Target represents an HTTP target to monitor
type Target struct {
	URL    string
	Method string
}

// Monitor handles HTTP monitoring
type Monitor struct {
	targets  []Target
	interval time.Duration
	client   *http.Client
	done     chan struct{}
}

// NewMonitor creates a new HTTP monitor
func NewMonitor(targetsFile string, interval time.Duration) (*Monitor, error) {
	targets, err := loadTargets(targetsFile)
	if err != nil {
		return nil, err
	}

	// Create a custom HTTP client with longer timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false, // Set to true only for testing
			},
		},
	}

	return &Monitor{
		targets:  targets,
		interval: interval,
		client:   client,
		done:     make(chan struct{}),
	}, nil
}

// Start begins the HTTP monitoring
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	log.Printf("HTTP Monitor started with %d targets", len(m.targets))

	// Run immediately at start
	m.checkAll()

	for {
		select {
		case <-ticker.C:
			m.checkAll()
		case <-m.done:
			return
		}
	}
}

// Stop halts the HTTP monitoring
func (m *Monitor) Stop() {
	close(m.done)
	log.Println("HTTP Monitor stopped")
}

// checkAll performs HTTP requests for all targets
func (m *Monitor) checkAll() {
	for _, target := range m.targets {
		go m.checkHTTP(target)
	}
}

// checkHTTP performs an HTTP request and records metrics
func (m *Monitor) checkHTTP(target Target) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		dnsStart, dnsEnd                   time.Time
		connectStart, connectEnd           time.Time
		tlsHandshakeStart, tlsHandshakeEnd time.Time
		firstByteTime                      time.Time
	)

	trace := &httptrace.ClientTrace{
		DNSStart:             func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(_ httptrace.DNSDoneInfo) { dnsEnd = time.Now() },
		ConnectStart:         func(_, _ string) { connectStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { connectEnd = time.Now() },
		TLSHandshakeStart:    func() { tlsHandshakeStart = time.Now() },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { tlsHandshakeEnd = time.Now() },
		GotFirstResponseByte: func() { firstByteTime = time.Now() },
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), target.Method, target.URL, nil)
	if err != nil {
		errLabels := prometheus.Labels{"url": target.URL, "method": target.Method, "error": err.Error()}
		httpRequestErrors.With(errLabels).Inc()
		return
	}

	start := time.Now()
	resp, err := m.client.Do(req)
	if err != nil {
		errLabels := prometheus.Labels{"url": target.URL, "method": target.Method, "error": err.Error()}
		httpRequestErrors.With(errLabels).Inc()
		return
	}
	defer resp.Body.Close()

	// Calculate durations
	if !dnsStart.IsZero() && !dnsEnd.IsZero() {
		httpDNSLookupDuration.With(prometheus.Labels{"url": target.URL}).Observe(dnsEnd.Sub(dnsStart).Seconds())
	}

	if !connectStart.IsZero() && !connectEnd.IsZero() {
		httpConnectDuration.With(prometheus.Labels{"url": target.URL}).Observe(connectEnd.Sub(connectStart).Seconds())
	}

	if !tlsHandshakeStart.IsZero() && !tlsHandshakeEnd.IsZero() {
		httpTLSHandshakeDuration.With(prometheus.Labels{"url": target.URL}).Observe(tlsHandshakeEnd.Sub(tlsHandshakeStart).Seconds())
	}

	if !firstByteTime.IsZero() {
		httpFirstByteDuration.With(prometheus.Labels{"url": target.URL}).Observe(firstByteTime.Sub(start).Seconds())
	}

	// Read the response body to get size
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		errLabels := prometheus.Labels{"url": target.URL, "method": target.Method, "error": err.Error()}
		httpRequestErrors.With(errLabels).Inc()
		return
	}

	status := resp.StatusCode
	requestDuration := time.Since(start).Seconds()

	// Record metrics
	labels := prometheus.Labels{"url": target.URL, "method": target.Method, "status_code": string(rune(status))}
	httpRequestDuration.With(labels).Observe(requestDuration)
	httpResponseSize.With(labels).Set(float64(len(body)))
}

// loadTargets loads HTTP targets from a file
func loadTargets(file string) ([]Target, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var targets []Target
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) != 2 {
			log.Printf("Invalid format for HTTP target: %s", line)
			continue
		}

		targets = append(targets, Target{
			URL:    parts[0],
			Method: parts[1],
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return targets, nil
}
