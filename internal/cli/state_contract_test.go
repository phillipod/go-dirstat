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

	"github.com/phillipod/go-dirstat/internal/index"
)

func TestStateReadCommandsTextJSONAndNoCreation(t *testing.T) {
	env := isolateStateQueryCLI(t)

	for _, format := range []string{outputFormatText, outputFormatJSON} {
		for _, subcommand := range []string{"status", "list", "size"} {
			result := runStateQueryCLI(t, context.Background(), "state", "--format="+format, subcommand)
			if result.err != nil {
				t.Fatalf("state --format=%s %s: %v", format, subcommand, result.err)
			}
			if format == outputFormatJSON && !json.Valid(result.stdout) {
				t.Fatalf("state --format=json %s produced invalid JSON %q", subcommand, result.stdout)
			}
			if result.stderr != "" {
				t.Fatalf("state --format=%s %s stderr = %q", format, subcommand, result.stderr)
			}
		}
	}

	for _, path := range []string{env.cacheHome, env.stateHome, env.configHome} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read-only state commands created %q: %v", path, err)
		}
	}
}

func TestStateStatusListAndSizeDescribeIndexAndHistory(t *testing.T) {
	isolateStateQueryCLI(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload.bin"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	refresh := runStateQueryCLI(t, context.Background(), "--apparent", "query", "--index=refresh", "--index-evidence=jsonl", "--kind=file", root)
	if refresh.err != nil {
		t.Fatalf("query refresh fixture: %v\nstderr: %s", refresh.err, refresh.stderr)
	}
	historyResult := runStateQueryCLI(t, context.Background(), "history", "growth", "--format=json", root)
	if historyResult.err != nil {
		t.Fatalf("history fixture: %v\nstderr: %s", historyResult.err, historyResult.stderr)
	}

	statusJSON := runStateQueryCLI(t, context.Background(), "state", "--format=json", "status")
	if statusJSON.err != nil {
		t.Fatal(statusJSON.err)
	}
	var summaries []stateSummary
	if err := json.Unmarshal(statusJSON.stdout, &summaries); err != nil {
		t.Fatalf("decode state status: %v\n%s", err, statusJSON.stdout)
	}
	if len(summaries) != 2 || summaries[0].Kind != stateKindIndex || summaries[1].Kind != stateKindHistory {
		t.Fatalf("state status summaries = %#v", summaries)
	}
	for _, summary := range summaries {
		if !summary.Exists || !summary.Owned || summary.Entries != 1 || summary.Valid != 1 || summary.Unsafe != 0 || summary.SizeBytes <= 0 || !summary.SizeComplete {
			t.Errorf("state status %s = %#v", summary.Kind, summary)
		}
	}

	listJSON := runStateQueryCLI(t, context.Background(), "state", "--format=json", "list")
	if listJSON.err != nil {
		t.Fatal(listJSON.err)
	}
	var entries []stateEntry
	if err := json.Unmarshal(listJSON.stdout, &entries); err != nil {
		t.Fatalf("decode state list: %v\n%s", err, listJSON.stdout)
	}
	if len(entries) != 2 {
		t.Fatalf("state list entries = %#v", entries)
	}
	for _, entry := range entries {
		if !entry.Valid || !entry.Safe || !entry.Complete || entry.Root != root || entry.SizeBytes <= 0 {
			t.Errorf("state list entry = %#v", entry)
		}
	}

	sizeJSON := runStateQueryCLI(t, context.Background(), "state", "--format=json", "size")
	if sizeJSON.err != nil {
		t.Fatal(sizeJSON.err)
	}
	var sizes []stateSummary
	if err := json.Unmarshal(sizeJSON.stdout, &sizes); err != nil || len(sizes) != 2 {
		t.Fatalf("state size JSON = %q, decoded %#v, error %v", sizeJSON.stdout, sizes, err)
	}

	for _, subcommand := range []string{"status", "list", "size"} {
		textResult := runStateQueryCLI(t, context.Background(), "state", subcommand)
		if textResult.err != nil {
			t.Fatalf("state %s: %v", subcommand, textResult.err)
		}
		if !bytes.Contains(textResult.stdout, []byte(stateKindIndex)) || !bytes.Contains(textResult.stdout, []byte(stateKindHistory)) {
			t.Fatalf("state %s text omitted a kind: %q", subcommand, textResult.stdout)
		}
	}
}

func TestStateKindDoesNotOpenUnselectedStore(t *testing.T) {
	env := isolateStateQueryCLI(t)
	badCache := filepath.Join(env.base, "cache-is-a-file")
	badHistory := filepath.Join(env.base, "history-is-a-file")
	if err := os.WriteFile(badCache, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badHistory, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		kind         string
		selected     string
		unselected   string
		selectedFlag string
		badFlag      string
	}{
		{kind: stateKindIndex, selected: filepath.Join(env.base, "missing-index"), unselected: badHistory, selectedFlag: "--cache-store=", badFlag: "--history-store="},
		{kind: stateKindHistory, selected: filepath.Join(env.base, "missing-history"), unselected: badCache, selectedFlag: "--history-store=", badFlag: "--cache-store="},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			result := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind="+tt.kind,
				tt.selectedFlag+tt.selected, tt.badFlag+tt.unselected, "status")
			if result.err != nil {
				t.Fatalf("selected %s store failed because of unselected store: %v", tt.kind, result.err)
			}
			var summaries []stateSummary
			if err := json.Unmarshal(result.stdout, &summaries); err != nil || len(summaries) != 1 || summaries[0].Kind != tt.kind {
				t.Fatalf("state status = %q, decoded %#v, error %v", result.stdout, summaries, err)
			}
		})
	}
}

