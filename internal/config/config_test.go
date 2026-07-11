package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingAndConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfg, err := Load()
	if err != nil || cfg.HistoryMax != 20 {
		t.Fatalf("default = %+v, %v", cfg, err)
	}
	path := filepath.Join(dir, "dirstat", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"tools":{"editor":["vi"]},"history_max":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load()
	if err != nil || len(cfg.Tools.Editor) != 1 || cfg.HistoryMax != 5 {
		t.Fatalf("configured = %+v, %v", cfg, err)
	}
}

func TestStateDirUsesXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir, err := StateDir()
	if err != nil || filepath.Base(dir) != "dirstat" {
		t.Fatalf("state dir = %q, %v", dir, err)
	}
}
