package dns

import (
	"bufio"
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Prometheus metrics
	dnsResolutionTime = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "resolution_time_seconds",
			Help:      "Time taken to resolve DNS in seconds",
		},
		[]string{"domain", "record_type", "resolver"},
	)

	dnsResolutionSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "resolution_success_total",
			Help:      "Total number of successful DNS resolutions",
		},
		[]string{"domain", "record_type", "resolver"},
	)

	dnsResolutionFailure = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "resolution_failure_total",
			Help:      "Total number of failed DNS resolutions",
		},
		[]string{"domain", "record_type", "resolver", "error"},
	)

	dnsResponseSize = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "response_size_bytes",
			Help:      "Size of DNS response in bytes",
		},
		[]string{"domain", "record_type", "resolver"},
	)
)

// Target represents a DNS target to monitor
type Target struct {
	Domain     string
	RecordType string
	Resolver   string
}

// Monitor handles DNS monitoring
type Monitor struct {
	targets  []Target
	interval time.Duration
	done     chan struct{}
}

// NewMonitor creates a new DNS monitor
func NewMonitor(targetsFile string, interval time.Duration) (*Monitor, error) {
	targets, err := loadTargets(targetsFile)
	if err != nil {
		return nil, err
	}

	return &Monitor{
		targets:  targets,
		interval: interval,
		done:     make(chan struct{}),
	}, nil
}

// Start begins the DNS monitoring
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	log.Printf("DNS Monitor started with %d targets", len(m.targets))

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

// Stop halts the DNS monitoring
func (m *Monitor) Stop() {
	close(m.done)
	log.Println("DNS Monitor stopped")
}

// checkAll performs DNS resolution for all targets
func (m *Monitor) checkAll() {
	for _, target := range m.targets {
		go m.checkDNS(target)
	}
}

// checkDNS performs a DNS query and records metrics
func (m *Monitor) checkDNS(target Target) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dns.Client{}
	qType, ok := dns.StringToType[target.RecordType]
	if !ok {
		qType = dns.TypeA // Default to A record if type is invalid
	}

	msg := dns.Msg{}
	msg.SetQuestion(dns.Fqdn(target.Domain), qType)

	resp, rtt, err := c.ExchangeContext(ctx, &msg, target.Resolver+":53")

	labels := prometheus.Labels{
		"domain":      target.Domain,
		"record_type": target.RecordType,
		"resolver":    target.Resolver,
	}

	if err != nil {
		errLabels := prometheus.Labels{
			"domain":      target.Domain,
			"record_type": target.RecordType,
			"resolver":    target.Resolver,
			"error":       err.Error(),
		}
		dnsResolutionFailure.With(errLabels).Inc()
		return
	}

	// Record successful metrics
	dnsResolutionTime.With(labels).Set(rtt.Seconds())
	dnsResolutionSuccess.With(labels).Inc()
	dnsResponseSize.With(labels).Set(float64(resp.Len()))
}

// loadTargets loads DNS targets from a file
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
		if len(parts) != 3 {
			log.Printf("Invalid format for DNS target: %s", line)
			continue
		}

		targets = append(targets, Target{
			Domain:     parts[0],
			RecordType: parts[1],
			Resolver:   parts[2],
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return targets, nil
}
