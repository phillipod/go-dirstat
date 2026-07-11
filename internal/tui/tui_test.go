package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/fsinfo"
	"github.com/phillipod/go-dirstat/internal/fsops"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

// asModel drives a model through Update and returns the concrete *model,
// collapsing the tea.Model interface in tests.
func asModel(t *testing.T, m tea.Model, msg tea.Msg) *model {
	t.Helper()
	mm, _ := m.Update(msg)
	return mm.(*model)
}

func mkTree(t *testing.T) (string, *tree.Node) {
	t.Helper()
	root := t.TempDir()
	must := func(p string, n int) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(make([]byte, n)); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(root, "big.bin"), 5000)
	must(filepath.Join(root, "a.go"), 100)
	must(filepath.Join(root, "sub", "c.md"), 50)
	must(filepath.Join(root, "sub", "deep", "d.txt"), 20)

	node, _, err := scan.Scan(context.Background(), root, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	return root, node
}

func TestModelScanAndTreeRender(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)

	m = asModel(t, m, scanDoneMsg{node: node})
	if !m.gotData {
		t.Fatal("model has no data after scan")
	}
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})

	v := m.View()
	for _, want := range []string{"big.bin", "sub/", "a.go", "move"} {
		if !contains(v, want) {
			t.Errorf("View missing %q", want)
		}
	}
}

func TestModelExpandCollapseNavigate(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})

	// Root is expanded, but sub is not, so deep must initially be hidden.
	if contains(m.View(), "deep/") {
		t.Fatal("deep/ is visible before sub is expanded")
	}
	for i, r := range m.rows {
		if r.node.Name == "sub" {
			m.cursor = i
			break
		}
	}
	if m.currentRow() == nil || m.currentRow().node.Name != "sub" {
		t.Fatal("sub row not found")
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !contains(m.View(), "deep/") {
		t.Error("expanding sub did not reveal deep/")
	}
}

func TestModelExtView(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyTab}) // -> ext
	if m.view != viewExt {
		t.Fatalf("view = %v, want ext", m.view)
	}
	if !contains(m.View(), ".go") {
		t.Error("ext view missing .go")
	}
}

func TestModelLargestFilesView(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = asModel(t, m, key("f"))

	if m.view != viewLargest {
		t.Fatalf("view = %v, want largest files", m.view)
	}
	if len(m.topRows) == 0 || m.topRows[0].file.Rel != "big.bin" {
		t.Fatalf("largest rows = %+v, want big.bin first", m.topRows)
	}
	for _, want := range []string{"big.bin", "largest files", "top:100"} {
		if !contains(m.View(), want) {
			t.Errorf("largest-files view missing %q\n%s", want, m.View())
		}
	}
}

func TestShiftTabCyclesViewsBackward(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.view != viewLargest {
		t.Fatalf("shift+tab from tree = %v, want largest files", m.view)
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.view != viewExt {
		t.Fatalf("shift+tab from largest = %v, want extensions", m.view)
	}
}

func TestModelSortCycle(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})

	if m.sort != tree.SortSizeDesc {
		t.Fatalf("default sort = %v, want size-desc", m.sort)
	}
	m = asModel(t, m, key("s"))
	if m.sort != tree.SortCountDesc {
		t.Errorf("after one 's', sort = %v, want count", m.sort)
	}
}

func TestPageKeysUseBubbleTeaKeyNames(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 20}
	for i := 0; i < 20; i++ {
		root.Adopt(&tree.Node{Name: "file" + string(rune('a'+i)), Apparent: 1})
	}
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 80, Height: 8})
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.cursor != m.page() {
		t.Fatalf("page down cursor = %d, want %d", m.cursor, m.page())
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.cursor != 0 {
		t.Fatalf("page up cursor = %d, want 0", m.cursor)
	}
}

