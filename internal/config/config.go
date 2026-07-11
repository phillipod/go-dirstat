// Package config loads optional user configuration for external tools and
// audit behavior. Commands are argv arrays so selected paths are never parsed
// or interpolated by a shell.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

type Tools struct {
	Pager  []string `json:"pager,omitempty"`
	Editor []string `json:"editor,omitempty"`
	Shell  []string `json:"shell,omitempty"`
}

type Config struct {
	Tools      Tools  `json:"tools,omitempty"`
	AuditPath  string `json:"audit_path,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	HistoryMax int    `json:"history_max,omitempty"`
}

func Default() Config { return Config{HistoryMax: 20} }

func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dirstat", "config.json"), nil
}

func Load() (Config, error) {
	cfg := Default()
	path, err := Path()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), err
	}
	if cfg.HistoryMax <= 0 {
		cfg.HistoryMax = 20
	}
	return cfg, nil
}

// StateDir returns the private directory used for audit and history indexes.
func StateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "dirstat"), nil
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "dirstat", "state"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "dirstat"), nil
}

func DefaultAuditPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "operations.jsonl"), nil
}
