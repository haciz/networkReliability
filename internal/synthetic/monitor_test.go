package synthetic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// writeTempFlows creates a temporary YAML file with the given content and
// returns its path. The file is automatically cleaned up by t.Cleanup.
func writeTempFlows(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "synthetic_flows_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
	return f.Name()
}

func newTestMonitor(t *testing.T, probe, flowsContent string) *Monitor {
	t.Helper()
	f := writeTempFlows(t, flowsContent)
	m, err := NewMonitor(f, probe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- expandVars ---

func TestExpandVars_LocalOverridesEnv(t *testing.T) {
	t.Setenv("MY_VAR", "from-env")
	vars := map[string]string{"MY_VAR": "from-local"}
	got := expandVars("${MY_VAR}", vars)
	if got != "from-local" {
		t.Fatalf("expected local value, got %q", got)
	}
}

func TestExpandVars_FallsBackToEnv(t *testing.T) {
	t.Setenv("ONLY_IN_ENV", "env-value")
	got := expandVars("${ONLY_IN_ENV}", map[string]string{})
	if got != "env-value" {
		t.Fatalf("expected env value, got %q", got)
	}
}

func TestExpandVars_Missing_Empty(t *testing.T) {
	got := expandVars("${NO_SUCH_VAR_XYZ}", map[string]string{})
	if got != "" {
		t.Fatalf("expected empty string for missing var, got %q", got)
	}
}

// --- extractJSONKey ---

func TestExtractJSONKey_TopLevel(t *testing.T) {
	body := []byte(`{"token": "abc123"}`)
	got, err := extractJSONKey(body, "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("expected 'abc123', got %q", got)
	}
}

func TestExtractJSONKey_Nested(t *testing.T) {
	body := []byte(`{"data": {"access_token": "xyz"}}`)
	got, err := extractJSONKey(body, "data.access_token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "xyz" {
		t.Fatalf("expected 'xyz', got %q", got)
	}
}

func TestExtractJSONKey_KeyNotFound(t *testing.T) {
	body := []byte(`{"other": "value"}`)
	_, err := extractJSONKey(body, "missing_key")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestExtractJSONKey_NotAnObject(t *testing.T) {
	body := []byte(`{"data": "not-an-object"}`)
	_, err := extractJSONKey(body, "data.token")
	if err == nil {
		t.Fatal("expected error when intermediate path is not an object")
	}
}

func TestExtractJSONKey_InvalidJSON(t *testing.T) {
	body := []byte(`not-json`)
	_, err := extractJSONKey(body, "key")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// --- loadFlows ---

func TestLoadFlows_Valid(t *testing.T) {
	content := `
flows:
  - name: test-flow
    steps:
      - name: step1
        url: https://example.com
        method: GET
`
	f := writeTempFlows(t, content)
	flows, err := loadFlows(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(flows) != 1 || flows[0].Name != "test-flow" {
		t.Fatalf("unexpected flows: %+v", flows)
	}
}

func TestLoadFlows_MissingFlowName(t *testing.T) {
	content := `
flows:
  - name: ""
    steps:
      - name: step1
        url: https://example.com
        method: GET
`
	f := writeTempFlows(t, content)
	_, err := loadFlows(f)
	if err == nil {
		t.Fatal("expected error for missing flow name")
	}
}

func TestLoadFlows_MissingStepURL(t *testing.T) {
	content := `
flows:
  - name: my-flow
    steps:
      - name: step1
        url: ""
        method: GET
`
	f := writeTempFlows(t, content)
	_, err := loadFlows(f)
	if err == nil {
		t.Fatal("expected error for missing step URL")
	}
}

func TestLoadFlows_NoSteps(t *testing.T) {
	content := `
flows:
  - name: empty-flow
    steps: []
`
	f := writeTempFlows(t, content)
	_, err := loadFlows(f)
	if err == nil {
		t.Fatal("expected error for flow with no steps")
	}
}

func TestLoadFlows_InvalidMethod(t *testing.T) {
	content := `
flows:
  - name: my-flow
    steps:
      - name: step1
        url: https://example.com
        method: INVALID
`
	f := writeTempFlows(t, content)
	_, err := loadFlows(f)
	if err == nil {
		t.Fatal("expected error for invalid method")
	}
}

// --- runFlow integration ---

func TestRunFlow_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"token": "abc"})
	}))
	defer srv.Close()

	probe := "test_synth_ok"
	content := `
flows:
  - name: test-flow
    steps:
      - name: fetch
        url: "` + srv.URL + `/health"
        method: GET
        expected_status: 200
`
	m := newTestMonitor(t, probe, content)

	labels := prometheus.Labels{"probe": probe, "flow": "test-flow"}
	before := testutil.ToFloat64(syntheticFlowSuccess.With(labels))
	m.runFlow(m.flows[0])
	after := testutil.ToFloat64(syntheticFlowSuccess.With(labels))

	if after-before != 1 {
		t.Fatalf("expected 1 success, got %f", after-before)
	}
}

func TestRunFlow_WrongStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	probe := "test_synth_wrong_status"
	content := `
flows:
  - name: fail-flow
    steps:
      - name: login
        url: "` + srv.URL + `/auth"
        method: POST
        expected_status: 200
`
	m := newTestMonitor(t, probe, content)

	labels := prometheus.Labels{"probe": probe, "flow": "fail-flow", "step": "login", "reason": "wrong_status"}
	before := testutil.ToFloat64(syntheticFlowFailure.With(labels))
	m.runFlow(m.flows[0])
	after := testutil.ToFloat64(syntheticFlowFailure.With(labels))

	if after-before != 1 {
		t.Fatalf("expected 1 wrong_status failure, got %f", after-before)
	}
}

func TestRunFlow_BodyMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "degraded"}`))
	}))
	defer srv.Close()

	probe := "test_synth_body"
	content := `
flows:
  - name: body-flow
    steps:
      - name: check
        url: "` + srv.URL + `/status"
        method: GET
        body_contains: "healthy"
`
	m := newTestMonitor(t, probe, content)

	labels := prometheus.Labels{"probe": probe, "flow": "body-flow", "step": "check", "reason": "body_mismatch"}
	before := testutil.ToFloat64(syntheticFlowFailure.With(labels))
	m.runFlow(m.flows[0])
	after := testutil.ToFloat64(syntheticFlowFailure.With(labels))

	if after-before != 1 {
		t.Fatalf("expected 1 body_mismatch failure, got %f", after-before)
	}
}

func TestRunFlow_ExtractAndInject(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"access_token": "secret-token"})
			return
		}
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probe := "test_synth_extract"
	content := `
flows:
  - name: extract-flow
    steps:
      - name: login
        url: "` + srv.URL + `/login"
        method: POST
        expected_status: 200
        extract:
          - name: token
            json_key: access_token
      - name: profile
        url: "` + srv.URL + `/me"
        method: GET
        headers:
          Authorization: "Bearer ${token}"
        expected_status: 200
`
	m := newTestMonitor(t, probe, content)
	m.runFlow(m.flows[0])

	if receivedAuth != "Bearer secret-token" {
		t.Fatalf("expected extracted token injected as Authorization header, got %q", receivedAuth)
	}
}