func TestRebuildSortsRowsWithoutMutatingMeasuredTree(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 110}
	root.Adopt(&tree.Node{Name: "a", Apparent: 10})
	root.Adopt(&tree.Node{Name: "z", Apparent: 100})
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})

	if got := m.rows[1].node.Name; got != "z" {
		t.Fatalf("first displayed child = %q, want size-sorted z", got)
	}
	if got := root.Children[0].Name; got != "a" {
		t.Fatalf("rebuild mutated measured-tree order: first child = %q, want a", got)
	}
}

func TestExtensionSortCycleIsEffectiveAndLabeled(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 1600}
	root.Adopt(&tree.Node{Name: "large.go", Apparent: 1000})
	for i := 0; i < 3; i++ {
		root.Adopt(&tree.Node{Name: "part" + string(rune('a'+i)) + ".txt", Apparent: 200})
	}
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = asModel(t, m, key("e"))

	if got := m.extRows[0].ext.Ext; got != ".go" {
		t.Fatalf("size-sorted top extension = %q, want .go", got)
	}
	m = asModel(t, m, key("s"))
	if m.extSort != extSortCount {
		t.Fatalf("extension sort = %v, want count", m.extSort)
	}
	if got := m.extRows[0].ext.Ext; got != ".txt" {
		t.Fatalf("count-sorted top extension = %q, want .txt", got)
	}
	if !contains(m.headerView(), "sort:count") {
		t.Errorf("extension header does not label active count sort\n%s", m.headerView())
	}
}

func TestModelSizeModeToggle(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = asModel(t, m, key("m"))
	if m.sizeMode != tree.SizeOnDisk {
		t.Errorf("size mode after 'm' = %v, want on-disk", m.sizeMode)
	}
}

func TestExtensionRowsHonorSizeMode(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 1100, Alloc: 2100}
	root.Adopt(&tree.Node{Name: "logical.go", Apparent: 1000, Alloc: 100})
	root.Adopt(&tree.Node{Name: "allocated.md", Apparent: 100, Alloc: 2000})
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})

	if got := m.extRows[0].ext.Ext; got != ".go" {
		t.Fatalf("apparent-size top extension = %q, want .go", got)
	}
	m = asModel(t, m, key("m"))
	if got := m.extRows[0].ext.Ext; got != ".md" {
		t.Fatalf("on-disk top extension = %q, want .md", got)
	}
	m.view = viewExt
	m.width, m.height = 100, 24
	if body := m.extBody(); !contains(body, format.Bytes(2000)) {
		t.Errorf("on-disk extension body missing %q\n%s", format.Bytes(2000), body)
	}
}

func TestExtensionDetailReportsSizeAndPercentage(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 1000}
	root.Adopt(&tree.Node{Name: "large.go", Apparent: 900})
	root.Adopt(&tree.Node{Name: "small.md", Apparent: 100})
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = asModel(t, m, key("e"))

	detail := m.detailLine()
	for _, want := range []string{".go", format.Bytes(900), "1 files", "90.0% of total"} {
		if !contains(detail, want) {
			t.Errorf("extension detail missing %q: %s", want, detail)
		}
	}
}

func TestDirectoryExtensionDetailUsesDirectoryLabel(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Alloc: 4096}
	app := New("/root", scope.New(), tree.SizeOnDisk, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = asModel(t, m, key("e"))

	if detail := m.detailLine(); !contains(detail, "1 directories") || contains(detail, "1 files") {
		t.Fatalf("directory extension detail mislabeled: %s", detail)
	}
}

func TestSelectionFollowsPathAcrossProgressReordering(t *testing.T) {
	first := &tree.Node{Name: "root", IsDir: true, Apparent: 110}
	first.Adopt(&tree.Node{Name: "z", Apparent: 100})
	first.Adopt(&tree.Node{Name: "a", Apparent: 10})
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: first})
	for i := range m.rows {
		if m.rows[i].node.Name == "a" {
			m.cursor = i
			break
		}
	}
	m.rememberSelection()

	// The same path moves ahead of z as its running total grows.
	progress := &tree.Node{Name: "root", IsDir: true, Apparent: 210}
	progress.Adopt(&tree.Node{Name: "z", Apparent: 10})
	progress.Adopt(&tree.Node{Name: "a", Apparent: 200})
	m = asModel(t, m, progressMsg{node: progress})
	if row := m.currentRow(); row == nil || row.node.Name != "a" {
		got := "<nil>"
		if row != nil {
			got = row.node.Name
		}
		t.Fatalf("selection jumped after live reorder: got %q, want a", got)
	}
}

