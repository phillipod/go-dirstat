//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

const (
	ptyTranscriptLimit = 2 << 20
	ptyPollInterval    = 20 * time.Millisecond
)

// TestTUIRealPTYLifecycle is intentionally a real process/terminal smoke rather
// than another model test. It is Unix-only because Windows ConPTY lifecycle is
// covered by the portable headless/model suite until a stable native harness is
// available there.
func TestTUIRealPTYLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	binary := ptyTestBinary(t, ctx)
	fixture := ptyFixture(t)
	isolation := t.TempDir()
	home := filepath.Join(isolation, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	stateHome := filepath.Join(isolation, "state")
	cacheHome := filepath.Join(isolation, "cache")
	configHome := filepath.Join(isolation, "config")

	command := exec.CommandContext(ctx, binary, "tui", "--read-only", "--no-cache", fixture)
	command.Env = isolatedPTYEnv(home, stateHome, cacheHome, configHome)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 100})
	if err != nil {
		t.Fatalf("start dirstat PTY: %v", err)
	}
	// Bubble Tea queries terminal background color and cursor position during
	// startup. A real terminal answers these; emulate the same replies so the
	// harness tests application lifecycle rather than query timeouts.
	if _, err := io.WriteString(terminal, "\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[1;1R"); err != nil {
		_ = terminal.Close()
		_ = command.Process.Kill()
		t.Fatalf("write PTY capability replies: %v", err)
	}

	transcript := &lockedTranscript{limit: ptyTranscriptLimit}
	readDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(transcript, terminal)
		readDone <- copyErr
	}()
	wait := &commandWait{done: make(chan struct{})}
	go func() {
		wait.err = command.Wait()
		close(wait.done)
	}()

	defer func() {
		if !wait.finished() {
			_ = command.Process.Kill()
			select {
			case <-wait.done:
			case <-time.After(2 * time.Second):
			}
		}
		_ = terminal.Close()
		select {
		case <-readDone:
		case <-time.After(time.Second):
		}
		writePTYArtifact(t, transcript.snapshot())
		if t.Failed() {
			t.Logf("PTY transcript tail: %q", transcript.tail(16<<10))
		}
	}()

	waitForPTY(t, ctx, wait, transcript, 0, "initial ANSI frame", func(frame []byte) bool {
		return bytes.Contains(frame, []byte("\x1b[")) &&
			bytes.Contains(frame, []byte("dirstat")) &&
			bytes.Contains(frame, []byte("largest.bin"))
	})
	waitForPTYQuiet(t, ctx, wait, transcript, 120*time.Millisecond)

	resizeAt := transcript.size()
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 30, Cols: 140}); err != nil {
		t.Fatalf("resize PTY: %v", err)
	}
	waitForPTY(t, ctx, wait, transcript, resizeAt, "resized context pane", func(frame []byte) bool {
		return bytes.Contains(frame, []byte("Inspect"))
	})

	navigationAt := transcript.size()
	writePTYInput(t, terminal, "\x1b[B")
	waitForPTY(t, ctx, wait, transcript, navigationAt, "navigation redraw", func(frame []byte) bool {
		return len(frame) > 0
	})

	helpAt := transcript.size()
	writePTYInput(t, terminal, "?")
	waitForPTY(t, ctx, wait, transcript, helpAt, "help screen", func(frame []byte) bool {
		return bytes.Contains(frame, []byte("toggle this help")) &&
			bytes.Contains(frame, []byte("set caller-available reclaim target"))
	})

	closeHelpAt := transcript.size()
	writePTYInput(t, terminal, "?")
	waitForPTY(t, ctx, wait, transcript, closeHelpAt, "help close", func(frame []byte) bool {
		return len(frame) > 0
	})

	rescanAt := transcript.size()
	rescanFile := filepath.Join(fixture, "rescan-visible.dat")
	if err := os.WriteFile(rescanFile, []byte("visible after rescan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePTYInput(t, terminal, "r")
	waitForPTY(t, ctx, wait, transcript, rescanAt, "rescan redraw", func(frame []byte) bool {
		return bytes.Contains(frame, []byte(filepath.Base(rescanFile)))
	})

	writePTYInput(t, terminal, "q")
	select {
	case <-wait.done:
		if wait.err != nil {
			t.Fatalf("dirstat TUI exit: %v", wait.err)
		}
	case <-ctx.Done():
		t.Fatalf("dirstat TUI did not shut down before deadline: %v", ctx.Err())
	}

	for _, path := range []string{stateHome, cacheHome, configHome} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("read-only/no-cache TUI created state %q: %v", path, statErr)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("isolated HOME contains unexpected state: %v", entryNames(entries))
	}
}

