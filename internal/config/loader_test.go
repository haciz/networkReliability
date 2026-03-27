package config

import (
	"os"
	"strings"
	"testing"
)

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "loader_test_*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoadLines_FileNotFound(t *testing.T) {
	_, err := LoadLines("/nonexistent/does-not-exist.txt", 2)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadLines_EmptyFile(t *testing.T) {
	f := writeTempFile(t, "")
	rows, err := LoadLines(f, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}

func TestLoadLines_CommentsOnly(t *testing.T) {
	f := writeTempFile(t, "# comment one\n# comment two\n\n")
	rows, err := LoadLines(f, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for comment-only file, got %d", len(rows))
	}
}

func TestLoadLines_AllValidLines(t *testing.T) {
	f := writeTempFile(t, "host1 A 8.8.8.8\nhost2 AAAA 8.8.4.4\n")
	rows, err := LoadLines(f, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0][0] != "host1" || rows[0][1] != "A" || rows[0][2] != "8.8.8.8" {
		t.Fatalf("unexpected row[0]: %v", rows[0])
	}
}

func TestLoadLines_SkipsShortLines(t *testing.T) {
	// "only_one" has 1 field, below minFields=2 — should be skipped
	f := writeTempFile(t, "a b\nonly_one\nc d\n")
	rows, err := LoadLines(f, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (skipping short line), got %d", len(rows))
	}
	if rows[0][0] != "a" || rows[1][0] != "c" {
		t.Fatalf("unexpected rows: %v", rows)
	}
}

func TestLoadLines_MixedCommentAndValid(t *testing.T) {
	content := strings.Join([]string{
		"# comment",
		"host1 A 8.8.8.8",
		"",
		"# another comment",
		"host2 AAAA 8.8.4.4",
	}, "\n")
	f := writeTempFile(t, content)
	rows, err := LoadLines(f, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
}
