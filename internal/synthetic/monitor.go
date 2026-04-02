// Package synthetic implements multi-step HTTP flow monitoring.
// Each flow is a sequence of HTTP requests where extracted values from one
// step can be injected as variables into subsequent steps.
package synthetic

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"monitoring-app/internal/config"
)

var (
	syntheticFlowDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "synthetic",
			Name:      "flow_duration_seconds",
			Help:      "Total duration of a complete synthetic flow in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "flow"},
	)

	syntheticFlowSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "synthetic",
			Name:      "flow_success_total",
			Help:      "Total number of fully successful synthetic flow executions",
		},
		[]string{"probe", "flow"},
	)

	syntheticFlowFailure = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "monitoring",
			Subsystem: "synthetic",
			Name:      "flow_failure_total",
			Help:      "Total number of failed synthetic flow executions",
		},
		// reason: transport | wrong_status | body_mismatch | extract_failed
		[]string{"probe", "flow", "step", "reason"},
	)

	syntheticStepDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "monitoring",
			Subsystem: "synthetic",
			Name:      "step_duration_seconds",
			Help:      "Duration of a single step within a synthetic flow in seconds",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"probe", "flow", "step"},
	)
)

// Extract describes how to pull a value out of a JSON response body.
type Extract struct {
	Name    string // variable name to store into
	JSONKey string // dot-notation path: "token" or "data.access_token"
}

// Step is one HTTP request within a flow.
type Step struct {
	Name           string
	URL            string
	Method         string
	Headers        map[string]string
	Body           string
	BodyType       string // "json" sets Content-Type: application/json
	ExpectedStatus int    // 0 means any 2xx is acceptable
	BodyContains   string // optional substring check on response body
	Extract        []Extract
}

// Flow is an ordered list of Steps.
type Flow struct {
	Name  string
	Steps []Step
}

// Monitor runs synthetic flows at a fixed interval.
type Monitor struct {
	probe      string
	configFile string
	flows      []Flow
	mu         sync.RWMutex
	interval   time.Duration
	client     *http.Client
	sem        chan struct{}
	done       chan struct{}
	wg         sync.WaitGroup
}

// NewMonitor creates a new synthetic flow monitor.
func NewMonitor(configFile, probe string, interval time.Duration) (*Monitor, error) {
	flows, err := loadFlows(configFile)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: false, // keep-alives useful across flow steps
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: false},
		},
	}
	return &Monitor{
		probe:      probe,
		configFile: configFile,
		flows:      flows,
		interval:   interval,
		client:     client,
		sem:        make(chan struct{}, 5),
		done:       make(chan struct{}),
	}, nil
}

// Reload re-reads the config file atomically. Called on SIGHUP.
func (m *Monitor) Reload() {
	flows, err := loadFlows(m.configFile)
	if err != nil {
		slog.Error("synthetic monitor reload failed", "error", err)
		return
	}
	m.mu.Lock()
	m.flows = flows
	m.mu.Unlock()
	slog.Info("synthetic monitor reloaded flows", "count", len(flows), "file", m.configFile)
}

// Start begins the synthetic monitoring loop.
func (m *Monitor) Start() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.mu.RLock()
	count := len(m.flows)
	m.mu.RUnlock()
	slog.Info("synthetic monitor started", "flows", count, "probe", m.probe)

	m.runAll()
	for {
		select {
		case <-ticker.C:
			m.runAll()
		case <-m.done:
			return
		}
	}
}

// Stop halts the monitor and drains in-flight flows.
func (m *Monitor) Stop() {
	close(m.done)
	m.wg.Wait()
	slog.Info("synthetic monitor stopped")
}

func (m *Monitor) runAll() {
	m.mu.RLock()
	flows := make([]Flow, len(m.flows))
	copy(flows, m.flows)
	m.mu.RUnlock()

	for _, f := range flows {
		f := f
		m.wg.Add(1)
		m.sem <- struct{}{}
		go func() {
			defer m.wg.Done()
			defer func() { <-m.sem }()
			m.runFlow(f)
		}()
	}
}

