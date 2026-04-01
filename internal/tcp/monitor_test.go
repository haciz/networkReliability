package tcp

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// freePort binds on :0, grabs the OS-assigned port, and closes the listener.
// The port is available for the caller immediately after return on most platforms.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func writeTempTargets(t *testing.T, lines string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "tcp_targets_*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(lines)
	f.Close()
	return f.Name()
}

func newTestMonitor(t *testing.T, probe, content string) *Monitor {
	t.Helper()
	f := writeTempTargets(t, content)
	m, err := NewMonitor(f, probe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheckTCP_Success(t *testing.T) {
	// Start a real TCP listener so the connection succeeds.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	addr := l.Addr().(*net.TCPAddr)
	probe := "test_tcp_success"
	m := newTestMonitor(t, probe, fmt.Sprintf("127.0.0.1 %d\n", addr.Port))
	target := Target{Host: "127.0.0.1", Port: strconv.Itoa(addr.Port)}

	before := testutil.ToFloat64(tcpConnectionSuccess.With(successLabels(probe, target)))
	m.checkTCP(target)
	after := testutil.ToFloat64(tcpConnectionSuccess.With(successLabels(probe, target)))

	if after-before != 1 {
		t.Fatalf("expected 1 success increment, got %f", after-before)
	}
}

func TestCheckTCP_Refused(t *testing.T) {
	// Grab a free port, close it, then try to connect → ECONNREFUSED.
	port := freePort(t)
	probe := "test_tcp_refused"
	target := Target{Host: "127.0.0.1", Port: strconv.Itoa(port)}
	m := newTestMonitor(t, probe, fmt.Sprintf("127.0.0.1 %d\n", port))

	before := testutil.ToFloat64(tcpConnectionFailure.With(failureLabels(probe, target, "refused")))
	m.checkTCP(target)
	after := testutil.ToFloat64(tcpConnectionFailure.With(failureLabels(probe, target, "refused")))

	if after-before != 1 {
		t.Fatalf("expected 1 refused failure, got %f", after-before)
	}
}

func TestLoadTargets_RejectsNonNumericPort(t *testing.T) {
	f := writeTempTargets(t, "192.168.1.1 notaport\n192.168.1.2 443\n")
	m, err := NewMonitor(f, "test", time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the valid target (port 443) should be loaded.
	if len(m.targets) != 1 {
		t.Fatalf("expected 1 target after rejecting bad port, got %d", len(m.targets))
	}
	if m.targets[0].Port != "443" {
		t.Fatalf("expected port 443, got %s", m.targets[0].Port)
	}
}

func TestReload_UpdatesTargets(t *testing.T) {
	f := writeTempTargets(t, "192.168.1.1 443\n")
	m, err := NewMonitor(f, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.targets) != 1 {
		t.Fatalf("expected 1 target before reload, got %d", len(m.targets))
	}

	// Overwrite file with 2 targets.
	if err := os.WriteFile(f, []byte("192.168.1.1 443\n192.168.1.2 80\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m.Reload()

	m.mu.RLock()
	count := len(m.targets)
	m.mu.RUnlock()
	if count != 2 {
		t.Fatalf("expected 2 targets after reload, got %d", count)
	}
}

func TestClassifyError_Timeout(t *testing.T) {
	err := fmt.Errorf("dial tcp: i/o timeout")
	if got := classifyError(err); got != "timeout" {
		t.Fatalf("expected timeout, got %s", got)
	}
}

func TestClassifyError_Refused(t *testing.T) {
	err := fmt.Errorf("dial tcp: connect: connection refused")
	if got := classifyError(err); got != "refused" {
		t.Fatalf("expected refused, got %s", got)
	}
}

func TestClassifyError_Other(t *testing.T) {
	err := fmt.Errorf("some unknown error")
	if got := classifyError(err); got != "other" {
		t.Fatalf("expected other, got %s", got)
	}
}

// helpers to build label maps matching the metric definitions

func successLabels(probe string, t Target) map[string]string {
	return map[string]string{"probe": probe, "host": t.Host, "port": t.Port}
}

func failureLabels(probe string, t Target, errType string) map[string]string {
	return map[string]string{"probe": probe, "host": t.Host, "port": t.Port, "error_type": errType}
}