func TestViewSeparatesBodyFromFooter(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})

	if !contains(m.View(), m.treeBody()+"\n"+m.detailLine()) {
		t.Errorf("tree body and detail footer are not on separate lines\n%s", m.View())
	}
	m = asModel(t, m, key("e"))
	if !contains(m.View(), m.extBody()+"\n"+m.detailLine()) {
		t.Errorf("extension body and detail footer are not on separate lines\n%s", m.View())
	}
}

func TestNarrowViewDoesNotWrapRows(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 1000}
	root.Adopt(&tree.Node{Name: strings.Repeat("very-long-name-", 5) + ".go", Apparent: 1000})
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 32, Height: 12})

	for _, viewKey := range []string{"", "e", "f"} {
		if viewKey != "" {
			m = asModel(t, m, key(viewKey))
		}
		for _, line := range strings.Split(m.View(), "\n") {
			if width := lipgloss.Width(line); width > m.width {
				t.Errorf("view %q line width = %d, want <= %d: %q", viewKey, width, m.width, line)
			}
		}
	}
}

func TestTUIEscapesTerminalControlsInNames(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 1}
	root.Adopt(&tree.Node{Name: "evil\x1b]52;payload\a", Apparent: 1})
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 12})

	view := m.View()
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\a") {
		t.Fatalf("TUI contains raw terminal controls: %q", view)
	}
	if !strings.Contains(view, `\x1B]52`) || !strings.Contains(view, `\x07`) {
		t.Fatalf("TUI missing visible control escapes: %q", view)
	}
}

func TestTUIDetailShowsSelectedEntryError(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true}
	root.Adopt(&tree.Node{Name: "blocked", Err: errors.New("permission denied")})
	app := New("/root", scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: root})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 12})
	m.cursor = 1
	if detail := m.detailLine(); !contains(detail, "permission denied") {
		t.Fatalf("selected error detail is not actionable: %s", detail)
	}
}

func TestHelpFitsWindowHeight(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 40, Height: 8})
	m = asModel(t, m, key("?"))

	if lines := strings.Count(m.View(), "\n") + 1; lines > m.height {
		t.Fatalf("help renders %d lines in a %d-line window:\n%s", lines, m.height, m.View())
	}
}

func TestHelpIsModalAndReturnsToPriorView(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, key("e"))
	m = asModel(t, m, key("?"))
	if m.view != viewHelp || m.returnView != viewExt {
		t.Fatalf("help state = view %v / return %v", m.view, m.returnView)
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.view != viewHelp {
		t.Fatal("tab changed the hidden data view while help was open")
	}
	m = asModel(t, m, key("?"))
	if m.view != viewExt {
		t.Fatalf("help returned to %v, want extensions", m.view)
	}
}

func TestRescanCancelsPriorGenerationAndIgnoresStaleMessages(t *testing.T) {
	app := New(t.TempDir(), scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m.scanGeneration = 7
	oldCtx, oldCancel := context.WithCancel(context.Background())
	m.scanCancel = oldCancel
	before := m.root

	updated, cmd := m.Update(key("r"))
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("rescan command is nil")
	}
	if m.scanGeneration != 8 {
		t.Fatalf("scan generation = %d, want 8", m.scanGeneration)
	}
	select {
	case <-oldCtx.Done():
	default:
		t.Fatal("rescan did not cancel prior scan context")
	}

	stale := &tree.Node{Name: "stale", IsDir: true}
	m = asModel(t, m, progressMsg{generation: 7, node: stale})
	m = asModel(t, m, scanDoneMsg{generation: 7, node: stale})
	if m.root != before {
		t.Fatal("stale generation overwrote the current tree")
	}
	if !m.scanning {
		t.Fatal("stale completion stopped the current scan")
	}

	fresh := &tree.Node{Name: "fresh", IsDir: true}
	m = asModel(t, m, progressMsg{generation: 8, node: fresh})
	if m.root != fresh {
		t.Fatal("current generation progress was not adopted")
	}
	m = asModel(t, m, scanDoneMsg{generation: 8, node: fresh})
	if m.scanning {
		t.Fatal("current generation completion left scan marked in flight")
	}
}

