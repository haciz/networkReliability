package http_monitor

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"monitoring-app/internal/config"
)

var (
	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "Total time to complete HTTP request in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "url", "method", "status_code"},
	)

	httpDNSLookupDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "dns_lookup_duration_seconds",
			Help:      "Time for DNS lookup phase of HTTP request in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "url"},
	)

	httpConnectDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "connect_duration_seconds",
			Help:      "Time for TCP connect phase of HTTP request in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "url"},
	)

	httpTLSHandshakeDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "tls_handshake_duration_seconds",
			Help:      "Time for TLS handshake phase of HTTP request in seconds (HTTPS only)",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "url"},
	)

	httpFirstByteDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "first_byte_duration_seconds",
			Help:      "Time from request start to first response byte in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "url"},
	)

	// httpRequestErrors counts transport-level failures (connection refused, timeout, etc.)
	httpRequestErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "request_errors_total",
			Help:      "Total number of HTTP transport-level errors",
		},
		// error_type: timeout | refused | other
		[]string{"probe", "url", "method", "error_type"},
	)

	// httpResponseErrors counts HTTP-level failures (4xx, 5xx responses)
	httpResponseErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "http",
			Name:      "response_errors_total",
			Help:      "Total number of HTTP 4xx/5xx responses",
		},
		[]string{"probe", "url", "method", "status_code"},
	)
)

// Target represents an HTTP target to monitor.
type Target struct {
	URL    string
	Method string
}

// Monitor handles HTTP monitoring.
type Monitor struct {
	probe       string
	targetsFile string
	targets     []Target
	mu          sync.RWMutex
	interval    time.Duration
	client      *http.Client
	sem         chan struct{}
	done        chan struct{}
}

// NewMonitor creates a new HTTP monitor.
func NewMonitor(targetsFile, probe string, interval time.Duration) (*Monitor, error) {
	targets, err := loadTargets(targetsFile)
	if err != nil {
		return nil, err
	}

	// DisableKeepAlives ensures DNS and TCP connect hooks fire on every probe.
	// Keep-alives reuse connections, which bypasses httptrace hooks and gives
	// zero readings for dns_lookup and connect phases — defeating the waterfall.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
			},
		},
	}

	return &Monitor{
		probe:       probe,
		targetsFile: targetsFile,
		targets:     targets,
		interval:    interval,
		client:      client,
		sem:         make(chan struct{}, 10),
		done:        make(chan struct{}),
	}, nil
}

// Reload re-reads the targets file atomically. Called on SIGHUP.
func (m *Monitor) Reload() {
	targets, err := loadTargets(m.targetsFile)
	if err != nil {
		log.Printf("HTTP Monitor: reload failed: %v", err)
		return
	}
	m.mu.Lock()
	m.targets = targets
	m.mu.Unlock()
	log.Printf("HTTP Monitor: reloaded %d targets from %s", len(targets), m.targetsFile)
}

// Start begins the HTTP monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	log.Printf("HTTP Monitor started with %d targets (probe=%s)", count, m.probe)

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

// Stop halts the HTTP monitoring. In-flight probes finish naturally.
func (m *Monitor) Stop() {
	close(m.done)
	log.Println("HTTP Monitor stopped")
}

func (m *Monitor) checkAll() {
	m.mu.RLock()
	targets := make([]Target, len(m.targets))
	copy(targets, m.targets)
	m.mu.RUnlock()

	for _, t := range targets {
		t := t
		m.sem <- struct{}{}
		go func() {
			defer func() { <-m.sem }()
			m.checkHTTP(t)
		}()
	}
}

func (m *Monitor) checkHTTP(target Target) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Normalize URL: strip query parameters to prevent label cardinality explosion.
	normURL := normalizeURL(target.URL)

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
		httpRequestErrors.With(prometheus.Labels{
			"probe":      m.probe,
			"url":        normURL,
			"method":     target.Method,
			"error_type": "other",
		}).Inc()
		return
	}

	start := time.Now()
	resp, err := m.client.Do(req)
	if err != nil {
		httpRequestErrors.With(prometheus.Labels{
			"probe":      m.probe,
			"url":        normURL,
			"method":     target.Method,
			"error_type": classifyError(err),
		}).Inc()
		return
	}
	defer resp.Body.Close()

	// Drain body without allocating — we only care about timing and status,
	// not the response content.
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	requestDuration := time.Since(start).Seconds()
	statusCode := strconv.Itoa(resp.StatusCode)

	// Record waterfall timings.
	if !dnsStart.IsZero() && !dnsEnd.IsZero() {
		httpDNSLookupDuration.With(prometheus.Labels{
			"probe": m.probe,
			"url":   normURL,
		}).Observe(dnsEnd.Sub(dnsStart).Seconds())
	}
	if !connectStart.IsZero() && !connectEnd.IsZero() {
		httpConnectDuration.With(prometheus.Labels{
			"probe": m.probe,
			"url":   normURL,
		}).Observe(connectEnd.Sub(connectStart).Seconds())
	}
	if !tlsHandshakeStart.IsZero() && !tlsHandshakeEnd.IsZero() {
		httpTLSHandshakeDuration.With(prometheus.Labels{
			"probe": m.probe,
			"url":   normURL,
		}).Observe(tlsHandshakeEnd.Sub(tlsHandshakeStart).Seconds())
	}
	if !firstByteTime.IsZero() {
		httpFirstByteDuration.With(prometheus.Labels{
			"probe": m.probe,
			"url":   normURL,
		}).Observe(firstByteTime.Sub(start).Seconds())
	}

	labels := prometheus.Labels{
		"probe":       m.probe,
		"url":         normURL,
		"method":      target.Method,
		"status_code": statusCode,
	}
	httpRequestDuration.With(labels).Observe(requestDuration)

	// 4xx and 5xx are application-level failures, tracked separately
	// so they don't silently count as successes in dashboards.
	if resp.StatusCode >= 400 {
		httpResponseErrors.With(labels).Inc()
	}
}

// normalizeURL strips query parameters from a URL to prevent unbounded label cardinality.
func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func classifyError(err error) string {
	s := err.Error()
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline") {
		return "timeout"
	}
	if strings.Contains(s, "refused") {
		return "refused"
	}
	return "other"
}

func loadTargets(file string) ([]Target, error) {
	rows, err := config.LoadLines(file, 2)
	if err != nil {
		return nil, err
	}
	targets := make([]Target, 0, len(rows))
	for _, parts := range rows {
		targets = append(targets, Target{URL: parts[0], Method: parts[1]})
	}
	return targets, nil
}
