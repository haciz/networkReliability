package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadLines reads a target file and returns each non-blank, non-comment line
// split into fields. Lines with fewer than minFields are skipped with a warning.
func LoadLines(file string, minFields int) ([][]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", file, err)
	}
	defer f.Close()

	var result [][]string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < minFields {
			// log via stderr so callers don't need a logger dependency
			fmt.Fprintf(os.Stderr, "config: %s:%d: expected %d fields, got %d — skipping\n",
				file, lineNum, minFields, len(parts))
			continue
		}
		result = append(result, parts)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", file, err)
	}
	return result, nil
}