func TestRefreshRetainsExistingTreeUntilCompletion(t *testing.T) {
	old := &tree.Node{Name: "old", IsDir: true, Apparent: 100}
	freshPartial := &tree.Node{Name: "partial", IsDir: true, Apparent: 10}
	freshFinal := &tree.Node{Name: "fresh", IsDir: true, Apparent: 200}
	app := New(t.TempDir(), scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m.root = old
	m.gotData = true
	m.scanGeneration = 7
	m.retainDuringScan = true

	m = asModel(t, m, progressMsg{generation: 7, node: freshPartial})
	if m.root != old {
		t.Fatal("refresh progress replaced the existing complete tree")
	}
	m = asModel(t, m, scanDoneMsg{generation: 7, node: freshFinal})
	if m.root != freshFinal || m.retainDuringScan {
		t.Fatal("refresh completion did not install the authoritative tree")
	}
}

func TestQuitCancelsActiveScan(t *testing.T) {
	app := New(t.TempDir(), scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	scanCtx, cancel := context.WithCancel(context.Background())
	m.scanCancel = cancel

	_, cmd := m.Update(key("q"))
	if cmd == nil {
		t.Fatal("quit command is nil")
	}
	select {
	case <-scanCtx.Done():
	default:
		t.Fatal("quit did not cancel active scan")
	}
}

func TestStopScanRetainsResultsAndInvalidatesGeneration(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 42}
	app := New(t.TempDir(), scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m.root = root
	m.gotData = true
	m.scanning = true
	m.scanGeneration = 4
	m.cacheNote = "cached 2m, refreshing…"
	scanCtx, cancel := context.WithCancel(context.Background())
	m.scanCancel = cancel

	m = asModel(t, m, key("c"))
	select {
	case <-scanCtx.Done():
	default:
		t.Fatal("stop did not cancel the active scan context")
	}
	if m.scanning {
		t.Fatal("stop left scanning=true")
	}
	if m.scanGeneration != 5 {
		t.Fatalf("generation after stop = %d, want 5", m.scanGeneration)
	}
	if m.root != root || !m.gotData {
		t.Fatal("stop discarded the retained tree")
	}
	if m.cacheNote != "" || m.scanNote != "scan stopped" {
		t.Fatalf("stop notes = cache %q / scan %q", m.cacheNote, m.scanNote)
	}

	stale := &tree.Node{Name: "stale", IsDir: true}
	m = asModel(t, m, progressMsg{generation: 4, node: stale})
	m = asModel(t, m, scanDoneMsg{generation: 4, node: stale, err: context.Canceled})
	if m.root != root || m.scanErr != nil {
		t.Fatal("canceled generation changed retained results")
	}
}

func TestCacheSaveErrorIsVisibleAndNonFatal(t *testing.T) {
	app := New(t.TempDir(), scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m.scanGeneration = 3
	m.width, m.height = 160, 24
	want := errors.New("disk full")

	m = asModel(t, m, cacheSavedMsg{generation: 3, err: want})
	if !errors.Is(m.cacheErr, want) {
		t.Fatalf("cache error = %v, want wrapped disk-full error", m.cacheErr)
	}
	if !contains(m.View(), "cache save failed") {
		t.Errorf("cache error not visible in TUI\n%s", m.View())
	}
	if m.scanErr != nil {
		t.Fatalf("cache error incorrectly failed scan: %v", m.scanErr)
	}

	m = asModel(t, m, cacheSavedMsg{generation: 2})
	if m.cacheErr == nil {
		t.Fatal("stale cache completion cleared current error")
	}
	m = asModel(t, m, cacheSavedMsg{generation: 3})
	if m.cacheErr != nil {
		t.Fatalf("successful cache save did not clear error: %v", m.cacheErr)
	}
}

func TestRefreshErrorRemainsVisibleWithExistingData(t *testing.T) {
	path, node := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, tea.WindowSizeMsg{Width: 160, Height: 24})
	m.scanning = true
	m = asModel(t, m, scanDoneMsg{err: errors.New("refresh failed")})

	if !contains(m.View(), "scan failed: refresh failed") {
		t.Errorf("refresh error is hidden while existing data remains visible\n%s", m.View())
	}
}

// TestModelStreamsProgress checks the progressive UX: the view renders the
// chrome and an explicit status immediately, then adopts streamed snapshots as
// they arrive instead of blocking behind a loading screen.
func TestModelStreamsProgress(t *testing.T) {
	path, full := mkTree(t)
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{})
	m := newModel(app)
	m = asModel(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})

	// The regular tree chrome is visible immediately and clearly says that its
	// running totals are still scanning.
	if !contains(m.View(), "scanning") || !contains(m.View(), "dirstat") {
		t.Fatalf("initial view does not show progressive scan status\n%s", m.View())
	}

	// A partial snapshot lands mid-scan: only "sub" is known so far. Built with
	// AddChild so parent links (and thus Path()) are wired, as a real scan would.
	partial := &tree.Node{
		Name: filepath.Base(path), IsDir: true, Depth: 0,
		Apparent: 50, FileCount: 1,
	}
	partial.AddChild(&tree.Node{Name: "sub", IsDir: true, Apparent: 50})
	m = asModel(t, m, progressMsg{node: partial, stats: scan.Stats{Files: 1}})
	if !contains(m.View(), "sub/") {
		t.Error("view did not adopt streamed snapshot (missing sub/)")
	}

	// The final, complete tree supersedes the snapshot.
	m = asModel(t, m, scanDoneMsg{node: full})
	if !contains(m.View(), "big.bin") {
		t.Error("view did not render final tree")
	}
	if contains(m.headerView(), "scanning") {
		t.Errorf("completed scan still shows scanning status: %s", m.headerView())
	}
}

