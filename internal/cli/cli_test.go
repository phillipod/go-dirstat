package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestParseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  int64
	}{
		{input: "", want: 0},
		{input: "42", want: 42},
		{input: "2K", want: 2 * 1024},
		{input: "3m", want: 3 * (1 << 20)},
		{input: " 4G ", want: 4 * (1 << 30)},
		{input: "1T", want: 1 << 40},
		{input: "1P", want: 1 << 50},
		{input: "1E", want: 1 << 60},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got, err := parseSize(tt.input)
			if err != nil {
				t.Fatalf("parseSize(%q) returned an error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("parseSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestConfigPolicyValid(t *testing.T) {
	t.Parallel()

	cfg := newConfig()
	cfg.MinSize = "2K"
	cfg.MaxSize = "4M"
	cfg.Depth = 3
	cfg.Limit = 20
	cfg.Jobs = 2
	cfg.Sort = "mtime"
	cfg.NoHidden = true

	policy, err := cfg.policy()
	if err != nil {
		t.Fatalf("policy returned an error: %v", err)
	}
	if policy.MinSize != 2*1024 || policy.MaxSize != 4*(1<<20) {
		t.Fatalf("policy size range = %d..%d, want %d..%d", policy.MinSize, policy.MaxSize, 2*1024, 4*(1<<20))
	}
	if policy.IncludeHidden {
		t.Fatal("policy includes hidden files when --no-hidden is set")
	}
	if got := cfg.textOptions(io.Discard).Sort; got != tree.SortMTimeDesc {
		t.Fatalf("sort mode = %v, want %v", got, tree.SortMTimeDesc)
	}
}

func TestTextOptionsDisableColorForNonFileWriter(t *testing.T) {
	cfg := newConfig()
	if cfg.textOptions(&strings.Builder{}).Color {
		t.Fatal("non-terminal command writer unexpectedly enables ANSI color")
	}
}

func TestCommandRejectsInvalidConfigurationBeforeScan(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "invalid minimum size", args: []string{"--min-size", "large"}, wantErr: "invalid --min-size"},
		{name: "invalid maximum size", args: []string{"--max-size", "1Q"}, wantErr: "invalid --max-size"},
		{name: "zero maximum size", args: []string{"--max-size", "0"}, wantErr: "must be greater than zero"},
		{name: "inverted size range", args: []string{"--min-size", "2M", "--max-size", "1M"}, wantErr: "must not exceed"},
		{name: "invalid sort", args: []string{"--sort", "newest"}, wantErr: "invalid --sort"},
		{name: "negative depth", args: []string{"--depth=-1"}, wantErr: "--depth must be zero or greater"},
		{name: "negative limit", args: []string{"--limit=-1"}, wantErr: "--limit must be zero or greater"},
		{name: "negative jobs", args: []string{"--jobs=-1"}, wantErr: "--jobs must be zero or greater"},
		{name: "excessive jobs", args: []string{"--jobs=4097"}, wantErr: "--jobs must not exceed 4096"},
		{name: "invalid format", args: []string{"--format=csv"}, wantErr: "invalid --format"},
		{name: "tsv extension flag", args: []string{"--format=tsv", "--by-ext"}, wantErr: "cannot be used with --by-ext"},
		{name: "tsv extensions command", args: []string{"extensions", "--format=tsv"}, wantErr: "unknown flag"},
		{name: "tsv tui command", args: []string{"tui", "--format=tsv"}, wantErr: "unknown flag"},
		{name: "malformed exclude glob", args: []string{"--exclude=["}, wantErr: "invalid --exclude glob"},
		{name: "relative exclude path", args: []string{"--exclude-path=build"}, wantErr: "--exclude-path must be absolute"},
		{name: "relative include path", args: []string{"--include-path=data"}, wantErr: "--include-path must be absolute"},
		{name: "empty included filesystem", args: []string{"--include-fs= "}, wantErr: "--include-fs must not be empty"},
		{name: "empty excluded filesystem", args: []string{"--exclude-fs="}, wantErr: "--exclude-fs must not be empty"},
		{name: "filesystem overlap", args: []string{"--include-fs=ext4", "--exclude-fs=ext4"}, wantErr: "cannot appear in both"},
		{name: "filesystem traversal conflict", args: []string{"--cross-device", "-x"}, wantErr: "cannot be used together"},
		{name: "cache flag in text", args: []string{"--no-cache"}, wantErr: "unknown flag"},
		{name: "depth with extensions", args: []string{"--by-ext", "--depth=1"}, wantErr: "cannot be used with an extension breakdown"},
		{name: "limit with extensions", args: []string{"extensions", "--limit=1"}, wantErr: "unknown flag"},
		{name: "files with extensions", args: []string{"extensions", "--files"}, wantErr: "unknown flag"},
		{name: "sort with extensions", args: []string{"extensions", "--sort=name"}, wantErr: "unknown flag"},
		{name: "text flag with tui", args: []string{"tui", "--bytes"}, wantErr: "unknown flag"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := New()
			cmd.SetArgs(append(tt.args, t.TempDir()))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("Execute(%q) succeeded, want an error containing %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Execute(%q) error = %q, want it to contain %q", tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestCommandTSVIsStableAcrossMultipleRoots(t *testing.T) {
	first := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "a file"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(first, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "dir", "z"), []byte("de"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(second, "tab\tline\nbreak"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := New()
	cmd.SetArgs([]string{"--format=tsv", "--bytes", "--apparent", "--files", "--sort=name", first, second})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	want := "5\t" + first + "\n" +
		"3\t" + filepath.Join(first, "a file") + "\n" +
		"2\t" + filepath.Join(first, "dir") + "\n" +
		"2\t" + filepath.Join(first, "dir", "z") + "\n" +
		"1\t" + second + "\n" +
		"1\t" + filepath.Join(second, `tab\tline\nbreak`) + "\n"
	if got := out.String(); got != want {
		t.Fatalf("TSV output:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(out.String(), "Total:") || strings.Contains(out.String(), "\n\n") || strings.Contains(out.String(), "%") {
		t.Fatalf("TSV contains rich-output chrome: %q", out.String())
	}
}

func TestCommandRichOutputLabelsMultipleRoots(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	cmd := New()
	cmd.SetArgs([]string{"--apparent", "--bytes", "--no-bar", "--no-counts", first, second})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.HasPrefix(got, first+":\n") {
		t.Fatalf("first root is not labeled:\n%s", got)
	}
	if !strings.Contains(got, "\n\n"+second+":\n") {
		t.Fatalf("second root is not labeled:\n%s", got)
	}
}

func TestCommandReturnsWriterErrors(t *testing.T) {
	for _, format := range []string{"text", "tsv"} {
		t.Run(format, func(t *testing.T) {
			wantErr := errors.New("output closed")
			cmd := New()
			cmd.SetArgs([]string{"--format=" + format, t.TempDir()})
			cmd.SetOut(cliErrorWriter{err: wantErr})
			cmd.SetErr(io.Discard)

			err := cmd.Execute()
			if !errors.Is(err, wantErr) {
				t.Fatalf("Execute error = %v, want wrapped %v", err, wantErr)
			}
		})
	}
}

type cliErrorWriter struct{ err error }

func (w cliErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCommandSilencesCobraErrorOutput(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"--format=csv", t.TempDir()})
	var stderr strings.Builder
	cmd.SetErr(&stderr)

	if err := cmd.Execute(); err == nil {
		t.Fatal("Execute succeeded, want invalid-format error")
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("Cobra wrote duplicate error output %q", got)
	}
}

func TestVersionRejectsArgumentsAndReturnsWriterErrors(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"version", "extra"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("version accepted an argument")
	}

	wantErr := errors.New("output closed")
	cmd = New()
	cmd.SetArgs([]string{"version"})
	cmd.SetOut(cliErrorWriter{err: wantErr})
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); !errors.Is(err, wantErr) {
		t.Fatalf("version error = %v, want wrapped %v", err, wantErr)
	}
}

func TestVersionFlag(t *testing.T) {
	cmd := New()
	cmd.SetArgs([]string{"--version"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "dirstat ") || !strings.Contains(out.String(), "built") {
		t.Fatalf("--version output = %q", out.String())
	}
}

func TestParseSizeRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"nope", "1.5G", "K", "-1", "8E", "9223372036854775807T"} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if _, err := parseSize(input); err == nil {
				t.Fatalf("parseSize(%q) succeeded, want an error", input)
			}
		})
	}
}
