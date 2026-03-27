package tcp

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"monitoring-app/internal/config"
)

var (
	// tcpConnectTime is the wall-clock duration to establish a TCP connection,
	// which is the closest measurable approximation to the 3-way handshake RTT
	// from userspace (includes DNS if hostname is given, but targets use IPs).
	tcpConnectTime = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "connect_time_seconds",
			Help:      "Time to establish a TCP connection in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "host", "port"},
	)

	tcpConnectionSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "connection_success_total",
			Help:      "Total number of successful TCP connections",
		},
		[]string{"probe", "host", "port"},
	)

	tcpConnectionFailure = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "connection_failure_total",
			Help:      "Total number of failed TCP connections",
		},
		// error_type: timeout | refused | other
		[]string{"probe", "host", "port", "error_type"},
	)
)

// Target represents a TCP target to monitor.
type Target struct {
	Host string
	Port string
}

// Monitor handles TCP monitoring.
type Monitor struct {
	probe       string
	targetsFile string
	targets     []Target
	mu          sync.RWMutex
	interval    time.Duration
	sem         chan struct{}
	done        chan struct{}
}

// NewMonitor creates a new TCP monitor.
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
		log.Printf("TCP Monitor: reload failed: %v", err)
		return
	}
	m.mu.Lock()
	m.targets = targets
	m.mu.Unlock()
	log.Printf("TCP Monitor: reloaded %d targets from %s", len(targets), m.targetsFile)
}

// Start begins the TCP monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	log.Printf("TCP Monitor started with %d targets (probe=%s)", count, m.probe)

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

// Stop halts the TCP monitoring. In-flight probes finish naturally.
func (m *Monitor) Stop() {
	close(m.done)
	log.Println("TCP Monitor stopped")
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
			m.checkTCP(t)
		}()
	}
}

func (m *Monitor) checkTCP(target Target) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := &net.Dialer{KeepAlive: -1}
	addr := net.JoinHostPort(target.Host, target.Port)

	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	elapsed := time.Since(start)

	labels := prometheus.Labels{
		"probe": m.probe,
		"host":  target.Host,
		"port":  target.Port,
	}

	if err != nil {
		tcpConnectionFailure.With(prometheus.Labels{
			"probe":      m.probe,
			"host":       target.Host,
			"port":       target.Port,
			"error_type": classifyError(err),
		}).Inc()
		return
	}
	conn.Close()

	tcpConnectTime.With(labels).Observe(elapsed.Seconds())
	tcpConnectionSuccess.With(labels).Inc()
}

func classifyError(err error) string {
	s := err.Error()
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline") {
		return "timeout"
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
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
		if _, err := strconv.Atoi(parts[1]); err != nil {
			log.Printf("TCP: invalid port %q in %s — skipping", parts[1], file)
			continue
		}
		targets = append(targets, Target{Host: parts[0], Port: parts[1]})
	}
	return targets, nil
}