func TestF8StagesGuardedDeleteAndTypedApply(t *testing.T) {
	path, node := mkTree(t)
	auditPath := filepath.Join(t.TempDir(), "tui-audit.jsonl")
	app := New(path, scope.New(), tree.SizeApparent, 0, Options{AuditPath: auditPath})
	m := newModel(app)
	m = asModel(t, m, scanDoneMsg{node: node})
	for i := range m.rows {
		if m.rows[i].node.Name == "big.bin" {
			m.cursor = i
			break
		}
	}
	target := m.selectedAbsolutePath()
	targetNode := m.findNode(target)
	if targetNode == nil {
		t.Fatal("selected delete target is absent from the measured tree")
	}
	beforeApparent, beforeAlloc := m.root.Apparent, m.root.Alloc
	beforeFiles := m.root.FileCount
	m.stats.Files, m.stats.Dirs = beforeFiles, m.root.DirCount+1

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF8})
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("F8 returned no staging command")
	}
	m = asModel(t, m, cmd())
	if m.management != managementReview || len(m.queue) != 1 {
		t.Fatalf("management=%v queue=%#v", m.management, m.queue)
	}
	if m.queue[0].Action != "delete" || m.queue[0].Expected == nil || m.queue[0].Expected.Path != target {
		t.Fatalf("unguarded queued operation: %#v", m.queue[0])
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("staging mutated target: %v", err)
	}

	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.management != managementConfirm {
		t.Fatalf("management=%v, want confirm", m.management)
	}
	m = asModel(t, m, key("apply"))
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.management != managementConfirm || !contains(m.managementError, "APPLY exactly") {
		t.Fatal("lowercase confirmation was accepted")
	}
	m.managementInput, m.managementError = "APPLY", ""
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if cmd == nil || m.management != managementApplying {
		t.Fatal("confirmed apply did not start asynchronously")
	}
	m = asModel(t, m, cmd())
	if m.management != managementResult || len(m.queue) != 0 {
		t.Fatalf("post-apply state management=%v queue=%d error=%q", m.management, len(m.queue), m.managementError)
	}
	if m.scanning {
		t.Fatal("an exact file deletion unnecessarily started a rescan")
	}
	if m.findNode(target) != nil {
		t.Fatal("deleted file remains in the measured tree")
	}
	if got, want := m.root.Apparent, beforeApparent-targetNode.Apparent; got != want {
		t.Fatalf("root apparent bytes = %d, want %d", got, want)
	}
	if got, want := m.root.Alloc, beforeAlloc-targetNode.Alloc; got != want {
		t.Fatalf("root allocated bytes = %d, want %d", got, want)
	}
	if m.root.FileCount != beforeFiles-1 || m.stats.Files != beforeFiles-1 {
		t.Fatalf("file totals = tree:%d stats:%d, want %d", m.root.FileCount, m.stats.Files, beforeFiles-1)
	}
	for _, row := range m.topRows {
		if row.file.Rel == "big.bin" {
			t.Fatal("deleted file remains in the largest-files view")
		}
	}
	for _, row := range m.extRows {
		if row.ext.Ext == ".bin" {
			t.Fatal("deleted file remains in the extension view")
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target still exists: %v", err)
	}
	if info, err := os.Stat(auditPath); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("audit log missing or unsafe: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(path, ".dirstat-audit.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("TUI ignored configured audit path: %v", err)
	}
}

