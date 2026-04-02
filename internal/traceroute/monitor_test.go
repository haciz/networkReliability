package traceroute

import (
	"os"
	"testing"
)

func writeTempTargets(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "traceroute_targets_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
	return f.Name()
}

func TestLoadTargets_Valid(t *testing.T) {
	content := `targets:
  - host: 8.8.8.8
    max_hops: 20
    probes_per_hop: 3
  - host: 1.1.1.1
`
	f := writeTempTargets(t, content)
	targets, err := loadTargets(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
	if targets[0].Host != "8.8.8.8" {
		t.Errorf("expected host '8.8.8.8', got %q", targets[0].Host)
	}
	if targets[0].MaxHops != 20 {
		t.Errorf("expected max_hops 20, got %d", targets[0].MaxHops)
	}
}

func TestLoadTargets_EmptyHost_Error(t *testing.T) {
	content := `targets:
  - host: ""
    max_hops: 30
`
	f := writeTempTargets(t, content)
	_, err := loadTargets(f)
	if err == nil {
		t.Fatal("expected error for empty host, got nil")
	}
}

func TestLoadTargets_MaxHopsTooLarge_Error(t *testing.T) {
	content := `targets:
  - host: 8.8.8.8
    max_hops: 65
`
	f := writeTempTargets(t, content)
	_, err := loadTargets(f)
	if err == nil {
		t.Fatal("expected error for max_hops > 64")
	}
}

func TestLoadTargets_ProbesPerHopTooLarge_Error(t *testing.T) {
	content := `targets:
  - host: 8.8.8.8
    probes_per_hop: 11
`
	f := writeTempTargets(t, content)
	_, err := loadTargets(f)
	if err == nil {
		t.Fatal("expected error for probes_per_hop > 10")
	}
}

func TestLoadTargets_NonYAML_Error(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "traceroute_targets_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("8.8.8.8\n")
	f.Close()

	_, err = loadTargets(f.Name())
	if err == nil {
		t.Fatal("expected error for non-YAML file")
	}
}