func TestStateReadOnlyCommandsReportUnsafeStoreBoundary(t *testing.T) {
	env := isolateStateQueryCLI(t)
	target := filepath.Join(env.base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(env.base, "cache-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	status := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+link, "status")
	if status.err != nil {
		t.Fatalf("state status returned an unstructured boundary error: %v", status.err)
	}
	var summaries []stateSummary
	if err := json.Unmarshal(status.stdout, &summaries); err != nil || len(summaries) != 1 {
		t.Fatalf("decode state status %q: summaries=%#v error=%v", status.stdout, summaries, err)
	}
	if summaries[0].Safe || summaries[0].Managed || summaries[0].InventoryComplete || summaries[0].SizeComplete || !summaries[0].Exists || !strings.Contains(summaries[0].Issue, "symlink") {
		t.Fatalf("unsafe state status = %#v", summaries[0])
	}

	list := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+link, "list")
	if list.err != nil {
		t.Fatalf("state list returned an unstructured boundary error: %v", list.err)
	}
	var entries []stateEntry
	if err := json.Unmarshal(list.stdout, &entries); err != nil || len(entries) != 1 || entries[0].Safe || !strings.Contains(entries[0].Issue, "symlink") {
		t.Fatalf("unsafe state list = %q entries=%#v error=%v", list.stdout, entries, err)
	}

	if _, err := os.Lstat(filepath.Join(target, ".dirstat.lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only boundary inspection created a lock through the symlink: %v", err)
	}
}

func TestStateMutationsRequireExactlyOneAuthorizationFlag(t *testing.T) {
	isolateStateQueryCLI(t)
	for _, subcommand := range []string{"prune", "clear", "migrate"} {
		for _, flags := range [][]string{nil, {"--dry-run", "--yes"}} {
			args := append([]string{"state", subcommand}, flags...)
			result := runStateQueryCLI(t, context.Background(), args...)
			if result.err == nil || !strings.Contains(result.err.Error(), "exactly one of --dry-run or --yes") {
				t.Fatalf("%v error = %v", args, result.err)
			}
		}
	}
}

func TestStateMissingStoreMutationsAreIdempotentAndNonCreating(t *testing.T) {
	env := isolateStateQueryCLI(t)
	cacheStore := filepath.Join(env.base, "missing-cache")
	historyStore := filepath.Join(env.base, "missing-history")
	for _, subcommand := range []string{"prune", "clear", "migrate"} {
		for _, authorization := range []string{"--dry-run", "--yes"} {
			result := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--cache-store="+cacheStore,
				"--history-store="+historyStore, subcommand, authorization)
			if result.err != nil {
				t.Fatalf("state %s %s on missing stores: %v", subcommand, authorization, result.err)
			}
			if string(bytes.TrimSpace(result.stdout)) != "[]" {
				t.Fatalf("state %s %s output = %q, want []", subcommand, authorization, result.stdout)
			}
			for _, path := range []string{cacheStore, historyStore} {
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("state %s %s created %q: %v", subcommand, authorization, path, err)
				}
			}
		}
	}
}