func TestSuccessfulDirectoryDeleteUpdatesSubtreeMetadataWithoutRescan(t *testing.T) {
	path, root := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: root})
	m.stats.Files, m.stats.Dirs = m.root.FileCount, m.root.DirCount+1

	target := filepath.Join(path, "sub")
	entry, err := fsinfo.Inspect(target, false)
	if err != nil {
		t.Fatal(err)
	}
	removed := m.findNode(target)
	if removed == nil {
		t.Fatal("directory is absent from the measured tree")
	}
	beforeApparent, beforeAlloc := m.root.Apparent, m.root.Alloc
	beforeFiles, beforeDirs := m.stats.Files, m.stats.Dirs
	m.queue = []fsops.Operation{{ID: "delete-sub", Action: fsops.ActionDelete, Source: target, Expected: &entry, Recursive: true}}
	m.management = managementApplying
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	updated, cmd := m.Update(appliedMsg{results: []fsops.Result{{
		OperationID: "delete-sub", Action: fsops.ActionDelete, Status: "ok", FinishedAt: time.Now(),
	}}})
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("metadata update did not request refreshed inspection")
	}
	if m.scanning {
		t.Fatal("an exact directory deletion unnecessarily started a rescan")
	}
	if m.findNode(target) != nil {
		t.Fatal("deleted directory remains in the measured tree")
	}
	if got, want := m.root.Apparent, beforeApparent-removed.Apparent; got != want {
		t.Fatalf("root apparent bytes = %d, want %d", got, want)
	}
	if got, want := m.root.Alloc, beforeAlloc-removed.Alloc; got != want {
		t.Fatalf("root allocated bytes = %d, want %d", got, want)
	}
	if got, want := m.stats.Files, beforeFiles-removed.FileCount; got != want {
		t.Fatalf("files = %d, want %d", got, want)
	}
	if got, want := m.stats.Dirs, beforeDirs-removed.DirCount-1; got != want {
		t.Fatalf("directories = %d, want %d", got, want)
	}
}

func TestF8MarksDirectoryDeleteRecursive(t *testing.T) {
	path, node := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: node})
	for i := range m.rows {
		if m.rows[i].node.Name == "sub" {
			m.cursor = i
			break
		}
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF8})
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("directory delete returned no staging command")
	}
	m = asModel(t, m, cmd())
	if len(m.queue) != 1 || !m.queue[0].Recursive || m.queue[0].Expected == nil || m.queue[0].Expected.Kind != "directory" {
		t.Fatalf("directory delete is not explicitly recursive: %#v", m.queue)
	}
}

