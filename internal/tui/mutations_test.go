package tui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestExactDeletePersistsPatchedCache(t *testing.T) {
	isolateUserCache(t)
	path, root := mkTree(t)
	stats := scan.Stats{Files: root.FileCount, Dirs: root.DirCount + 1}
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{UseCache: true, DisableAudit: true}))
	if m.store == nil {
		t.Fatalf("cache store is unavailable: %v", m.cacheErr)
	}
	updated, _ := m.Update(scanDoneMsg{node: root, stats: stats})
	m = updated.(*model)

	target := filepath.Join(path, "big.bin")
	entry, err := fsinfo.Inspect(target, false)
	if err != nil {
		t.Fatal(err)
	}
	m.queue = []fsops.Operation{{ID: "delete", Action: fsops.ActionDelete, Source: target, Expected: &entry}}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	updated, cmd := m.Update(appliedMsg{results: []fsops.Result{{
		OperationID: "delete", Action: fsops.ActionDelete, Status: "ok", FinishedAt: time.Now(),
	}}})
	m = updated.(*model)
	if cmd == nil || m.scanning {
		t.Fatalf("exact delete command=%v scanning=%t, want cache/inspection commands without scan", cmd != nil, m.scanning)
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("exact cached delete did not schedule a cache save batch")
	}
	saved := false
	for _, batched := range batch {
		msg := batched()
		if result, ok := msg.(cacheSavedMsg); ok {
			saved = true
			if result.err != nil {
				t.Fatalf("save patched cache: %v", result.err)
			}
		}
	}
	if !saved {
		t.Fatal("exact delete batch omitted the cache save")
	}

	snapshot, err := m.store.Load(m.rootAbs, m.fingerprint)
	if err != nil {
		t.Fatalf("load patched cache: %v", err)
	}
	if snapshot.Files != stats.Files-1 {
		t.Fatalf("cached files = %d, want %d", snapshot.Files, stats.Files-1)
	}
	found := false
	snapshot.ToTree().Walk(func(node *tree.Node) bool {
		if node.Path() == "big.bin" {
			found = true
		}
		return true
	})
	if found {
		t.Fatal("deleted file remains in the persisted cache tree")
	}
}

func TestDeletingHardlinkOwnerUpdatesImmediatelyAndReconciles(t *testing.T) {
	path := t.TempDir()
	first := filepath.Join(path, "first.bin")
	second := filepath.Join(path, "second.bin")
	if err := os.WriteFile(first, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hardlinks are unavailable: %v", err)
	}
	root, stats, err := scan.Scan(context.Background(), path, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: root, stats: stats})

	var owner *tree.Node
	for _, child := range m.root.Children {
		if !child.IsDir && !child.Hardlink {
			owner = child
			break
		}
	}
	if owner == nil {
		t.Fatal("scan did not identify a hardlink byte owner")
	}
	target := filepath.Join(path, owner.Name)
	entry, err := fsinfo.Inspect(target, false)
	if err != nil {
		t.Fatal(err)
	}
	m.queue = []fsops.Operation{{ID: "delete-owner", Action: fsops.ActionDelete, Source: target, Expected: &entry}}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	updated, cmd := m.Update(appliedMsg{results: []fsops.Result{{
		OperationID: "delete-owner", Action: fsops.ActionDelete, Status: "ok", FinishedAt: time.Now(),
	}}})
	m = updated.(*model)
	if m.findNode(target) != nil {
		t.Fatal("deleted hardlink remains in the immediate tree")
	}
	if cmd == nil || !m.scanning {
		t.Fatal("ambiguous hardlink ownership did not start reconciliation")
	}
	m.cancelScan()
}

func TestApplyInterruptsOldScanAndReconcilesFromPatchedTree(t *testing.T) {
	path, root := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: root})
	_ = m.startScan()
	oldGeneration := m.scanGeneration
	m.pauseScanForApply()
	if m.scanning || !m.applyNeedsScan || m.scanGeneration != oldGeneration+1 {
		t.Fatalf("paused scan state: scanning=%t needs_scan=%t generation=%d", m.scanning, m.applyNeedsScan, m.scanGeneration)
	}

	target := filepath.Join(path, "a.go")
	entry, err := fsinfo.Inspect(target, false)
	if err != nil {
		t.Fatal(err)
	}
	m.queue = []fsops.Operation{{ID: "delete", Action: fsops.ActionDelete, Source: target, Expected: &entry}}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(appliedMsg{results: []fsops.Result{{
		OperationID: "delete", Action: fsops.ActionDelete, Status: "ok", FinishedAt: time.Now(),
	}}})
	m = updated.(*model)
	if m.findNode(target) != nil {
		t.Fatal("delete was not reflected before reconciliation")
	}
	if cmd == nil || !m.scanning || m.scanGeneration != oldGeneration+2 {
		t.Fatalf("reconciliation state: command=%t scanning=%t generation=%d", cmd != nil, m.scanning, m.scanGeneration)
	}
	m.cancelScan()
}

func TestDeleteInvalidatesInspectionStartedBeforeMutation(t *testing.T) {
	path, root := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: root})
	for i, row := range m.rows {
		if row.node.Name == "a.go" {
			m.cursor = i
			break
		}
	}
	target := m.selectedAbsolutePath()
	entry, err := fsinfo.Inspect(target, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.requestInspect() == nil {
		t.Fatal("inspection was not scheduled")
	}
	staleGeneration := m.inspectGeneration
	m.queue = []fsops.Operation{{ID: "delete", Action: fsops.ActionDelete, Source: target, Expected: &entry}}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(appliedMsg{results: []fsops.Result{{
		OperationID: "delete", Action: fsops.ActionDelete, Status: "ok", FinishedAt: time.Now(),
	}}})
	m = updated.(*model)
	if m.inspectGeneration <= staleGeneration {
		t.Fatal("delete did not invalidate the pre-mutation inspection")
	}
	m = asModel(t, m, inspectMsg{generation: staleGeneration, path: target, entry: entry})
	if m.inspectPath == target {
		t.Fatal("stale pre-mutation metadata replaced the post-delete inspection")
	}
}

func isolateUserCache(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LocalAppData", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	default:
		t.Setenv("XDG_CACHE_HOME", dir)
	}
}