func (m *Monitor) runFlow(flow Flow) {
	// vars holds values extracted from step responses, available to later steps.
	vars := make(map[string]string)
	flowStart := time.Now()

	for _, step := range flow.Steps {
		if err := m.runStep(flow.Name, step, vars); err != nil {
			syntheticFlowFailure.With(prometheus.Labels{
				"probe":  m.probe,
				"flow":   flow.Name,
				"step":   step.Name,
				"reason": classifyStepError(err),
			}).Inc()
			slog.Warn("synthetic flow failed",
				"flow", flow.Name, "step", step.Name, "error", err)
			return
		}
	}

	flowDur := time.Since(flowStart)
	syntheticFlowDuration.With(prometheus.Labels{
		"probe": m.probe,
		"flow":  flow.Name,
	}).Observe(flowDur.Seconds())
	syntheticFlowSuccess.With(prometheus.Labels{
		"probe": m.probe,
		"flow":  flow.Name,
	}).Inc()

	slog.Debug("synthetic flow succeeded",
		"flow", flow.Name, "duration_ms", flowDur.Milliseconds())
}

// stepError carries a structured reason for classifying failure metrics.
type stepError struct {
	reason  string
	message string
}

func (e *stepError) Error() string { return e.message }

func classifyStepError(err error) string {
	if se, ok := err.(*stepError); ok {
		return se.reason
	}
	return "transport"
}

func (m *Monitor) runStep(flowName string, step Step, vars map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := expandVars(step.URL, vars)
	body := expandVars(step.Body, vars)

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, step.Method, url, bodyReader)
	if err != nil {
		return &stepError{reason: "transport", message: fmt.Sprintf("build request: %v", err)}
	}

	if step.BodyType == "json" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range step.Headers {
		req.Header.Set(k, expandVars(v, vars))
	}

	stepStart := time.Now()
	resp, err := m.client.Do(req)
	stepDur := time.Since(stepStart)

	syntheticStepDuration.With(prometheus.Labels{
		"probe": m.probe,
		"flow":  flowName,
		"step":  step.Name,
	}).Observe(stepDur.Seconds())

	if err != nil {
		return &stepError{reason: "transport", message: fmt.Sprintf("request: %v", err)}
	}
	defer resp.Body.Close()

	// Validate status code.
	wantStatus := step.ExpectedStatus
	if wantStatus == 0 {
		// Default: any 2xx.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &stepError{
				reason:  "wrong_status",
				message: fmt.Sprintf("expected 2xx, got %d", resp.StatusCode),
			}
		}
	} else if resp.StatusCode != wantStatus {
		return &stepError{
			reason:  "wrong_status",
			message: fmt.Sprintf("expected %d, got %d", wantStatus, resp.StatusCode),
		}
	}

	// Read body for content checks and variable extraction.
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return &stepError{reason: "transport", message: fmt.Sprintf("read body: %v", err)}
	}

	// Optional body substring check.
	if step.BodyContains != "" && !bytes.Contains(rawBody, []byte(step.BodyContains)) {
		return &stepError{
			reason:  "body_mismatch",
			message: fmt.Sprintf("body does not contain %q", step.BodyContains),
		}
	}

	// Variable extraction from JSON body.
	for _, ext := range step.Extract {
		val, err := extractJSONKey(rawBody, ext.JSONKey)
		if err != nil {
			return &stepError{
				reason:  "extract_failed",
				message: fmt.Sprintf("extract %q: %v", ext.JSONKey, err),
			}
		}
		vars[ext.Name] = val
	}

	return nil
}