func TestPartialApplyDropsCompletedOperationsAndUpdatesExactDeletes(t *testing.T) {
	path, node := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: node})
	target := filepath.Join(path, "big.bin")
	entry, err := fsinfo.Inspect(target, false)
	if err != nil {
		t.Fatal(err)
	}
	before := m.root.Apparent
	removed := m.findNode(target)
	if removed == nil {
		t.Fatal("delete target is absent from the measured tree")
	}
	m.queue = []fsops.Operation{
		{ID: "done", Action: fsops.ActionDelete, Source: target, Expected: &entry},
		{ID: "failed", Action: fsops.ActionDelete, Source: filepath.Join(path, "failed")},
		{ID: "pending", Action: fsops.ActionDelete, Source: filepath.Join(path, "pending")},
	}
	m.management = managementApplying
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(appliedMsg{
		results: []fsops.Result{
			{OperationID: "done", Action: fsops.ActionDelete, Status: "ok", FinishedAt: time.Now()},
			{OperationID: "failed", Action: fsops.ActionDelete, Status: "error", Error: "stale"},
		},
		err: errors.New("stale"),
	})
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("partial mutation did not refresh inspection")
	}
	if m.scanning {
		t.Fatal("an exact successful delete in a partial apply unnecessarily started a rescan")
	}
	if m.findNode(target) != nil || m.root.Apparent != before-removed.Apparent {
		t.Fatal("successful portion of partial apply was not reflected in the tree")
	}
	if len(m.queue) != 2 || m.queue[0].ID != "failed" || m.queue[1].ID != "pending" {
		t.Fatalf("remaining queue = %#v", m.queue)
	}
	if m.management != managementResult || !contains(m.managementError, "stale") {
		t.Fatalf("management=%v error=%q", m.management, m.managementError)
	}
}

func TestSuccessfulNonDeleteApplyStartsReconciliationScan(t *testing.T) {
	path, node := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{DisableAudit: true}))
	m = asModel(t, m, scanDoneMsg{node: node})
	m.queue = []fsops.Operation{{ID: "copy", Action: fsops.ActionCopy, Source: filepath.Join(path, "a.go")}}
	m.management = managementApplying

	updated, cmd := m.Update(appliedMsg{results: []fsops.Result{{OperationID: "copy", Action: fsops.ActionCopy, Status: "ok"}}})
	m = updated.(*model)
	if cmd == nil || !m.scanning {
		t.Fatal("a non-delete mutation did not start a reconciliation scan")
	}
	m.cancelScan()
}

func TestF5QueuesCopyDestinationWithoutMutation(t *testing.T) {
	path, node := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{}))
	m = asModel(t, m, scanDoneMsg{node: node})
	for i := range m.rows {
		if m.rows[i].node.Name == "a.go" {
			m.cursor = i
			break
		}
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyF5})
	if m.management != managementDestination {
		t.Fatalf("management=%v", m.management)
	}
	m = asModel(t, m, key("copy.go"))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("copy destination returned no staging command")
	}
	m = asModel(t, m, cmd())
	if len(m.queue) != 1 || m.queue[0].Destination != filepath.Join(path, "copy.go") {
		t.Fatalf("queue=%#v", m.queue)
	}
	if _, err := os.Stat(filepath.Join(path, "copy.go")); !os.IsNotExist(err) {
		t.Fatalf("queueing copied file: %v", err)
	}
}

func TestF6AndF7QueueMoveAndMkdir(t *testing.T) {
	path, node := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{}))
	m = asModel(t, m, scanDoneMsg{node: node})
	for i := range m.rows {
		if m.rows[i].node.Name == "a.go" {
			m.cursor = i
			break
		}
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyF6})
	m = asModel(t, m, key("moved.go"))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("move returned no staging command")
	}
	m = asModel(t, m, cmd())
	if len(m.queue) != 1 || m.queue[0].Action != "move" || m.queue[0].Destination != filepath.Join(path, "moved.go") {
		t.Fatalf("move queue=%#v", m.queue)
	}
	m.closeManagement()
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyF7})
	m = asModel(t, m, key("new-directory"))
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if cmd == nil {
		t.Fatal("mkdir returned no staging command")
	}
	m = asModel(t, m, cmd())
	if len(m.queue) != 2 || m.queue[1].Action != "mkdir" || m.queue[1].Source != filepath.Join(path, "new-directory") {
		t.Fatalf("mkdir queue=%#v", m.queue)
	}
	if _, err := os.Stat(filepath.Join(path, "new-directory")); !os.IsNotExist(err) {
		t.Fatalf("queueing created directory: %v", err)
	}
}

