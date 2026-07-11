package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingAndConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg, err := Load()
	if err != nil || cfg.HistoryMax != 20 || cfg.State != Default().State {
		t.Fatalf("default = %+v, %v", cfg, err)
	}
	path := filepath.Join(dir, "dirstat", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"tools":{"editor":["vi"]},"history_max":5,"tui":{"target_available_bytes":10737418240,"queue_max_operations":25,"queue_max_reclaim_bytes":53687091200}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load()
	if err != nil || len(cfg.Tools.Editor) != 1 || cfg.HistoryMax != 5 || cfg.TUI.TargetAvailableBytes != 10<<30 || cfg.TUI.QueueMaxOperations != 25 || cfg.TUI.QueueMaxReclaimBytes != 50<<30 {
		t.Fatalf("configured = %+v, %v", cfg, err)
	}
}

func TestLoadDefaultsWhenUserConfigDirectoryIsUnavailable(t *testing.T) {
	for _, name := range []string{"XDG_CONFIG_HOME", "HOME", "APPDATA", "USERPROFILE"} {
		t.Setenv(name, "")
	}
	cfg, err := Load()
	if err != nil || !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("Load() without a user config directory = %+v, %v; want defaults", cfg, err)
	}
}

func TestLoadStateDurationBounds(t *testing.T) {
	maxHours := int64(math.MaxInt64 / int64(time.Hour))
	maxDays := int64(math.MaxInt64 / int64(24*time.Hour))
	valid := fmt.Sprintf(`{"state":{"cache_ttl_hours":%d,"history_ttl_days":%d}}`, maxHours, maxDays)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "dirstat", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("maximum representable state TTL rejected: %v", err)
	}
	assertConfigLoadError(t, fmt.Sprintf(`{"state":{"cache_ttl_hours":%d}}`, maxHours+1), "state.cache_ttl_hours is too large")
	assertConfigLoadError(t, fmt.Sprintf(`{"state":{"history_ttl_days":%d}}`, maxDays+1), "state.history_ttl_days is too large")
}

func TestStateDirUsesXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := StateDir()
	if err != nil || filepath.Base(dir) != "dirstat" {
		t.Fatalf("state dir = %q, %v", dir, err)
	}
}

func TestLoadRejectsUnknownDuplicateAndTrailingConfiguration(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown safety field", data: `{"readonly":true}`, want: `unknown field "readonly"`},
		{name: "unknown nested field", data: `{"tools":{"editr":["vi"]}}`, want: `unknown field "editr"`},
		{name: "duplicate field", data: `{"read_only":true,"read_only":false}`, want: `duplicate field "read_only"`},
		{name: "duplicate nested field", data: `{"tools":{"editor":["vi"],"editor":["ed"]}}`, want: `duplicate field "tools.editor"`},
		{name: "trailing object", data: `{} {}`, want: "trailing JSON value"},
		{name: "non object", data: `[]`, want: "top-level value must be one JSON object"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertConfigLoadError(t, test.data, test.want)
		})
	}
}

func TestLoadValidatesSafetySensitiveValues(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "zero history", data: `{"history_max":0}`, want: "field history_max must be greater than zero"},
		{name: "zero cache bytes", data: `{"state":{"cache_max_bytes":0}}`, want: "state.cache_max_bytes must be greater than zero"},
		{name: "zero cache TTL", data: `{"state":{"cache_ttl_hours":0}}`, want: "state.cache_ttl_hours must be greater than zero"},
		{name: "zero history bytes", data: `{"state":{"history_max_bytes":0}}`, want: "state.history_max_bytes must be greater than zero"},
		{name: "zero history TTL", data: `{"state":{"history_ttl_days":0}}`, want: "state.history_ttl_days must be greater than zero"},
		{name: "zero queue operations", data: `{"tui":{"queue_max_operations":0}}`, want: "tui.queue_max_operations must be greater than zero"},
		{name: "negative reclaim cap", data: `{"tui":{"queue_max_reclaim_bytes":-1}}`, want: "tui.queue_max_reclaim_bytes cannot be negative"},
		{name: "relative audit", data: `{"audit_path":"relative.jsonl"}`, want: "field audit_path must be absolute"},
		{name: "blank executable", data: `{"tools":{"editor":["  "]}}`, want: "tools.editor[0]"},
		{name: "sudo executable", data: `{"tools":{"shell":["/usr/bin/sudo","sh"]}}`, want: "must not invoke sudo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertConfigLoadError(t, test.data, test.want)
		})
	}
}

func assertConfigLoadError(t *testing.T, data, want string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "dirstat", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want path and %q", err, want)
	}
}