func TestStateRejectsUnmanagedStoreAndPreservesForeignData(t *testing.T) {
	for _, subcommand := range []string{"prune", "clear", "migrate"} {
		t.Run(subcommand, func(t *testing.T) {
			env := isolateStateQueryCLI(t)
			store := filepath.Join(env.base, "unmanaged")
			if err := os.Mkdir(store, 0o700); err != nil {
				t.Fatal(err)
			}
			foreign := filepath.Join(store, "operator-notes.txt")
			if err := os.WriteFile(foreign, []byte("do not delete"), 0o600); err != nil {
				t.Fatal(err)
			}
			result := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+store,
				subcommand, "--yes")
			if result.err == nil {
				t.Fatalf("state %s adopted or mutated a foreign store", subcommand)
			}
			if data, err := os.ReadFile(foreign); err != nil || string(data) != "do not delete" {
				t.Fatalf("foreign data after state %s = %q, %v", subcommand, data, err)
			}
		})
	}
}

func TestStatePruneAndClearRemoveCorruptOwnedEntriesButNotForeignOrSymlinkEntries(t *testing.T) {
	for _, subcommand := range []string{"prune", "clear"} {
		t.Run(subcommand, func(t *testing.T) {
			env := isolateStateQueryCLI(t)
			store := filepath.Join(env.base, "index")
			makeOwnedIndexStore(t, store)
			corrupt := filepath.Join(store, strings.Repeat("a", 32)+".bin")
			foreign := filepath.Join(store, "foreign.txt")
			if err := os.WriteFile(corrupt, []byte("not a snapshot"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
			symlink := filepath.Join(store, strings.Repeat("b", 32)+".bin")
			symlinkCreated := false
			if runtime.GOOS != windowsOS {
				outside := filepath.Join(env.base, "outside")
				if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, symlink); err == nil {
					symlinkCreated = true
				}
			}

			dryRun := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+store,
				subcommand, "--dry-run")
			if dryRun.err != nil {
				t.Fatalf("state %s --dry-run: %v", subcommand, dryRun.err)
			}
			var dryActions []stateAction
			if err := json.Unmarshal(dryRun.stdout, &dryActions); err != nil || len(dryActions) != 1 || dryActions[0].Entry.ID != filepath.Base(corrupt) || !dryActions[0].DryRun || dryActions[0].Removed {
				t.Fatalf("dry-run actions = %q, decoded %#v, error %v", dryRun.stdout, dryActions, err)
			}
			for _, path := range []string{corrupt, foreign} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("dry-run removed %q: %v", path, err)
				}
			}

			apply := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+store,
				subcommand, "--yes")
			if apply.err != nil {
				t.Fatalf("state %s --yes: %v", subcommand, apply.err)
			}
			var actions []stateAction
			if err := json.Unmarshal(apply.stdout, &actions); err != nil || len(actions) != 1 || !actions[0].Removed || actions[0].DryRun {
				t.Fatalf("apply actions = %q, decoded %#v, error %v", apply.stdout, actions, err)
			}
			if _, err := os.Lstat(corrupt); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("corrupt entry still exists: %v", err)
			}
			if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign" {
				t.Fatalf("foreign entry changed: %q, %v", data, err)
			}
			if symlinkCreated {
				if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("unsafe symlink entry changed: %v, %v", info, err)
				}
			}
		})
	}
}

