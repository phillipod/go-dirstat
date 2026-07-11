package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/index"
)

func TestQueryIndexNonWritingModesDoNotCreateState(t *testing.T) {
	for _, mode := range []string{queryIndexLive, queryIndexPrefer, queryIndexOnly} {
		t.Run(mode, func(t *testing.T) {
			env := isolateStateQueryCLI(t)
			root := queryIndexFixtureRoot(t)
			args := queryIndexArgs(mode, root)
			result := runStateQueryCLI(t, context.Background(), args...)
			if mode == queryIndexOnly {
				if result.err == nil || !strings.Contains(result.err.Error(), "run query --index=refresh") {
					t.Fatalf("missing index-only error = %v", result.err)
				}
			} else if result.err != nil {
				t.Fatalf("query --index=%s: %v\n%s", mode, result.err, result.stderr)
			}
			if mode == queryIndexPrefer {
				evidence := decodeQueryIndexEvidence(t, result.stderr)
				if len(evidence) != 1 || evidence[0].Mode != mode || evidence[0].Source != queryIndexLive ||
					!evidence[0].Complete || !strings.Contains(evidence[0].Detail, "fallback=") {
					t.Fatalf("missing-index prefer evidence = %#v", evidence)
				}
			}
			for _, path := range []string{env.cacheHome, env.stateHome, env.configHome} {
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("query --index=%s created %q: %v", mode, path, err)
				}
			}
		})
	}
}

func TestQueryIndexFreshModesHaveOutputParityAndStructuredEvidence(t *testing.T) {
	isolateStateQueryCLI(t)
	root := queryIndexFixtureRoot(t)

	live := runStateQueryCLI(t, context.Background(), queryIndexArgs(queryIndexLive, root)...)
	if live.err != nil || live.stderr != "" {
		t.Fatalf("live query output=%q stderr=%q error=%v", live.stdout, live.stderr, live.err)
	}
	if records := decodeJSONLines(t, live.stdout); len(records) != 2 {
		t.Fatalf("live records = %#v", records)
	}

	for _, mode := range []string{queryIndexRefresh, queryIndexPrefer, queryIndexOnly} {
		result := runStateQueryCLI(t, context.Background(), queryIndexArgs(mode, root)...)
		if result.err != nil {
			t.Fatalf("query --index=%s: %v\n%s", mode, result.err, result.stderr)
		}
		if !bytes.Equal(result.stdout, live.stdout) {
			t.Fatalf("query --index=%s output differs from live:\n%s\nwant:\n%s", mode, result.stdout, live.stdout)
		}
		evidence := decodeQueryIndexEvidence(t, result.stderr)
		if len(evidence) != 1 {
			t.Fatalf("query --index=%s evidence = %#v", mode, evidence)
		}
		wantSource := "index"
		if mode == queryIndexRefresh {
			wantSource = queryIndexLive
		}
		entry := evidence[0]
		if entry.Mode != mode || entry.Source != wantSource || entry.Root != root || entry.Fingerprint == "" ||
			!entry.Complete || entry.ScannedAt.IsZero() || entry.Age == "" {
			t.Fatalf("query --index=%s evidence = %#v", mode, entry)
		}
		if strings.Contains(result.stderr, "dirstat:") {
			t.Fatalf("JSONL evidence contains text contamination: %q", result.stderr)
		}
	}
}

func TestQueryIndexMultiRootEvidenceIsJSONLAndStdoutStaysMachineClean(t *testing.T) {
	isolateStateQueryCLI(t)
	first := queryIndexFixtureRoot(t)
	second := queryIndexFixtureRoot(t)
	result := runStateQueryCLI(t, context.Background(), "--apparent", "query", "--index=refresh", "--index-evidence=jsonl",
		"--format=jsonl", "--fields=path,size", "--kind=file", "--sort=name", first, second)
	if result.err != nil {
		t.Fatalf("multi-root refresh: %v\n%s", result.err, result.stderr)
	}
	records := decodeJSONLines(t, result.stdout)
	if len(records) != 4 {
		t.Fatalf("multi-root records = %#v", records)
	}
	for _, record := range records {
		if _, ok := record["path"].(string); !ok || record["size"] == nil {
			t.Fatalf("query record is contaminated or incomplete: %#v", record)
		}
	}
	evidence := decodeQueryIndexEvidence(t, result.stderr)
	if len(evidence) != 2 || evidence[0].Root != first || evidence[1].Root != second {
		t.Fatalf("multi-root evidence = %#v", evidence)
	}
	for _, entry := range evidence {
		if entry.Mode != queryIndexRefresh || entry.Source != queryIndexLive || !entry.Complete || entry.Detail != "published" {
			t.Fatalf("multi-root evidence entry = %#v", entry)
		}
	}
}

