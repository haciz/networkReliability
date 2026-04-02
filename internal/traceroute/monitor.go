// Package traceroute implements periodic path-tracing to network targets.
// It sends ICMP echo requests with increasing TTL values and records per-hop
// round-trip times. When a router's TTL expires it returns ICMP Time Exceeded,
// revealing the hop IP and RTT.
//
// Requires CAP_NET_RAW (Linux) or root (macOS). In Docker, add:
//
//	cap_add: [NET_RAW]
package traceroute

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
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
	// tracerouteHopRTT records the average RTT to each hop in the current path.
	tracerouteHopRTT = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "traceroute",
			Name:      "hop_rtt_seconds",
			Help:      "Average RTT to this hop number in the most recent traceroute (seconds)",
		},
		[]string{"probe", "target", "hop"},
	)

	// tracerouteHopInfo is an info-style metric (always value 1) that binds a
	// hop number to its current IP address. Use with group_left(hop_ip) in
	// PromQL to annotate hop RTT graphs.
	tracerouteHopInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "traceroute",
			Name:      "hop_ip_info",
			Help:      "Info metric: 1 for the current IP address at each hop",
		},
		[]string{"probe", "target", "hop", "hop_ip"},
	)

	tracerouteTotalHops = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "monitoring",
			Subsystem: "traceroute",
			Name:      "hops_total",
			Help:      "Number of hops to reach the destination in the most recent traceroute",
		},
		[]string{"probe", "target"},
	)
)

// Target is one destination to trace.
type Target struct {
	Host         string
	MaxHops      int // default 30
	ProbesPerHop int // default 3
}

// Monitor periodically runs traceroutes to a set of targets.
type Monitor struct {
	probe       string
	targetsFile string
	targets     []Target
	mu          sync.RWMutex
	interval    time.Duration
	sem         chan struct{}
	done        chan struct{}
	wg          sync.WaitGroup
}

// NewMonitor creates a new traceroute monitor.
// Requires CAP_NET_RAW.
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
		sem:         make(chan struct{}, 3), // traceroute is expensive; limit concurrency
		done:        make(chan struct{}),
	}, nil
}

// Reload re-reads the targets file atomically. Called on SIGHUP.
func (m *Monitor) Reload() {
	targets, err := loadTargets(m.targetsFile)
	if err != nil {
		slog.Error("traceroute monitor reload failed", "error", err)
		return
	}
	m.mu.Lock()
	m.targets = targets
	m.mu.Unlock()
	slog.Info("traceroute monitor reloaded targets", "count", len(targets), "file", m.targetsFile)
}

// Start begins the traceroute monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	slog.Info("traceroute monitor started", "targets", count, "probe", m.probe)

	m.traceAll()
	for {
		select {
		case <-ticker.C:
			m.traceAll()
		case <-m.done:
			return
		}
	}
}

// Stop halts the monitor and drains in-flight traces.
func (m *Monitor) Stop() {
	close(m.done)
	m.wg.Wait()
	slog.Info("traceroute monitor stopped")
}

func (m *Monitor) traceAll() {
	m.mu.RLock()
	targets := make([]Target, len(m.targets))
	copy(targets, m.targets)
	m.mu.RUnlock()

	for _, t := range targets {
		t := t
		select {
		case m.sem <- struct{}{}:
		case <-m.done:
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() { <-m.sem }()
			m.traceTarget(t)
		}()
	}
}

// traceTarget runs a full traceroute to target.Host and updates metrics.
func (m *Monitor) traceTarget(target Target) {
	maxHops := target.MaxHops
	if maxHops < 1 {
		maxHops = 30
	}
	probesPerHop := target.ProbesPerHop
	if probesPerHop < 1 {
		probesPerHop = 3
	}

	// Per-target ICMP ID derived from FNV hash of the host name. Avoids
	// collision when multiple targets are probed concurrently (unlike a single
	// PID-based ID shared across all goroutines).
	h := fnv.New32a()
	h.Write([]byte(target.Host))
	id := int(h.Sum32() & 0xffff)

	// Resolve with a timeout so a slow DNS server can't block the goroutine
	// indefinitely. net.ResolveIPAddr has no context parameter.
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resolveCancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, target.Host)
	if err != nil {
		slog.Error("traceroute: cannot resolve host", "host", target.Host, "error", err)
		return
	}
	var dst *net.IPAddr
	for _, a := range addrs {
		if a.IP.To4() != nil {
			dst = &net.IPAddr{IP: a.IP}
			break
		}
	}
	if dst == nil {
		slog.Error("traceroute: no IPv4 address for host", "host", target.Host)
		return
	}

	// Remove stale hop_ip_info series from the previous trace. Without this,
	// path changes accumulate unbounded {hop_ip} label values over time.
	tracerouteHopInfo.DeletePartialMatch(prometheus.Labels{
		"probe":  m.probe,
		"target": target.Host,
	})

	// One raw socket per trace run.
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		slog.Error("traceroute: failed to open raw socket", "host", target.Host, "error", err)
		return
	}
	defer conn.Close()

	p4 := ipv4.NewPacketConn(conn)
	if err := p4.SetControlMessage(ipv4.FlagTTL, true); err != nil {
		slog.Error("traceroute: SetControlMessage failed", "error", err)
		return
	}

	reachedDst := false

	for hop := 1; hop <= maxHops; hop++ {
		hopLabel := fmt.Sprintf("%d", hop)
		hopIP, avgRTT, reached := m.probeHop(conn, p4, dst, hop, probesPerHop, id)

		if hopIP != "" {
			tracerouteHopRTT.With(prometheus.Labels{
				"probe":  m.probe,
				"target": target.Host,
				"hop":    hopLabel,
			}).Set(avgRTT)

			tracerouteHopInfo.With(prometheus.Labels{
				"probe":  m.probe,
				"target": target.Host,
				"hop":    hopLabel,
				"hop_ip": hopIP,
			}).Set(1)
		}

		if reached {
			tracerouteTotalHops.With(prometheus.Labels{
				"probe":  m.probe,
				"target": target.Host,
			}).Set(float64(hop))
			reachedDst = true
			break
		}
	}

	if !reachedDst {
		slog.Warn("traceroute: destination not reached within max hops",
			"host", target.Host, "max_hops", maxHops)
	}
}

