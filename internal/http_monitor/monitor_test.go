package http_monitor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// histSampleCount returns the sample_count for a histogram with the given labels.
// HistogramVec.With() returns Observer (not Collector), so testutil.ToFloat64 won't work.
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

func writeTempTargets(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "http_targets_*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
	return f.Name()
}

func newTestMonitor(t *testing.T, probe string) *Monitor {
	t.Helper()
	// Empty targets file — tests call checkHTTP directly with explicit targets.
	f := writeTempTargets(t, "")
	m, err := NewMonitor(f, probe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCheckHTTP_Success200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probe := "test_http_200"
	m := newTestMonitor(t, probe)
	target := Target{URL: srv.URL + "/health", Method: "GET"}
	normURL := normalizeURL(target.URL)
	labels := map[string]string{
		"probe":       probe,
		"url":         normURL,
		"method":      "GET",
		"status_code": "200",
	}

	before := histSampleCount(t, "monitoring_http_request_duration_seconds", labels)
	m.checkHTTP(target)
	after := histSampleCount(t, "monitoring_http_request_duration_seconds", labels)

	if after-before != 1 {
		t.Fatalf("expected 1 duration observation, got %d", after-before)
	}
}

func TestCheckHTTP_4xxIncrementsResponseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	probe := "test_http_404"
	m := newTestMonitor(t, probe)
	target := Target{URL: srv.URL + "/missing", Method: "GET"}
	normURL := normalizeURL(target.URL)
	labels := map[string]string{
		"probe":       probe,
		"url":         normURL,
		"method":      "GET",
		"status_code": "404",
	}

	before := testutil.ToFloat64(httpResponseErrors.With(labels))
	m.checkHTTP(target)
	after := testutil.ToFloat64(httpResponseErrors.With(labels))

	if after-before != 1 {
		t.Fatalf("expected 1 response error increment for 404, got %f", after-before)
	}
}

func TestCheckHTTP_5xxIncrementsResponseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	probe := "test_http_500"
	m := newTestMonitor(t, probe)
	target := Target{URL: srv.URL + "/error", Method: "GET"}
	normURL := normalizeURL(target.URL)
	labels := map[string]string{
		"probe":       probe,
		"url":         normURL,
		"method":      "GET",
		"status_code": "500",
	}

	before := testutil.ToFloat64(httpResponseErrors.With(labels))
	m.checkHTTP(target)
	after := testutil.ToFloat64(httpResponseErrors.With(labels))

	if after-before != 1 {
		t.Fatalf("expected 1 response error increment for 500, got %f", after-before)
	}
}

func TestCheckHTTP_ConnectionRefused(t *testing.T) {
	// Use a server that immediately closes so the port is free.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL + "/gone"
	srv.Close() // Now nothing listens there.

	probe := "test_http_refused"
	m := newTestMonitor(t, probe)
	target := Target{URL: url, Method: "GET"}
	normURL := normalizeURL(url)

	beforeOther := testutil.ToFloat64(httpRequestErrors.With(map[string]string{
		"probe": probe, "url": normURL, "method": "GET", "error_type": "refused",
	}))
	beforeTimeout := testutil.ToFloat64(httpRequestErrors.With(map[string]string{
		"probe": probe, "url": normURL, "method": "GET", "error_type": "timeout",
	}))

	m.checkHTTP(target)

	afterOther := testutil.ToFloat64(httpRequestErrors.With(map[string]string{
		"probe": probe, "url": normURL, "method": "GET", "error_type": "refused",
	}))
	afterTimeout := testutil.ToFloat64(httpRequestErrors.With(map[string]string{
		"probe": probe, "url": normURL, "method": "GET", "error_type": "timeout",
	}))

	// Should have incremented exactly one error type counter.
	total := (afterOther - beforeOther) + (afterTimeout - beforeTimeout)
	if total != 1 {
		t.Fatalf("expected exactly 1 request error, got %f", total)
	}
}

func TestCheckHTTP_QueryParamsStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probe := "test_http_querystrip"
	m := newTestMonitor(t, probe)

	// URL with query params — the label stored should NOT include the query string.
	rawURL := srv.URL + "/search?q=secret&page=1"
	target := Target{URL: rawURL, Method: "GET"}
	norm := normalizeURL(rawURL)

	if norm == rawURL {
		t.Fatal("normalizeURL did not strip query params")
	}

	labels := map[string]string{
		"probe":       probe,
		"url":         norm,
		"method":      "GET",
		"status_code": "200",
	}
	before := histSampleCount(t, "monitoring_http_request_duration_seconds", labels)
	m.checkHTTP(target)
	after := histSampleCount(t, "monitoring_http_request_duration_seconds", labels)

	if after-before != 1 {
		t.Fatal("expected metric recorded under stripped URL label")
	}
}

func TestCheckHTTP_DisableKeepAlives_ConnectFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probe := "test_http_keepalive"
	m := newTestMonitor(t, probe)
	target := Target{URL: srv.URL + "/", Method: "GET"}
	normURL := normalizeURL(target.URL)

	// Make two consecutive requests. With DisableKeepAlives=true, both should
	// record connect timing (a new TCP connection per request).
	phaseLabels := map[string]string{"probe": probe, "url": normURL}

	before := histSampleCount(t, "monitoring_http_connect_duration_seconds", phaseLabels)
	m.checkHTTP(target)
	mid := histSampleCount(t, "monitoring_http_connect_duration_seconds", phaseLabels)
	m.checkHTTP(target)
	after := histSampleCount(t, "monitoring_http_connect_duration_seconds", phaseLabels)

	if mid-before != 1 {
		t.Fatal("first request did not record connect duration")
	}
	if after-mid != 1 {
		t.Fatal("second request did not record connect duration (keep-alives may be active)")
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"http://example.com/path?q=1&x=2", "http://example.com/path"},
		{"http://example.com/path", "http://example.com/path"},
		{"http://example.com/path#fragment", "http://example.com/path"},
		{"not a url %%", "not a url %%"}, // parse error → return raw
	}
	for _, c := range cases {
		got := normalizeURL(c.raw)
		if got != c.want {
			t.Errorf("normalizeURL(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCheckHTTP_CustomHeaders(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probe := "test_http_headers"
	m := newTestMonitor(t, probe)
	target := Target{
		URL:     srv.URL + "/api",
		Method:  "GET",
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	}
	m.checkHTTP(target)

	if receivedAuth != "Bearer test-token" {
		t.Fatalf("expected Authorization header 'Bearer test-token', got %q", receivedAuth)
	}
}

func TestReload_UpdatesTargets(t *testing.T) {
	f := writeTempTargets(t, "http://example.com GET\n")
	m, err := NewMonitor(f, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.targets) != 1 {
		t.Fatalf("expected 1 target before reload, got %d", len(m.targets))
	}

	if err := os.WriteFile(f, []byte("http://example.com GET\nhttp://example.org POST\n"), 0644); err != nil {
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
