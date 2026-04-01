package tcp

import (
	"context"
	"fmt"
	"log/slog"
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
	wg          sync.WaitGroup // tracks in-flight probes for graceful shutdown
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
		slog.Error("TCP monitor reload failed", "error", err)
		return
	}
	m.mu.Lock()
	m.targets = targets
	m.mu.Unlock()
	slog.Info("TCP monitor reloaded targets", "count", len(targets), "file", m.targetsFile)
}

// Start begins the TCP monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	slog.Info("TCP monitor started", "targets", count, "probe", m.probe)

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

// Stop halts the TCP monitoring and waits for all in-flight probes to finish.
func (m *Monitor) Stop() {
	close(m.done)
	m.wg.Wait()
	slog.Info("TCP monitor stopped")
}

func (m *Monitor) checkAll() {
	m.mu.RLock()
	targets := make([]Target, len(m.targets))
	copy(targets, m.targets)
	m.mu.RUnlock()

	for _, t := range targets {
		t := t
		m.wg.Add(1)
		m.sem <- struct{}{}
		go func() {
			defer m.wg.Done()
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

type tcpYAMLConfig struct {
	Targets []tcpYAMLTarget `yaml:"targets"`
}

type tcpYAMLTarget struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

func loadTargets(file string) ([]Target, error) {
	if config.IsYAML(file) {
		return loadTargetsYAML(file)
	}
	// Legacy .txt path — kept for backward compatibility.
	rows, err := config.LoadLines(file, 2)
	if err != nil {
		return nil, err
	}
	targets := make([]Target, 0, len(rows))
	for _, parts := range rows {
		if _, err := strconv.Atoi(parts[1]); err != nil {
			slog.Warn("TCP: invalid port, skipping", "port", parts[1], "file", file)
			continue
		}
		targets = append(targets, Target{Host: parts[0], Port: parts[1]})
	}
	return targets, nil
}

func loadTargetsYAML(file string) ([]Target, error) {
	var cfg tcpYAMLConfig
	if err := config.DecodeYAML(file, &cfg); err != nil {
		return nil, err
	}
	var errs []string
	for i, t := range cfg.Targets {
		if t.Host == "" {
			errs = append(errs, fmt.Sprintf("target[%d]: host is required", i))
		}
		if t.Port < 1 || t.Port > 65535 {
			errs = append(errs, fmt.Sprintf("target[%d]: port %d is out of range (1-65535)", i, t.Port))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", file, strings.Join(errs, "; "))
	}
	targets := make([]Target, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		targets = append(targets, Target{Host: t.Host, Port: strconv.Itoa(t.Port)})
	}
	return targets, nil
}