// extractJSONKey extracts a value from a JSON body using a dot-notation path.
// e.g. "access_token" or "data.token".
func extractJSONKey(body []byte, path string) (string, error) {
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}

	parts := strings.Split(path, ".")
	cur := parsed
	for _, part := range parts {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("path %q: not an object at %q", path, part)
		}
		cur, ok = m[part]
		if !ok {
			return "", fmt.Errorf("path %q: key %q not found", path, part)
		}
	}

	switch v := cur.(type) {
	case string:
		return v, nil
	case float64:
		return fmt.Sprintf("%v", v), nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// expandVars replaces ${VAR} placeholders: first checks the local vars map
// (extracted values from previous steps), then falls back to os.Getenv.
func expandVars(s string, vars map[string]string) string {
	return os.Expand(s, func(key string) string {
		if v, ok := vars[key]; ok {
			return v
		}
		return os.Getenv(key)
	})
}

// --- config loading ---

type syntheticYAMLConfig struct {
	Flows []syntheticYAMLFlow `yaml:"flows"`
}

type syntheticYAMLFlow struct {
	Name  string              `yaml:"name"`
	Steps []syntheticYAMLStep `yaml:"steps"`
}

type syntheticYAMLStep struct {
	Name           string            `yaml:"name"`
	URL            string            `yaml:"url"`
	Method         string            `yaml:"method"`
	Headers        map[string]string `yaml:"headers"`
	Body           string            `yaml:"body"`
	BodyType       string            `yaml:"body_type"`
	ExpectedStatus int               `yaml:"expected_status"`
	BodyContains   string            `yaml:"body_contains"`
	Extract        []syntheticExtract `yaml:"extract"`
}

type syntheticExtract struct {
	Name    string `yaml:"name"`
	JSONKey string `yaml:"json_key"`
}

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true, "OPTIONS": true,
}

func loadFlows(file string) ([]Flow, error) {
	if !config.IsYAML(file) {
		return nil, fmt.Errorf("synthetic monitor requires a YAML config file, got: %s", file)
	}
	var cfg syntheticYAMLConfig
	if err := config.DecodeYAML(file, &cfg); err != nil {
		return nil, err
	}

	var errs []string
	for fi, f := range cfg.Flows {
		if f.Name == "" {
			errs = append(errs, fmt.Sprintf("flow[%d]: name is required", fi))
		}
		if len(f.Steps) == 0 {
			errs = append(errs, fmt.Sprintf("flow[%d] %q: at least one step is required", fi, f.Name))
		}
		for si, s := range f.Steps {
			prefix := fmt.Sprintf("flow[%d] %q step[%d]", fi, f.Name, si)
			if s.Name == "" {
				errs = append(errs, fmt.Sprintf("%s: name is required", prefix))
			}
			if s.URL == "" {
				errs = append(errs, fmt.Sprintf("%s: url is required", prefix))
			}
			method := strings.ToUpper(s.Method)
			if method == "" {
				method = "GET"
			}
			if !validMethods[method] {
				errs = append(errs, fmt.Sprintf("%s: unknown method %q", prefix, s.Method))
			}
			for ei, ext := range s.Extract {
				if ext.Name == "" {
					errs = append(errs, fmt.Sprintf("%s extract[%d]: name is required", prefix, ei))
				}
				if ext.JSONKey == "" {
					errs = append(errs, fmt.Sprintf("%s extract[%d]: json_key is required", prefix, ei))
				}
			}
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %s", file, strings.Join(errs, "; "))
	}

	flows := make([]Flow, 0, len(cfg.Flows))
	for _, f := range cfg.Flows {
		steps := make([]Step, 0, len(f.Steps))
		for _, s := range f.Steps {
			method := strings.ToUpper(s.Method)
			if method == "" {
				method = "GET"
			}
			extracts := make([]Extract, 0, len(s.Extract))
			for _, e := range s.Extract {
				extracts = append(extracts, Extract{Name: e.Name, JSONKey: e.JSONKey})
			}
			steps = append(steps, Step{
				Name:           s.Name,
				URL:            s.URL,
				Method:         method,
				Headers:        s.Headers,
				Body:           s.Body,
				BodyType:       s.BodyType,
				ExpectedStatus: s.ExpectedStatus,
				BodyContains:   s.BodyContains,
				Extract:        extracts,
			})
		}
		flows = append(flows, Flow{Name: f.Name, Steps: steps})
	}
	return flows, nil
}
