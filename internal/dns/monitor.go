package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"monitoring-app/internal/config"
)

var (
	// dnsResolverDisagreement is 1 when two or more resolvers return different
	// answer sets for the same query (split-brain DNS, partial propagation, etc.).
	dnsResolverDisagreement = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "dns",
			Name:      "resolver_disagreement",
			Help:      "1 if multiple resolvers return different answers for the same query, 0 if consistent",
		},
		[]string{"probe", "domain", "record_type"},
	)

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

// Target represents a single-resolver DNS probe.
type Target struct {
	Domain     string
	RecordType string
	Resolver   string
}

// CompareTarget probes the same domain across multiple resolvers simultaneously
// and emits a disagreement metric when answers differ.
type CompareTarget struct {
	Domain     string
	RecordType string
	Resolvers  []string
}

// Monitor handles DNS monitoring.
type Monitor struct {
	probe          string
	targetsFile    string
	targets        []Target
	compareTargets []CompareTarget
	mu             sync.RWMutex
	interval       time.Duration
	sem            chan struct{} // bounds concurrent probes
	done           chan struct{}
	wg             sync.WaitGroup // tracks in-flight probes for graceful shutdown
}

// NewMonitor creates a new DNS monitor.
func NewMonitor(targetsFile, probe string, interval time.Duration) (*Monitor, error) {
	targets, compareTargets, err := loadTargets(targetsFile)
	if err != nil {
		return nil, err
	}
	return &Monitor{
		probe:          probe,
		targetsFile:    targetsFile,
		targets:        targets,
		compareTargets: compareTargets,
		interval:       interval,
		sem:            make(chan struct{}, 10),
		done:           make(chan struct{}),
	}, nil
}

// Reload re-reads the targets file atomically. Called on SIGHUP.
func (m *Monitor) Reload() {
	targets, compareTargets, err := loadTargets(m.targetsFile)
	if err != nil {
		slog.Error("DNS monitor reload failed", "error", err)
		return
	}
	m.mu.Lock()
	m.targets = targets
	m.compareTargets = compareTargets
	m.mu.Unlock()
	slog.Info("DNS monitor reloaded targets", "count", len(targets)+len(compareTargets), "file", m.targetsFile)
}

// Start begins the DNS monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	slog.Info("DNS monitor started", "targets", count, "probe", m.probe)

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

// Stop halts the DNS monitoring and waits for all in-flight probes to finish.
func (m *Monitor) Stop() {
	close(m.done)
	m.wg.Wait()
	slog.Info("DNS monitor stopped")
}

func (m *Monitor) checkAll() {
	m.mu.RLock()
	targets := make([]Target, len(m.targets))
	copy(targets, m.targets)
	compareTargets := make([]CompareTarget, len(m.compareTargets))
	copy(compareTargets, m.compareTargets)
	m.mu.RUnlock()

	for _, t := range targets {
		t := t
		m.wg.Add(1)
		m.sem <- struct{}{}
		go func() {
			defer m.wg.Done()
			defer func() { <-m.sem }()
			m.checkDNS(t)
		}()
	}

	for _, ct := range compareTargets {
		ct := ct
		m.wg.Add(1)
		m.sem <- struct{}{}
		go func() {
			defer m.wg.Done()
			defer func() { <-m.sem }()
			m.checkDNSCompare(ct)
		}()
	}
}

// checkDNSCompare queries the same domain across all resolvers in ct.Resolvers,
// emits standard per-resolver metrics for each, then emits a disagreement gauge
// if the answer sets differ across resolvers.
func (m *Monitor) checkDNSCompare(ct CompareTarget) {
	type result struct {
		resolver string
		answers  []string // sorted RR values
		ok       bool
	}

	resultCh := make(chan result, len(ct.Resolvers))

	for _, resolver := range ct.Resolvers {
		resolver := resolver
		go func() {
			answers, ok := m.queryAndRecord(ct.Domain, ct.RecordType, resolver)
			resultCh <- result{resolver: resolver, answers: answers, ok: ok}
		}()
	}

	// Collect all results.
	results := make([]result, 0, len(ct.Resolvers))
	for range ct.Resolvers {
		results = append(results, <-resultCh)
	}

	// Compare answer sets across resolvers that succeeded.
	var successAnswers [][]string
	for _, r := range results {
		if r.ok {
			successAnswers = append(successAnswers, r.answers)
		}
	}

	disagreement := 0.0
	if len(successAnswers) >= 2 {
		ref := canonicalAnswerSet(successAnswers[0])
		for _, other := range successAnswers[1:] {
			if canonicalAnswerSet(other) != ref {
				disagreement = 1.0
				break
			}
		}
	}

	dnsResolverDisagreement.With(prometheus.Labels{
		"probe":       m.probe,
		"domain":      ct.Domain,
		"record_type": ct.RecordType,
	}).Set(disagreement)

	if disagreement == 1.0 {
		slog.Warn("DNS resolver disagreement detected",
			"domain", ct.Domain,
			"record_type", ct.RecordType,
			"resolvers", ct.Resolvers,
		)
	}
}

