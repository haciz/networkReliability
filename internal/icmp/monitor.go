package icmp

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"monitoring-app/internal/config"
)

var (
	icmpRTT = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "icmp",
			Name:      "rtt_seconds",
			Help:      "ICMP round-trip time per successful ping in seconds",
			Buckets:   []float64{.0001, .0005, .001, .005, .01, .05, .1, .5, 1},
		},
		[]string{"probe", "host"},
	)

	icmpPacketLoss = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "icmp",
			Name:      "packet_loss_ratio",
			Help:      "Ratio of lost ICMP packets per probe cycle (0.0 = no loss, 1.0 = total loss)",
		},
		[]string{"probe", "host"},
	)
)

// Target represents an ICMP ping target.
type Target struct {
	Host  string
	Count int // number of pings per cycle; defaults to 5
}

// Monitor handles ICMP ping monitoring.
type Monitor struct {
	probe       string
	targetsFile string
	targets     []Target
	mu          sync.RWMutex
	interval    time.Duration
	sem         chan struct{}
	done        chan struct{}
	wg          sync.WaitGroup
	pid         int // ICMP identifier — process PID, shared across all probes
}

// NewMonitor creates a new ICMP monitor.
// Requires CAP_NET_RAW (Linux) or root (macOS) to open raw ICMP sockets.
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
		pid:         os.Getpid() & 0xffff,
	}, nil
}

// Reload re-reads the targets file atomically. Called on SIGHUP.
func (m *Monitor) Reload() {
	targets, err := loadTargets(m.targetsFile)
	if err != nil {
		slog.Error("ICMP monitor reload failed", "error", err)
		return
	}
	m.mu.Lock()
	m.targets = targets
	m.mu.Unlock()
	slog.Info("ICMP monitor reloaded targets", "count", len(targets), "file", m.targetsFile)
}

// Start begins the ICMP monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	slog.Info("ICMP monitor started", "targets", count, "probe", m.probe)

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

// Stop halts the ICMP monitoring and waits for all in-flight probes to finish.
func (m *Monitor) Stop() {
	close(m.done)
	m.wg.Wait()
	slog.Info("ICMP monitor stopped")
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
			m.pingHost(t)
		}()
	}
}

// pingHost sends Count ICMP echo requests to target.Host and records metrics.
func (m *Monitor) pingHost(target Target) {
	count := target.Count
	if count < 1 {
		count = 5
	}

	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		slog.Error("ICMP: failed to open raw socket", "host", target.Host, "error", err)
		icmpPacketLoss.With(prometheus.Labels{"probe": m.probe, "host": target.Host}).Set(1.0)
		return
	}
	defer conn.Close()

	dst, err := net.ResolveIPAddr("ip4", target.Host)
	if err != nil {
		slog.Error("ICMP: failed to resolve host", "host", target.Host, "error", err)
		icmpPacketLoss.With(prometheus.Labels{"probe": m.probe, "host": target.Host}).Set(1.0)
		return
	}

	sent := 0
	received := 0

	for seq := 0; seq < count; seq++ {
		rtt, ok := m.sendPing(conn, dst, seq)
		sent++
		if ok {
			received++
			icmpRTT.With(prometheus.Labels{
				"probe": m.probe,
				"host":  target.Host,
			}).Observe(rtt.Seconds())
		}
		if seq < count-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}

	loss := float64(sent-received) / float64(sent)
	icmpPacketLoss.With(prometheus.Labels{
		"probe": m.probe,
		"host":  target.Host,
	}).Set(loss)
}

// sendPing sends a single ICMP echo request and returns the RTT.
func (m *Monitor) sendPing(conn *icmp.PacketConn, dst *net.IPAddr, seq int) (time.Duration, bool) {
	// Build echo request with 8-byte payload carrying send timestamp.
	payload := make([]byte, 8)
	now := time.Now()
	binary.BigEndian.PutUint64(payload, uint64(now.UnixNano()))

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   m.pid,
			Seq:  seq,
			Data: payload,
		},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return 0, false
	}

	if err := conn.SetDeadline(time.Now().Add(1 * time.Second)); err != nil {
		return 0, false
	}

	sendTime := time.Now()
	if _, err := conn.WriteTo(wb, dst); err != nil {
		return 0, false
	}

	// Read replies; discard any that don't match our ID+seq.
	rb := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(rb)
		if err != nil {
			// Timeout or deadline exceeded — counts as lost.
			return 0, false
		}
		rtt := time.Since(sendTime)

		rm, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), rb[:n])
		if err != nil {
			continue
		}
		if rm.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		if !ok || echo.ID != m.pid || echo.Seq != seq {
			continue
		}
		return rtt, true
	}
}

// --- config loading ---

type icmpYAMLConfig struct {
	Targets []icmpYAMLTarget `yaml:"targets"`
}

type icmpYAMLTarget struct {
	Host  string `yaml:"host"`
	Count int    `yaml:"count"`
}

func loadTargets(file string) ([]Target, error) {
	if config.IsYAML(file) {
		return loadTargetsYAML(file)
	}
	// Legacy .txt path: <host> [count]
	rows, err := config.LoadLines(file, 1)
	if err != nil {
		return nil, err
	}
	targets := make([]Target, 0, len(rows))
	for _, parts := range rows {
		t := Target{Host: parts[0], Count: 5}
		targets = append(targets, t)
	}
	return targets, nil
}

func loadTargetsYAML(file string) ([]Target, error) {
	var cfg icmpYAMLConfig
	if err := config.DecodeYAML(file, &cfg); err != nil {
		return nil, err
	}
	var errs []string
	for i, t := range cfg.Targets {
		if t.Host == "" {
			errs = append(errs, fmt.Sprintf("target[%d]: host is required", i))
		}
		if t.Count < 0 {
			errs = append(errs, fmt.Sprintf("target[%d]: count must be >= 0", i))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", file, strings.Join(errs, "; "))
	}
	targets := make([]Target, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		count := t.Count
		if count == 0 {
			count = 5
		}
		targets = append(targets, Target{Host: t.Host, Count: count})
	}
	return targets, nil
}

// classifyError is kept for symmetry with other monitors but unused externally.
func classifyError(err error) string {
	s := err.Error()
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline") {
		return "timeout"
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return "timeout"
	}
	return "other"
}
