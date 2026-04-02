package dns

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// startDNSServer starts a UDP DNS server on a free port and returns
// its address ("127.0.0.1:PORT") and a shutdown function.
func startDNSServer(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()

	srv := &dns.Server{
		PacketConn: pc,
		Net:        "udp",
		Handler:    dns.HandlerFunc(handler),
	}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() {
		if err := srv.ActivateAndServe(); err != nil {
			// Ignore errors after shutdown.
		}
	}()
	<-ready
	t.Cleanup(func() { srv.Shutdown() })
	return addr
}

func writeTempTargets(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "dns_targets_*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
	return f.Name()
}

func newTestMonitor(t *testing.T, probe, targetsContent string) *Monitor {
	t.Helper()
	f := writeTempTargets(t, targetsContent)
	m, err := NewMonitor(f, probe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// histSampleCount returns the sample_count for a histogram with the given labels
// by gathering from the default registry. HistogramVec.With() returns an Observer,
// not a Collector, so testutil.ToFloat64 cannot be used directly.
func histSampleCount(t *testing.T, metricName string, labels map[string]string) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			if dtoLabelsMatch(m.GetLabel(), labels) {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func dtoLabelsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	got := make(map[string]string, len(pairs))
	for _, p := range pairs {
		got[p.GetName()] = p.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func successLabels(probe, domain, rtype, resolver string) map[string]string {
	return map[string]string{
		"probe": probe, "domain": domain, "record_type": rtype, "resolver": resolver,
	}
}

func failureLabels(probe, domain, rtype, resolver, errType string) map[string]string {
	return map[string]string{
		"probe": probe, "domain": domain, "record_type": rtype,
		"resolver": resolver, "error_type": errType,
	}
}

func TestCheckDNS_SuccessfulARecord(t *testing.T) {
	addr := startDNSServer(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("93.184.216.34"),
		})
		w.WriteMsg(m)
	})

	probe := "test_dns_ok"
	target := Target{Domain: "example.com", RecordType: "A", Resolver: addr}
	m := newTestMonitor(t, probe, fmt.Sprintf("example.com A %s\n", addr))

	sl := successLabels(probe, "example.com", "A", addr)
	before := testutil.ToFloat64(dnsResolutionSuccess.With(sl))
	m.checkDNS(target)
	after := testutil.ToFloat64(dnsResolutionSuccess.With(sl))

	if after-before != 1 {
		t.Fatalf("expected 1 success increment, got %f", after-before)
	}
}

func TestCheckDNS_InvalidRecordTypeDefaultsToA(t *testing.T) {
	var queriedType uint16
	addr := startDNSServer(t, func(w dns.ResponseWriter, r *dns.Msg) {
		queriedType = r.Question[0].Qtype
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("1.2.3.4"),
		})
		w.WriteMsg(m)
	})

	probe := "test_dns_badtype"
	target := Target{Domain: "example.com", RecordType: "BOGUSTYPE", Resolver: addr}
	m := newTestMonitor(t, probe, fmt.Sprintf("example.com BOGUSTYPE %s\n", addr))
	m.checkDNS(target) // must not panic

	if queriedType != dns.TypeA {
		t.Fatalf("expected fallback to TypeA (1), got %d", queriedType)
	}
}

func TestCheckDNS_NXDOMAIN(t *testing.T) {
	addr := startDNSServer(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeNameError // NXDOMAIN
		w.WriteMsg(m)
	})

	probe := "test_dns_nxdomain"
	target := Target{Domain: "does-not-exist.example", RecordType: "A", Resolver: addr}
	m := newTestMonitor(t, probe, fmt.Sprintf("does-not-exist.example A %s\n", addr))

	fl := failureLabels(probe, "does-not-exist.example", "A", addr, "nxdomain")
	before := testutil.ToFloat64(dnsResolutionFailure.With(fl))
	m.checkDNS(target)
	after := testutil.ToFloat64(dnsResolutionFailure.With(fl))

	if after-before != 1 {
		t.Fatalf("expected 1 nxdomain failure, got %f", after-before)
	}
}

func TestCheckDNS_SERVFAIL(t *testing.T) {
	addr := startDNSServer(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeServerFailure
		w.WriteMsg(m)
	})

	probe := "test_dns_servfail"
	target := Target{Domain: "example.com", RecordType: "A", Resolver: addr}
	m := newTestMonitor(t, probe, fmt.Sprintf("example.com A %s\n", addr))

	fl := failureLabels(probe, "example.com", "A", addr, "servfail")
	before := testutil.ToFloat64(dnsResolutionFailure.With(fl))
	m.checkDNS(target)
	after := testutil.ToFloat64(dnsResolutionFailure.With(fl))

	if after-before != 1 {
		t.Fatalf("expected 1 servfail failure, got %f", after-before)
	}
}

func TestCheckDNS_RTTRecorded(t *testing.T) {
	addr := startDNSServer(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("1.2.3.4"),
		})
		w.WriteMsg(m)
	})

	probe := "test_dns_rtt"
	target := Target{Domain: "example.com", RecordType: "A", Resolver: addr}
	m := newTestMonitor(t, probe, fmt.Sprintf("example.com A %s\n", addr))

	sl := successLabels(probe, "example.com", "A", addr)
	before := histSampleCount(t, "monitoring_dns_resolution_time_seconds", sl)
	m.checkDNS(target)
	after := histSampleCount(t, "monitoring_dns_resolution_time_seconds", sl)

	if after-before != 1 {
		t.Fatalf("expected 1 histogram observation, got %d", after-before)
	}
}