// queryAndRecord performs a single DNS query, records standard metrics, and
// returns the sorted answer strings plus a success flag.
func (m *Monitor) queryAndRecord(domain, recordType, resolver string) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dns.Client{}
	qType, ok := dns.StringToType[recordType]
	if !ok {
		qType = dns.TypeA
	}

	msg := dns.Msg{}
	msg.SetQuestion(dns.Fqdn(domain), qType)

	resolverAddr := resolver
	if !strings.Contains(resolverAddr, ":") {
		resolverAddr += ":53"
	}

	start := time.Now()
	resp, _, err := c.ExchangeContext(ctx, &msg, resolverAddr)
	rtt := time.Since(start)

	baseLabels := prometheus.Labels{
		"probe":       m.probe,
		"domain":      domain,
		"record_type": recordType,
		"resolver":    resolver,
	}

	if err != nil {
		dnsResolutionFailure.With(prometheus.Labels{
			"probe": m.probe, "domain": domain,
			"record_type": recordType, "resolver": resolver,
			"error_type": classifyError(err),
		}).Inc()
		return nil, false
	}
	if resp.Rcode != dns.RcodeSuccess {
		dnsResolutionFailure.With(prometheus.Labels{
			"probe": m.probe, "domain": domain,
			"record_type": recordType, "resolver": resolver,
			"error_type": classifyRcode(resp.Rcode),
		}).Inc()
		return nil, false
	}

	dnsResolutionTime.With(baseLabels).Observe(rtt.Seconds())
	dnsResolutionSuccess.With(baseLabels).Inc()
	dnsResponseSize.With(baseLabels).Observe(float64(resp.Len()))

	answers := make([]string, 0, len(resp.Answer))
	for _, rr := range resp.Answer {
		answers = append(answers, rr.String())
	}
	return answers, true
}

// canonicalAnswerSet sorts and joins answer strings so two sets can be compared
// with a simple string equality check.
func canonicalAnswerSet(answers []string) string {
	sorted := make([]string, len(answers))
	copy(sorted, answers)
	sort.Strings(sorted)
	return strings.Join(sorted, "\n")
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

type dnsYAMLConfig struct {
	Targets []dnsYAMLTarget `yaml:"targets"`
}

type dnsYAMLTarget struct {
	Domain     string   `yaml:"domain"`
	RecordType string   `yaml:"record_type"`
	Resolver   string   `yaml:"resolver"`   // single-resolver probe
	Resolvers  []string `yaml:"resolvers"`  // multi-resolver comparison (mutually exclusive with Resolver)
}

func loadTargets(file string) ([]Target, []CompareTarget, error) {
	if config.IsYAML(file) {
		return loadTargetsYAML(file)
	}
	// Legacy .txt path — kept for backward compatibility (no multi-resolver support).
	rows, err := config.LoadLines(file, 3)
	if err != nil {
		return nil, nil, err
	}
	targets := make([]Target, 0, len(rows))
	for _, parts := range rows {
		targets = append(targets, Target{
			Domain:     parts[0],
			RecordType: parts[1],
			Resolver:   parts[2],
		})
	}
	return targets, nil, nil
}

func loadTargetsYAML(file string) ([]Target, []CompareTarget, error) {
	var cfg dnsYAMLConfig
	if err := config.DecodeYAML(file, &cfg); err != nil {
		return nil, nil, err
	}
	var errs []string
	for i, t := range cfg.Targets {
		if t.Domain == "" {
			errs = append(errs, fmt.Sprintf("target[%d]: domain is required", i))
		}
		if t.RecordType == "" {
			errs = append(errs, fmt.Sprintf("target[%d]: record_type is required", i))
		}
		hasResolver := t.Resolver != ""
		hasResolvers := len(t.Resolvers) > 0
		if !hasResolver && !hasResolvers {
			errs = append(errs, fmt.Sprintf("target[%d]: resolver or resolvers is required", i))
		}
		if hasResolver && hasResolvers {
			errs = append(errs, fmt.Sprintf("target[%d]: resolver and resolvers are mutually exclusive", i))
		}
		if hasResolvers && len(t.Resolvers) < 2 {
			errs = append(errs, fmt.Sprintf("target[%d]: resolvers requires at least 2 entries", i))
		}
	}
	if len(errs) > 0 {
		return nil, nil, fmt.Errorf("%s: %s", file, strings.Join(errs, "; "))
	}
	var targets []Target
	var compareTargets []CompareTarget
	for _, t := range cfg.Targets {
		if t.Resolver != "" {
			targets = append(targets, Target{
				Domain:     t.Domain,
				RecordType: t.RecordType,
				Resolver:   t.Resolver,
			})
		} else {
			compareTargets = append(compareTargets, CompareTarget{
				Domain:     t.Domain,
				RecordType: t.RecordType,
				Resolvers:  t.Resolvers,
			})
		}
	}
	return targets, compareTargets, nil
}