// probeHop sends probesPerHop ICMP echo requests with TTL=hop to dst and
// collects replies. Returns the responding IP, average RTT across successful
// probes, and whether the destination itself was reached.
// id is the per-trace ICMP identifier (derived from target host hash).
func (m *Monitor) probeHop(
	conn *icmp.PacketConn,
	p4 *ipv4.PacketConn,
	dst *net.IPAddr,
	hop, probesPerHop, id int,
) (hopIP string, avgRTT float64, reached bool) {
	var rtts []float64
	var respondingIP string

	for probe := 0; probe < probesPerHop; probe++ {
		seq := (hop-1)*probesPerHop + probe

		// Embed send timestamp in payload for latency measurement.
		payload := make([]byte, 8)
		binary.BigEndian.PutUint64(payload, uint64(time.Now().UnixNano()))

		msg := icmp.Message{
			Type: ipv4.ICMPTypeEcho,
			Code: 0,
			Body: &icmp.Echo{
				ID:   id,
				Seq:  seq,
				Data: payload,
			},
		}
		wb, err := msg.Marshal(nil)
		if err != nil {
			continue
		}

		cm := &ipv4.ControlMessage{TTL: hop}
		sendTime := time.Now()

		if _, err := p4.WriteTo(wb, cm, dst); err != nil {
			slog.Debug("traceroute: write failed", "hop", hop, "error", err)
			continue
		}

		// Read replies with a per-probe timeout.
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		rb := make([]byte, 1500)

		for {
			n, peer, err := conn.ReadFrom(rb)
			if err != nil {
				// Timeout — this hop doesn't respond ("*").
				break
			}
			rtt := time.Since(sendTime)

			rm, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), rb[:n])
			if err != nil {
				continue
			}

			switch rm.Type {
			case ipv4.ICMPTypeEchoReply:
				echo, ok := rm.Body.(*icmp.Echo)
				if !ok || echo.ID != id || echo.Seq != seq {
					continue
				}
				respondingIP = peer.String()
				rtts = append(rtts, rtt.Seconds())
				reached = true

			case ipv4.ICMPTypeTimeExceeded:
				te, ok := rm.Body.(*icmp.TimeExceeded)
				if !ok {
					continue
				}
				// The Time Exceeded body contains the original IP header (20 bytes)
				// followed by the first 8 bytes of the original ICMP packet:
				// type(1) code(1) checksum(2) ID(2) Seq(2).
				if len(te.Data) < 28 {
					continue
				}
				origID := int(te.Data[24])<<8 | int(te.Data[25])
				origSeq := int(te.Data[26])<<8 | int(te.Data[27])
				if origID != id || origSeq != seq {
					continue
				}
				respondingIP = peer.String()
				rtts = append(rtts, rtt.Seconds())
			}

			// Got a matching reply — move to next probe.
			break
		}
	}

	if len(rtts) == 0 {
		return "", 0, reached
	}

	sum := 0.0
	for _, r := range rtts {
		sum += r
	}
	return respondingIP, sum / float64(len(rtts)), reached
}

// --- config loading ---

type tracerouteYAMLConfig struct {
	Targets []tracerouteYAMLTarget `yaml:"targets"`
}

type tracerouteYAMLTarget struct {
	Host         string `yaml:"host"`
	MaxHops      int    `yaml:"max_hops"`
	ProbesPerHop int    `yaml:"probes_per_hop"`
}

func loadTargets(file string) ([]Target, error) {
	if !config.IsYAML(file) {
		return nil, fmt.Errorf("traceroute monitor requires a YAML targets file, got: %s", file)
	}
	var cfg tracerouteYAMLConfig
	if err := config.DecodeYAML(file, &cfg); err != nil {
		return nil, err
	}
	var errs []string
	for i, t := range cfg.Targets {
		if t.Host == "" {
			errs = append(errs, fmt.Sprintf("target[%d]: host is required", i))
		}
		if t.MaxHops < 0 || t.MaxHops > 64 {
			errs = append(errs, fmt.Sprintf("target[%d]: max_hops must be 1-64", i))
		}
		if t.ProbesPerHop < 0 || t.ProbesPerHop > 10 {
			errs = append(errs, fmt.Sprintf("target[%d]: probes_per_hop must be 1-10", i))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", file, strings.Join(errs, "; "))
	}
	targets := make([]Target, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		targets = append(targets, Target{
			Host:         t.Host,
			MaxHops:      t.MaxHops,
			ProbesPerHop: t.ProbesPerHop,
		})
	}
	return targets, nil
}