func TestQueryIndexPreferFallsBackWithoutMutatingStaleOrCorruptEntries(t *testing.T) {
	for _, state := range []string{"stale", "corrupt"} {
		t.Run(state, func(t *testing.T) {
			env := isolateStateQueryCLI(t)
			root := queryIndexFixtureRoot(t)
			live := runStateQueryCLI(t, context.Background(), queryIndexArgs(queryIndexLive, root)...)
			if live.err != nil {
				t.Fatal(live.err)
			}
			refresh := runStateQueryCLI(t, context.Background(), queryIndexArgs(queryIndexRefresh, root)...)
			if refresh.err != nil {
				t.Fatal(refresh.err)
			}
			path := singleMatchingFile(t, filepath.Join(env.cacheHome, "dirstat"), "*.bin")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if state == "stale" {
				snapshot, err := index.Unmarshal(data, "")
				if err != nil {
					t.Fatal(err)
				}
				snapshot.ScannedAt = time.Now().Add(-31 * 24 * time.Hour).UTC()
				data, err = snapshot.Marshal()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				data = []byte("corrupt persisted query index")
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}

			prefer := runStateQueryCLI(t, context.Background(), queryIndexArgs(queryIndexPrefer, root)...)
			if prefer.err != nil {
				t.Fatalf("prefer %s fallback: %v\n%s", state, prefer.err, prefer.stderr)
			}
			if !bytes.Equal(prefer.stdout, live.stdout) {
				t.Fatalf("prefer %s output differs from live:\n%s\nwant:\n%s", state, prefer.stdout, live.stdout)
			}
			evidence := decodeQueryIndexEvidence(t, prefer.stderr)
			wantDiagnostic := state
			if state == "corrupt" {
				wantDiagnostic = "incompatible"
			}
			if len(evidence) != 1 || evidence[0].Source != queryIndexLive || !strings.Contains(strings.ToLower(evidence[0].Detail), wantDiagnostic) {
				t.Fatalf("prefer %s evidence = %#v", state, evidence)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, data) {
				t.Fatalf("prefer %s mutated index: equal=%t error=%v", state, bytes.Equal(after, data), err)
			}

			only := runStateQueryCLI(t, context.Background(), queryIndexArgs(queryIndexOnly, root)...)
			if only.err == nil || !strings.Contains(strings.ToLower(only.err.Error()), state) {
				t.Fatalf("only %s error = %v", state, only.err)
			}
			if len(only.stdout) != 0 || only.stderr != "" {
				t.Fatalf("only %s emitted output stdout=%q stderr=%q", state, only.stdout, only.stderr)
			}
		})
	}
}

func TestQueryIndexRefreshNeverPublishesIncompleteScan(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("dangling symlink fixture requires native symlink support")
	}
	env := isolateStateQueryCLI(t)
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := runStateQueryCLI(t, context.Background(), "--follow", "query", "--index=refresh", "--index-evidence=jsonl",
		"--format=jsonl", "--kind=file", root)
	if result.err == nil || ExitCode(result.err) != ExitScanIncomplete {
		t.Fatalf("incomplete refresh error=%v exit=%d", result.err, ExitCode(result.err))
	}
	if len(result.stdout) != 0 || result.stderr != "" {
		t.Fatalf("incomplete refresh emitted output stdout=%q stderr=%q", result.stdout, result.stderr)
	}
	if err := filepath.WalkDir(env.cacheHome, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if filepath.Ext(path) == ".bin" {
			t.Fatalf("incomplete refresh published snapshot %q", path)
		}
		return nil
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("inspect incomplete refresh cache: %v", err)
	}
}

func TestQueryIndexRefreshHonorsCanceledContextWithoutCreatingState(t *testing.T) {
	env := isolateStateQueryCLI(t)
	root := queryIndexFixtureRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := runStateQueryCLI(t, ctx, queryIndexArgs(queryIndexRefresh, root)...)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled refresh error = %v", result.err)
	}
	if len(result.stdout) != 0 || result.stderr != "" {
		t.Fatalf("canceled refresh emitted output stdout=%q stderr=%q", result.stdout, result.stderr)
	}
	if _, err := os.Lstat(env.cacheHome); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("canceled refresh created cache state: %v", err)
	}
}

func TestLiveQueryAutomaticallyExcludesOperationalState(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "scan")
	cacheHome := filepath.Join(root, ".cache")
	stateHome := filepath.Join(root, ".state")
	configHome := filepath.Join(base, "config")
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	for path, content := range map[string]string{
		filepath.Join(root, "visible.txt"):                  "visible",
		filepath.Join(cacheHome, "dirstat", "cache.bin"):    "cache",
		filepath.Join(stateHome, "dirstat", "history", "x"): "history",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := runStateQueryCLI(t, context.Background(), "query", "--format=tsv", "--fields=relative", "--kind=file", "--sort=name", root)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if got := strings.TrimSpace(string(result.stdout)); got != "visible.txt" {
		t.Fatalf("live query included operational state: %q", got)
	}
}

type queryIndexEvidenceContract struct {
	Mode        string    `json:"mode"`
	Source      string    `json:"source"`
	Root        string    `json:"root"`
	Age         string    `json:"age"`
	Fingerprint string    `json:"fingerprint"`
	Complete    bool      `json:"complete"`
	ScannedAt   time.Time `json:"scanned_at"`
	Detail      string    `json:"detail"`
}

func queryIndexFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "alpha.bin"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "beta.bin"), []byte("beta-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func queryIndexArgs(mode, root string) []string {
	args := []string{"--apparent", "query", "--format=jsonl", "--fields=relative,size", "--kind=file", "--sort=name", "--index=" + mode}
	if mode != queryIndexLive {
		args = append(args, "--index-evidence=jsonl")
	}
	return append(args, root)
}

func decodeQueryIndexEvidence(t *testing.T, raw string) []queryIndexEvidenceContract {
	t.Helper()
	lines := nonemptyLines([]byte(raw))
	result := make([]queryIndexEvidenceContract, 0, len(lines))
	for _, line := range lines {
		var entry queryIndexEvidenceContract
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("query index evidence contains non-JSONL data %q: %v", line, err)
		}
		result = append(result, entry)
	}
	return result
}

func decodeJSONLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	lines := nonemptyLines(raw)
	result := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("stdout contains non-JSONL data %q: %v", line, err)
		}
		result = append(result, record)
	}
	return result
}

func nonemptyLines(raw []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}