func ptyTestBinary(t *testing.T, ctx context.Context) string {
	t.Helper()
	if configured := os.Getenv("DIRSTAT_PTY_BINARY"); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(absolute)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			t.Fatalf("DIRSTAT_PTY_BINARY %q is not executable: %v", absolute, err)
		}
		return absolute
	}

	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve PTY test source path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	binary := filepath.Join(t.TempDir(), "dirstat-pty")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binary, "./cmd/dirstat")
	build.Dir = repo
	build.Env = withoutEnvironment(os.Environ(), "GOROOT")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real dirstat binary: %v\n%s", err, output)
	}
	return binary
}

func ptyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{
		"largest.bin":           bytes.Repeat([]byte("x"), 32<<10),
		"small.txt":             []byte("small\n"),
		"nested/diagnostic.log": []byte("diagnostic\n"),
	}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func isolatedPTYEnv(home, state, cache, config string) []string {
	environment := withoutEnvironment(os.Environ(), "HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "TERM", "NO_COLOR")
	return append(environment,
		"HOME="+home,
		"XDG_STATE_HOME="+state,
		"XDG_CACHE_HOME="+cache,
		"XDG_CONFIG_HOME="+config,
		"TERM=xterm-256color",
	)
}

func withoutEnvironment(environment []string, names ...string) []string {
	rejected := make(map[string]bool, len(names))
	for _, name := range names {
		rejected[name] = true
	}
	filtered := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if !rejected[name] {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func writePTYInput(t *testing.T, terminal *os.File, input string) {
	t.Helper()
	if _, err := io.WriteString(terminal, input); err != nil {
		t.Fatalf("write PTY input %q: %v", input, err)
	}
}

func waitForPTY(
	t *testing.T,
	ctx context.Context,
	wait *commandWait,
	transcript *lockedTranscript,
	after int,
	description string,
	predicate func([]byte) bool,
) {
	t.Helper()
	ticker := time.NewTicker(ptyPollInterval)
	defer ticker.Stop()
	for {
		frame := transcript.after(after)
		if predicate(frame) {
			return
		}
		select {
		case <-wait.done:
			t.Fatalf("dirstat exited before %s: %v", description, wait.err)
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %s: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForPTYQuiet(t *testing.T, ctx context.Context, wait *commandWait, transcript *lockedTranscript, quietFor time.Duration) {
	t.Helper()
	ticker := time.NewTicker(ptyPollInterval)
	defer ticker.Stop()
	lastSize := transcript.size()
	quietSince := time.Now()
	for {
		select {
		case <-wait.done:
			t.Fatalf("dirstat exited before initial frame settled: %v", wait.err)
		case <-ctx.Done():
			t.Fatalf("timeout waiting for PTY output to settle: %v", ctx.Err())
		case <-ticker.C:
			current := transcript.size()
			if current != lastSize {
				lastSize, quietSince = current, time.Now()
			} else if time.Since(quietSince) >= quietFor {
				return
			}
		}
	}
}

type commandWait struct {
	done chan struct{}
	err  error
}

func (w *commandWait) finished() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

type lockedTranscript struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (t *lockedTranscript) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, data...)
	if overflow := len(t.data) - t.limit; overflow > 0 {
		copy(t.data, t.data[overflow:])
		t.data = t.data[:t.limit]
	}
	return len(data), nil
}

func (t *lockedTranscript) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.data)
}

func (t *lockedTranscript) snapshot() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.data...)
}

func (t *lockedTranscript) after(offset int) []byte {
	data := t.snapshot()
	if offset < 0 || offset > len(data) {
		return data
	}
	return data[offset:]
}

func (t *lockedTranscript) tail(limit int) string {
	data := t.snapshot()
	if len(data) > limit {
		data = data[len(data)-limit:]
	}
	return string(data)
}

func writePTYArtifact(t *testing.T, transcript []byte) {
	t.Helper()
	dir := os.Getenv("DIRSTAT_PTY_ARTIFACT_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Errorf("create PTY artifact directory: %v", err)
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("tui-pty-%s-%s.ansi", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(path, transcript, 0o600); err != nil {
		t.Errorf("write PTY transcript: %v", err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}