func TestReload_UpdatesTargets(t *testing.T) {
	f := writeTempTargets(t, "example.com A 8.8.8.8\n")
	m, err := NewMonitor(f, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.targets) != 1 {
		t.Fatalf("expected 1 target before reload, got %d", len(m.targets))
	}

	if err := os.WriteFile(f, []byte("example.com A 8.8.8.8\nexample.org AAAA 8.8.4.4\n"), 0644); err != nil {
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

func TestCanonicalAnswerSet_SortsAndJoins(t *testing.T) {
	cases := []struct {
		input []string
		want  string
	}{
		{[]string{"b", "a", "c"}, "a\nb\nc"},
		{[]string{"x"}, "x"},
		{[]string{}, ""},
	}
	for _, c := range cases {
		got := canonicalAnswerSet(c.input)
		if got != c.want {
			t.Errorf("canonicalAnswerSet(%v) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestCheckDNSCompare_Agreement(t *testing.T) {
	handler := func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("1.2.3.4"),
		})
		w.WriteMsg(m)
	}
	addr1 := startDNSServer(t, handler)
	addr2 := startDNSServer(t, handler)

	probe := "test_cmp_agree"
	ct := CompareTarget{
		Domain:     "example.com",
		RecordType: "A",
		Resolvers:  []string{addr1, addr2},
	}
	m := newTestMonitor(t, probe, fmt.Sprintf("example.com A %s\n", addr1))
	m.checkDNSCompare(ct)

	labels := prometheus.Labels{"probe": probe, "domain": "example.com", "record_type": "A"}
	got := testutil.ToFloat64(dnsResolverDisagreement.With(labels))
	if got != 0.0 {
		t.Fatalf("expected disagreement=0, got %f", got)
	}
}

func TestCheckDNSCompare_Disagreement(t *testing.T) {
	addr1 := startDNSServer(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("1.1.1.1"),
		})
		w.WriteMsg(m)
	})
	addr2 := startDNSServer(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("2.2.2.2"),
		})
		w.WriteMsg(m)
	})

	probe := "test_cmp_disagree"
	ct := CompareTarget{
		Domain:     "split.example",
		RecordType: "A",
		Resolvers:  []string{addr1, addr2},
	}
	m := newTestMonitor(t, probe, fmt.Sprintf("split.example A %s\n", addr1))
	m.checkDNSCompare(ct)

	labels := prometheus.Labels{"probe": probe, "domain": "split.example", "record_type": "A"}
	got := testutil.ToFloat64(dnsResolverDisagreement.With(labels))
	if got != 1.0 {
		t.Fatalf("expected disagreement=1, got %f", got)
	}
}

func TestLoadTargetsYAML_MultiResolver(t *testing.T) {
	content := `targets:
  - domain: example.com
    record_type: A
    resolvers:
      - 8.8.8.8
      - 1.1.1.1
`
	f := writeTempTargets(t, content)
	// rename so config.IsYAML returns true
	yamlFile := f + ".yaml"
	if err := os.Rename(f, yamlFile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(yamlFile) })

	targets, compareTargets, err := loadTargets(yamlFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("expected 0 single-resolver targets, got %d", len(targets))
	}
	if len(compareTargets) != 1 {
		t.Fatalf("expected 1 compare target, got %d", len(compareTargets))
	}
	if len(compareTargets[0].Resolvers) != 2 {
		t.Fatalf("expected 2 resolvers, got %d", len(compareTargets[0].Resolvers))
	}
}

func TestLoadTargetsYAML_BothResolverAndResolvers_Error(t *testing.T) {
	content := `targets:
  - domain: example.com
    record_type: A
    resolver: 8.8.8.8
    resolvers:
      - 8.8.8.8
      - 1.1.1.1
`
	f := writeTempTargets(t, content)
	yamlFile := f + ".yaml"
	if err := os.Rename(f, yamlFile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(yamlFile) })

	_, _, err := loadTargets(yamlFile)
	if err == nil {
		t.Fatal("expected error for both resolver and resolvers, got nil")
	}
}

func TestLoadTargetsYAML_OneResolver_Error(t *testing.T) {
	content := `targets:
  - domain: example.com
    record_type: A
    resolvers:
      - 8.8.8.8
`
	f := writeTempTargets(t, content)
	yamlFile := f + ".yaml"
	if err := os.Rename(f, yamlFile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(yamlFile) })

	_, _, err := loadTargets(yamlFile)
	if err == nil {
		t.Fatal("expected error for resolvers with only 1 entry, got nil")
	}
}

func TestClassifyRcode(t *testing.T) {
	cases := []struct{ rcode int; want string }{
		{dns.RcodeNameError, "nxdomain"},
		{dns.RcodeServerFailure, "servfail"},
		{dns.RcodeRefused, "refused"},
		{dns.RcodeNotImplemented, "rcode_4"},
	}
	for _, c := range cases {
		got := classifyRcode(c.rcode)
		if got != c.want {
			t.Errorf("classifyRcode(%d) = %q, want %q", c.rcode, got, c.want)
		}
	}
}
