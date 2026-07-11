// Package config loads optional user configuration for external tools and
// audit behavior. Commands are argv arrays so selected paths are never parsed
// or interpolated by a shell.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Tools struct {
	Pager  []string `json:"pager,omitempty"`
	Editor []string `json:"editor,omitempty"`
	Shell  []string `json:"shell,omitempty"`
}

// TUI configures pressure goals and queue safety policy for interactive use.
// Byte values are raw bytes so the JSON contract remains unambiguous.
type TUI struct {
	TargetAvailableBytes uint64 `json:"target_available_bytes,omitempty"`
	QueueMaxOperations   int    `json:"queue_max_operations,omitempty"`
	QueueMaxReclaimBytes int64  `json:"queue_max_reclaim_bytes,omitempty"`
}

// State configures global cache and durable-history retention. TTL values use
// whole hours/days so the JSON contract remains portable and unambiguous.
type State struct {
	CacheMaxBytes   int64 `json:"cache_max_bytes,omitempty"`
	CacheTTLHours   int   `json:"cache_ttl_hours,omitempty"`
	HistoryMaxBytes int64 `json:"history_max_bytes,omitempty"`
	HistoryTTLDays  int   `json:"history_ttl_days,omitempty"`
}

type Config struct {
	Tools      Tools  `json:"tools,omitempty"`
	TUI        TUI    `json:"tui,omitempty"`
	AuditPath  string `json:"audit_path,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	HistoryMax int    `json:"history_max,omitempty"`
	State      State  `json:"state,omitempty"`
}

func Default() Config {
	return Config{
		HistoryMax: 20,
		TUI:        TUI{QueueMaxOperations: 1000},
		State: State{
			CacheMaxBytes: 512 << 20, CacheTTLHours: 30 * 24,
			HistoryMaxBytes: 2 << 30, HistoryTTLDays: 30,
		},
	}
}

func Path() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "dirstat", "config.json"), nil
	}
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
		// Pure analysis remains available for service accounts without a home
		// directory. Explicit state commands resolve their required paths later.
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := validateJSONDocument(data); err != nil {
		return Default(), fmt.Errorf("config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Default(), fmt.Errorf("config %s: %w", path, err)
	}
	if err := validate(cfg); err != nil {
		return Default(), fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func validateJSONDocument(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if token != json.Delim('{') {
		return errors.New("top-level value must be one JSON object")
	}
	if err := consumeJSONObject(decoder, ""); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value after configuration object")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func consumeJSONObject(decoder *json.Decoder, path string) error {
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode object field: %w", err)
		}
		field, ok := token.(string)
		if !ok {
			return errors.New("object field name is not a string")
		}
		fieldPath := field
		if path != "" {
			fieldPath = path + "." + field
		}
		if seen[field] {
			return fmt.Errorf("duplicate field %q", fieldPath)
		}
		seen[field] = true
		if err := consumeJSONValue(decoder, fieldPath); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil {
		return fmt.Errorf("close object: %w", err)
	} else if token != json.Delim('}') {
		return errors.New("malformed JSON object")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode field %q: %w", path, err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return consumeJSONObject(decoder, path)
	case '[':
		index := 0
		for decoder.More() {
			if err := consumeJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		if token, err := decoder.Token(); err != nil {
			return fmt.Errorf("close array %q: %w", path, err)
		} else if token != json.Delim(']') {
			return fmt.Errorf("field %q has a malformed array", path)
		}
		return nil
	default:
		return fmt.Errorf("field %q has an unexpected delimiter", path)
	}
}

func validate(cfg Config) error {
	if cfg.HistoryMax <= 0 {
		return errors.New("field history_max must be greater than zero")
	}
	if cfg.State.CacheMaxBytes <= 0 {
		return errors.New("field state.cache_max_bytes must be greater than zero")
	}
	if cfg.State.CacheTTLHours <= 0 {
		return errors.New("field state.cache_ttl_hours must be greater than zero")
	}
	if int64(cfg.State.CacheTTLHours) > int64((time.Duration(1<<63-1))/time.Hour) {
		return errors.New("field state.cache_ttl_hours is too large")
	}
	if cfg.State.HistoryMaxBytes <= 0 {
		return errors.New("field state.history_max_bytes must be greater than zero")
	}
	if cfg.State.HistoryTTLDays <= 0 {
		return errors.New("field state.history_ttl_days must be greater than zero")
	}
	if int64(cfg.State.HistoryTTLDays) > int64((time.Duration(1<<63-1))/(24*time.Hour)) {
		return errors.New("field state.history_ttl_days is too large")
	}
	if cfg.TUI.QueueMaxOperations <= 0 {
		return errors.New("field tui.queue_max_operations must be greater than zero")
	}
	if cfg.TUI.QueueMaxReclaimBytes < 0 {
		return errors.New("field tui.queue_max_reclaim_bytes cannot be negative")
	}
	if cfg.AuditPath != "" {
		if strings.IndexByte(cfg.AuditPath, 0) >= 0 {
			return errors.New("field audit_path contains a NUL byte")
		}
		if !filepath.IsAbs(cfg.AuditPath) {
			return errors.New("field audit_path must be absolute")
		}
	}
	for name, argv := range map[string][]string{
		"tools.pager":  cfg.Tools.Pager,
		"tools.editor": cfg.Tools.Editor,
		"tools.shell":  cfg.Tools.Shell,
	} {
		if err := validateArgv(name, argv); err != nil {
			return err
		}
	}
	return nil
}

func validateArgv(field string, argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	for i, arg := range argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("field %s[%d] contains a NUL byte", field, i)
		}
	}
	if strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("field %s[0] must name an executable", field)
	}
	if strings.EqualFold(filepath.Base(argv[0]), "sudo") {
		return fmt.Errorf("field %s[0] must not invoke sudo", field)
	}
	return nil
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