func TestStateRetainsFirstKindActionsWhenSecondKindFails(t *testing.T) {
	env := isolateStateQueryCLI(t)
	cacheStore := filepath.Join(env.base, "index")
	historyStore := filepath.Join(env.base, "foreign-history")
	makeOwnedIndexStore(t, cacheStore)
	corrupt := filepath.Join(cacheStore, strings.Repeat("c", 32)+".bin")
	if err := os.WriteFile(corrupt, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(historyStore, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(historyStore, "foreign")
	if err := os.WriteFile(foreign, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--cache-store="+cacheStore,
		"--history-store="+historyStore, "clear", "--yes")
	if result.err == nil || !strings.Contains(result.err.Error(), "history") {
		t.Fatalf("state clear error = %v", result.err)
	}
	var actions []stateAction
	if err := json.Unmarshal(result.stdout, &actions); err != nil || len(actions) != 1 || actions[0].Kind != stateKindIndex || !actions[0].Removed {
		t.Fatalf("partial actions = %q, decoded %#v, error %v", result.stdout, actions, err)
	}
	if _, err := os.Lstat(corrupt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("completed cache action was not applied: %v", err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "preserve" {
		t.Fatalf("failing history store changed: %q, %v", data, err)
	}
}

func TestStateMutationHonorsCanceledContextBeforeRemoval(t *testing.T) {
	env := isolateStateQueryCLI(t)
	store := filepath.Join(env.base, "index")
	makeOwnedIndexStore(t, store)
	corrupt := filepath.Join(store, strings.Repeat("d", 32)+".bin")
	if err := os.WriteFile(corrupt, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := runStateQueryCLI(t, ctx, "state", "--format=json", "--kind=index", "--cache-store="+store, "clear", "--yes")
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled clear error = %v", result.err)
	}
	if _, err := os.Lstat(corrupt); err != nil {
		t.Fatalf("canceled clear removed entry: %v", err)
	}
}

func TestStateIndexMigrateDryRunApplyAndIdempotence(t *testing.T) {
	env := isolateStateQueryCLI(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	refresh := runStateQueryCLI(t, context.Background(), "query", "--index=refresh", "--kind=file", root)
	if refresh.err != nil {
		t.Fatalf("create legacy index fixture: %v\n%s", refresh.err, refresh.stderr)
	}
	store := filepath.Join(env.cacheHome, "dirstat")
	marker := filepath.Join(store, ".dirstat-store")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	snapshot := singleMatchingFile(t, store, "*.bin")
	before, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	dryRun := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+store,
		"migrate", "--dry-run")
	if dryRun.err != nil {
		t.Fatalf("index migrate dry-run: %v", dryRun.err)
	}
	assertSingleStateAction(t, dryRun.stdout, stateKindIndex, "adopt", true)
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("migration dry-run wrote marker: %v", err)
	}
	if after, err := os.ReadFile(snapshot); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("migration dry-run changed snapshot: equal=%t error=%v", bytes.Equal(after, before), err)
	}

	apply := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+store,
		"migrate", "--yes")
	if apply.err != nil {
		t.Fatalf("index migrate --yes: %v", apply.err)
	}
	assertSingleStateAction(t, apply.stdout, stateKindIndex, "adopt", false)
	if info, err := os.Lstat(marker); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("migration did not publish ownership marker: %v, %v", info, err)
	}
	if after, err := os.ReadFile(snapshot); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("migration changed valid snapshot: equal=%t error=%v", bytes.Equal(after, before), err)
	}

	repeat := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+store,
		"migrate", "--yes")
	if repeat.err != nil || string(bytes.TrimSpace(repeat.stdout)) != "[]" {
		t.Fatalf("idempotent index migrate output=%q error=%v", repeat.stdout, repeat.err)
	}
}

func TestStateHistoryMigrateDryRunApplyAndIdempotence(t *testing.T) {
	env := isolateStateQueryCLI(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheStore := filepath.Join(env.base, "legacy-cache")
	legacyStore := filepath.Join(cacheStore, "history")
	destination := filepath.Join(env.base, "durable-history")
	fixture := runStateQueryCLI(t, context.Background(), "history", "--store="+legacyStore, "growth", "--format=json", root)
	if fixture.err != nil {
		t.Fatalf("create legacy history fixture: %v\n%s", fixture.err, fixture.stderr)
	}
	if err := os.Remove(filepath.Join(legacyStore, ".dirstat-store")); err != nil {
		t.Fatal(err)
	}
	legacySnapshot := singleMatchingFile(t, legacyStore, "*/*.bin")
	before, err := os.ReadFile(legacySnapshot)
	if err != nil {
		t.Fatal(err)
	}

	dryRun := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=history", "--cache-store="+cacheStore,
		"--history-store="+destination, "migrate", "--dry-run")
	if dryRun.err != nil {
		t.Fatalf("history migrate dry-run: %v", dryRun.err)
	}
	assertStateActionReasons(t, dryRun.stdout, stateKindHistory, true, "initialize", "adopt", "migrate")
	if _, err := os.Lstat(destination); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("history migration dry-run created destination: %v", err)
	}
	if after, err := os.ReadFile(legacySnapshot); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("history migration dry-run changed source: equal=%t error=%v", bytes.Equal(after, before), err)
	}

	apply := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=history", "--cache-store="+cacheStore,
		"--history-store="+destination, "migrate", "--yes")
	if apply.err != nil {
		t.Fatalf("history migrate --yes: %v", apply.err)
	}
	assertStateActionReasons(t, apply.stdout, stateKindHistory, false, "initialize", "adopt", "migrate")
	if _, err := os.Lstat(legacySnapshot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("history migration retained source record: %v", err)
	}
	if got := singleMatchingFile(t, destination, "*/*.bin"); got == "" {
		t.Fatal("history migration did not publish destination record")
	}

	repeat := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=history", "--cache-store="+cacheStore,
		"--history-store="+destination, "migrate", "--yes")
	if repeat.err != nil || string(bytes.TrimSpace(repeat.stdout)) != "[]" {
		t.Fatalf("idempotent history migrate output=%q error=%v", repeat.stdout, repeat.err)
	}
}

