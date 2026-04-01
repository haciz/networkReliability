package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// IsYAML returns true when the file has a .yaml or .yml extension.
func IsYAML(file string) bool {
	ext := strings.ToLower(filepath.Ext(file))
	return ext == ".yaml" || ext == ".yml"
}

// DecodeYAML reads file, expands ${ENV_VAR} references, then unmarshals into
// out. Unknown YAML keys are rejected so typos in config files fail fast.
func DecodeYAML(file string, out interface{}) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	expanded := expandEnv(string(raw))
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("parse %s: %w", file, err)
	}
	return nil
}

// expandEnv substitutes ${VAR} references. Missing vars emit a warning and
// are left as-is rather than silently expanding to an empty string.
func expandEnv(s string) string {
	return os.Expand(s, func(key string) string {
		val, ok := os.LookupEnv(key)
		if !ok {
			slog.Warn("config: env var not set, leaving unexpanded", "var", key)
			return "${" + key + "}"
		}
		return val
	})
}