func TestReadOnlyBlocksManagementAndEditor(t *testing.T) {
	path, node := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{ReadOnly: true, Editor: []string{"editor"}}))
	m = asModel(t, m, scanDoneMsg{node: node})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF8})
	m = updated.(*model)
	if cmd != nil || len(m.queue) != 0 || !contains(m.managementError, "read-only") {
		t.Fatalf("delete not blocked: queue=%v error=%q", m.queue, m.managementError)
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyF4})
	m = updated.(*model)
	if cmd != nil || !contains(m.managementError, "read-only") {
		t.Fatal("editor not blocked in read-only mode")
	}
	if !contains(m.headerView(), "read-only") {
		t.Fatal("read-only mode is not visible")
	}
}

func TestExternalEditorUsesExactArgvWithoutShell(t *testing.T) {
	cmd, err := pathCommand([]string{"editor", "--flag", "value;touch /tmp/bad"}, "/tmp/a file")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"editor", "--flag", "value;touch /tmp/bad", "/tmp/a file"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("argv=%q, want %q", cmd.Args, want)
	}
}

func TestExternalEditorRejectsSudo(t *testing.T) {
	if _, err := pathCommand([]string{"/usr/bin/sudo", "editor"}, "/tmp/file"); err == nil || !contains(err.Error(), "sudo") {
		t.Fatalf("sudo command was accepted: %v", err)
	}
}

func TestPagerAndShellCommandsPreserveExactArgv(t *testing.T) {
	pager, err := pathCommand([]string{"pager", "--literal=a;b"}, "/tmp/a file")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pager", "--literal=a;b", "/tmp/a file"}; !reflect.DeepEqual(pager.Args, want) {
		t.Fatalf("pager argv=%q, want %q", pager.Args, want)
	}
	shell, err := workingDirectoryCommand([]string{"shell", "--noprofile"}, "/tmp/a directory")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"shell", "--noprofile"}; !reflect.DeepEqual(shell.Args, want) {
		t.Fatalf("shell argv=%q, want %q", shell.Args, want)
	}
	if shell.Dir != "/tmp/a directory" {
		t.Fatalf("shell cwd=%q", shell.Dir)
	}
	for _, argv := range [][]string{{"sudo", "pager"}, {"/usr/bin/sudo", "shell"}} {
		if err := validateExecutable(argv); err == nil {
			t.Fatalf("accepted sudo argv %q", argv)
		}
	}
}

func TestF3TogglesPreviewContextPanel(t *testing.T) {
	m := newModel(New(t.TempDir(), scope.New(), tree.SizeApparent, 0, Options{}))
	if !m.contextPanel {
		t.Fatal("context panel should start enabled")
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyF3})
	if m.contextPanel {
		t.Fatal("F3 did not hide context panel")
	}
	m = asModel(t, m, tea.KeyMsg{Type: tea.KeyF3})
	if !m.contextPanel {
		t.Fatal("F3 did not show context panel")
	}
}

func TestHelpBlocksFilesystemActionKeys(t *testing.T) {
	path, node := mkTree(t)
	m := newModel(New(path, scope.New(), tree.SizeApparent, 0, Options{}))
	m = asModel(t, m, scanDoneMsg{node: node})
	m = asModel(t, m, key("?"))
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF8})
	m = updated.(*model)
	if cmd != nil || m.management != managementNone || len(m.queue) != 0 {
		t.Fatal("F8 escaped the help modal")
	}
}

// helpers ----------------------------------------------------------------

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
