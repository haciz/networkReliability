package dns

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"monitoring-app/internal/config"
)

var (
	dnsResolutionTime = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "resolution_time_seconds",
			Help:      "Time taken to resolve DNS in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "domain", "record_type", "resolver"},
	)

	dnsResolutionSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "resolution_success_total",
			Help:      "Total number of successful DNS resolutions",
		},
		[]string{"probe", "domain", "record_type", "resolver"},
	)

	dnsResolutionFailure = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "resolution_failure_total",
			Help:      "Total number of failed DNS resolutions",
		},
		// error_type is a bounded enum: timeout | refused | nxdomain | servfail | other
		[]string{"probe", "domain", "record_type", "resolver", "error_type"},
	)

	dnsResponseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "response_size_bytes",
			Help:      "Size of DNS response in bytes",
			Buckets:   []float64{64, 128, 256, 512, 1024, 2048, 4096},
		},
		[]string{"probe", "domain", "record_type", "resolver"},
	)
)

// Target represents a DNS target to monitor.
type Target struct {
	Domain     string
	RecordType string
	Resolver   string
}

// Monitor handles DNS monitoring.
type Monitor struct {
	probe     string
	targetsFile string
	targets   []Target
	mu        sync.RWMutex
	interval  time.Duration
	sem       chan struct{} // bounds concurrent probes
	done      chan struct{}
}

// NewMonitor creates a new DNS monitor.
func NewMonitor(targetsFile, probe string, interval time.Duration) (*Monitor, error) {
	targets, err := loadTargets(targetsFile)
	if err != nil {
		return nil, err
	}
	return &Monitor{
		probe:       probe,
		targetsFile: targetsFile,
		targets:     targets,
		interval:    interval,
		sem:         make(chan struct{}, 10),
		done:        make(chan struct{}),
	}, nil
}

// Reload re-reads the targets file atomically. Called on SIGHUP.
func (m *Monitor) Reload() {
	targets, err := loadTargets(m.targetsFile)
	if err != nil {
		log.Printf("DNS Monitor: reload failed: %v", err)
		return
	}
	m.mu.Lock()
	m.targets = targets
	m.mu.Unlock()
	log.Printf("DNS Monitor: reloaded %d targets from %s", len(targets), m.targetsFile)
}

// Start begins the DNS monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	log.Printf("DNS Monitor started with %d targets (probe=%s)", count, m.probe)

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

// Stop halts the DNS monitoring. In-flight probes finish naturally.
func (m *Monitor) Stop() {
	close(m.done)
	log.Println("DNS Monitor stopped")
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
			m.checkDNS(t)
		}()
	}
}

func (m *Monitor) checkDNS(target Target) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dns.Client{}
	qType, ok := dns.StringToType[target.RecordType]
	if !ok {
		qType = dns.TypeA
	}

	msg := dns.Msg{}
	msg.SetQuestion(dns.Fqdn(target.Domain), qType)

	start := time.Now()
	// Support "host" (appends :53) and "host:port" (used as-is, e.g. for testing).
	resolverAddr := target.Resolver
	if !strings.Contains(resolverAddr, ":") {
		resolverAddr = resolverAddr + ":53"
	}
	resp, _, err := c.ExchangeContext(ctx, &msg, resolverAddr)
	rtt := time.Since(start)

	baseLabels := prometheus.Labels{
		"probe":       m.probe,
		"domain":      target.Domain,
		"record_type": target.RecordType,
		"resolver":    target.Resolver,
	}

	if err != nil {
		dnsResolutionFailure.With(prometheus.Labels{
			"probe":       m.probe,
			"domain":      target.Domain,
			"record_type": target.RecordType,
			"resolver":    target.Resolver,
			"error_type":  classifyError(err),
		}).Inc()
		return
	}

	// Check DNS-level errors (NXDOMAIN, SERVFAIL, etc.) — err==nil doesn't mean success
	if resp.Rcode != dns.RcodeSuccess {
		dnsResolutionFailure.With(prometheus.Labels{
			"probe":       m.probe,
			"domain":      target.Domain,
			"record_type": target.RecordType,
			"resolver":    target.Resolver,
			"error_type":  classifyRcode(resp.Rcode),
		}).Inc()
		return
	}

	dnsResolutionTime.With(baseLabels).Observe(rtt.Seconds())
	dnsResolutionSuccess.With(baseLabels).Inc()
	dnsResponseSize.With(baseLabels).Observe(float64(resp.Len()))
}

// classifyError maps a transport error to a bounded enum string.
func classifyError(err error) string {
	s := err.Error()
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline") {
		return "timeout"
	}
	if strings.Contains(s, "refused") {
		return "refused"
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return "timeout"
	}
	return "other"
}

// classifyRcode maps a DNS RCODE to a bounded enum string.
func classifyRcode(rcode int) string {
	switch rcode {
	case dns.RcodeNameError: // NXDOMAIN
		return "nxdomain"
	case dns.RcodeServerFailure:
		return "servfail"
	case dns.RcodeRefused:
		return "refused"
	default:
		return fmt.Sprintf("rcode_%d", rcode)
	}
}

func loadTargets(file string) ([]Target, error) {
	rows, err := config.LoadLines(file, 3)
	if err != nil {
		return nil, err
	}
	targets := make([]Target, 0, len(rows))
	for _, parts := range rows {
		targets = append(targets, Target{
			Domain:     parts[0],
			RecordType: parts[1],
			Resolver:   parts[2],
		})
	}
	return targets, nil
}