func TestStateAllMigrationLeavesCacheInventoryClean(t *testing.T) {
	env := isolateStateQueryCLI(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheStore := filepath.Join(env.base, "legacy-cache")
	legacyStore := filepath.Join(cacheStore, "history")
	destination := filepath.Join(env.base, "durable-history")
	fixture := runStateQueryCLI(t, context.Background(), "history", "--store="+legacyStore, "growth", "--format=json", root)
	if fixture.err != nil {
		t.Fatal(fixture.err)
	}
	if err := os.Remove(filepath.Join(legacyStore, ".dirstat-store")); err != nil {
		t.Fatal(err)
	}
	apply := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--cache-store="+cacheStore,
		"--history-store="+destination, "migrate", "--yes")
	if apply.err != nil {
		t.Fatalf("all-state migration: %v\n%s", apply.err, apply.stdout)
	}
	status := runStateQueryCLI(t, context.Background(), "state", "--format=json", "--kind=index", "--cache-store="+cacheStore, "status")
	if status.err != nil {
		t.Fatal(status.err)
	}
	var summaries []stateSummary
	if err := json.Unmarshal(status.stdout, &summaries); err != nil || len(summaries) != 1 {
		t.Fatalf("cache status after migration = %q, decoded=%#v error=%v", status.stdout, summaries, err)
	}
	if summary := summaries[0]; !summary.Owned || !summary.Managed || !summary.Safe || !summary.InventoryComplete || summary.Entries != 0 || summary.Unsafe != 0 {
		t.Fatalf("cache status after migration = %#v", summary)
	}
}

type stateQueryCLIEnv struct {
	base       string
	home       string
	cacheHome  string
	stateHome  string
	configHome string
}

func isolateStateQueryCLI(t *testing.T) stateQueryCLIEnv {
	t.Helper()
	base := t.TempDir()
	env := stateQueryCLIEnv{
		base:       base,
		home:       filepath.Join(base, "home"),
		cacheHome:  filepath.Join(base, "cache"),
		stateHome:  filepath.Join(base, "state"),
		configHome: filepath.Join(base, "config"),
	}
	t.Setenv("HOME", env.home)
	t.Setenv("XDG_CACHE_HOME", env.cacheHome)
	t.Setenv("XDG_STATE_HOME", env.stateHome)
	t.Setenv("XDG_CONFIG_HOME", env.configHome)
	return env
}

type stateQueryCLIResult struct {
	stdout []byte
	stderr string
	err    error
}

func runStateQueryCLI(t *testing.T, ctx context.Context, args ...string) stateQueryCLIResult {
	t.Helper()
	cmd := New()
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	return stateQueryCLIResult{stdout: bytes.Clone(stdout.Bytes()), stderr: stderr.String(), err: err}
}

func makeOwnedIndexStore(t *testing.T, dir string) {
	t.Helper()
	store, err := index.NewStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("index store is nil")
	}
}

func singleMatchingFile(t *testing.T, root, pattern string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	matches, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("glob %q below %q matched %v", pattern, root, matches)
	}
	return matches[0]
}

func assertSingleStateAction(t *testing.T, raw []byte, kind, reason string, dryRun bool) {
	t.Helper()
	var actions []stateAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		t.Fatalf("decode state actions %q: %v", raw, err)
	}
	if len(actions) != 1 || actions[0].Kind != kind || actions[0].Reason != reason || actions[0].DryRun != dryRun {
		t.Fatalf("state actions = %#v, want one kind=%s reason=%s dry_run=%t", actions, kind, reason, dryRun)
	}
}

func assertStateActionReasons(t *testing.T, raw []byte, kind string, dryRun bool, reasons ...string) {
	t.Helper()
	var actions []stateAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		t.Fatalf("decode state actions %q: %v", raw, err)
	}
	if len(actions) != len(reasons) {
		t.Fatalf("state actions = %#v, want reasons %v", actions, reasons)
	}
	for i, reason := range reasons {
		if actions[i].Kind != kind || actions[i].Reason != reason || actions[i].DryRun != dryRun {
			t.Fatalf("state action %d = %#v, want kind=%s reason=%s dry_run=%t", i, actions[i], kind, reason, dryRun)
		}
	}
}
