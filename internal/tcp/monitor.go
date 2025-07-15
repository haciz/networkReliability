package tcp

import (
	"bufio"
	"context"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/net/trace"
)

var (
	// Prometheus metrics for TCP monitoring
	tcpConnectTime = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "connect_time_seconds",
			Help:      "Time taken to establish TCP connection in seconds",
		},
		[]string{"host", "port"},
	)

	tcpHandshakeTime = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "handshake_time_seconds",
			Help:      "Time taken for TCP 3-way handshake in seconds",
		},
		[]string{"host", "port"},
	)

	tcpRTT = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "rtt_seconds",
			Help:      "TCP Round Trip Time in seconds",
		},
		[]string{"host", "port"},
	)

	tcpConnectionSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "connection_success_total",
			Help:      "Total number of successful TCP connections",
		},
		[]string{"host", "port"},
	)

	tcpConnectionFailure = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "connection_failure_total",
			Help:      "Total number of failed TCP connections",
		},
		[]string{"host", "port", "error"},
	)

	tcpConnectionResets = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "tcp",
			Name:      "connection_resets_total",
			Help:      "Total number of TCP connection resets",
		},
		[]string{"host", "port"},
	)
)

// Target represents a TCP target to monitor
type Target struct {
	Host string
	Port string
}

// Monitor handles TCP monitoring
type Monitor struct {
	targets  []Target
	interval time.Duration
	done     chan struct{}
}

// NewMonitor creates a new TCP monitor
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

// Start begins the TCP monitoring
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	log.Printf("TCP Monitor started with %d targets", len(m.targets))

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

// Stop halts the TCP monitoring
func (m *Monitor) Stop() {
	close(m.done)
	log.Println("TCP Monitor stopped")
}

// checkAll performs TCP checks for all targets
func (m *Monitor) checkAll() {
	for _, target := range m.targets {
		go m.checkTCP(target)
	}
}

// checkTCP performs a TCP connection test and records metrics
func (m *Monitor) checkTCP(target Target) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tr := trace.New("tcp", "connect")
	defer tr.Finish()

	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: -1, // No keep-alive for monitoring connections
	}

	addr := net.JoinHostPort(target.Host, target.Port)
	tr.LazyPrintf("Connecting to %s", addr)

	connectStart := time.Now()

	// Establish TCP connection
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	connectEnd := time.Now()
	connectDuration := connectEnd.Sub(connectStart)

	// Create base labels for metrics
	labels := prometheus.Labels{
		"host": target.Host,
		"port": target.Port,
	}

	if err != nil {
		tr.LazyPrintf("Connection failed: %v", err)
		tr.SetError()

		// Record failure
		errLabels := prometheus.Labels{
			"host":  target.Host,
			"port":  target.Port,
			"error": err.Error(),
		}
		tcpConnectionFailure.With(errLabels).Inc()
		return
	}
	defer conn.Close()

	// Measure TCP handshake time (approximately equal to the connect time in most cases)
	tcpHandshakeTime.With(labels).Set(connectDuration.Seconds())
	tcpConnectTime.With(labels).Set(connectDuration.Seconds())
	tcpConnectionSuccess.With(labels).Inc()

	// Attempt to get more detailed TCP info including RTT
	tcpConn, ok := conn.(*net.TCPConn)
	if ok {
		if _, err := tcpConn.SyscallConn(); err == nil {
			// For simplicity, we're using the connect time as an approximation of RTT
			// In a real implementation, you might use platform-specific code to get actual RTT
			rtt := connectDuration / 2 // Very rough approximation
			tcpRTT.With(labels).Set(rtt.Seconds())
		}
	}

	tr.LazyPrintf("Connection successful, duration: %v", connectDuration)
}

// loadTargets loads TCP targets from a file
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
			log.Printf("Invalid format for TCP target: %s", line)
			continue
		}

		// Validate port number
		port := parts[1]
		if _, err := strconv.Atoi(port); err != nil {
			log.Printf("Invalid port number for TCP target: %s", line)
			continue
		}

		targets = append(targets, Target{
			Host: parts[0],
			Port: port,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return targets, nil
}
