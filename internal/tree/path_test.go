package tree

import (
	"fmt"
	"runtime"
	"testing"
)

// TestPath checks the lazy relative-path reconstruction that replaced the stored
// Rel field. Path() must mirror the old Rel semantics: "" for the root, and the
// joined basenames of a node's ancestors otherwise.
func TestPath(t *testing.T) {
	root := &Node{Name: "root", IsDir: true}
	a := &Node{Name: "a", IsDir: true}
	b := &Node{Name: "b.txt"}
	root.AddChild(a)
	a.AddChild(b)

	if got := root.Path(); got != "" {
		t.Errorf("root.Path() = %q, want %q", got, "")
	}
	if got := a.Path(); got != "a" {
		t.Errorf("a.Path() = %q, want %q", got, "a")
	}
	if got := b.Path(); got != "a/b.txt" {
		t.Errorf("b.Path() = %q, want %q", got, "a/b.txt")
	}
	// A node with no parent (detached, or not yet attached by the scanner) has
	// no defined relative path.
	if got := (&Node{Name: "lonely"}).Path(); got != "" {
		t.Errorf("detached node Path() = %q, want %q", got, "")
	}
}

// TestPathStableAcrossClone ensures Path() is content-stable across a Clone, so
// the TUI's expansion map (keyed by Path) keeps working as snapshots replace the
// live tree with fresh copies.
func TestPathStableAcrossClone(t *testing.T) {
	root := &Node{Name: "root", IsDir: true}
	sub := &Node{Name: "sub", IsDir: true}
	root.AddChild(sub)
	deep := &Node{Name: "deep", IsDir: true}
	sub.AddChild(deep)
	deep.AddChild(&Node{Name: "f.bin"})

	clone := root.Clone()
	got := clone.Children[0].Children[0].Children[0].Path()
	if want := "sub/deep/f.bin"; got != want {
		t.Errorf("cloned Path() = %q, want %q", got, want)
	}
}

// buildWideTree builds a directory tree of `dirs` directories each holding
// `files` file leaves under a single root, returning the root and the total
// node count. Used to exercise Path()/Clone() at scale without touching disk.
func buildWideTree(dirs, files int) (*Node, int) {
	root := &Node{Name: "root", IsDir: true}
	for d := 0; d < dirs; d++ {
		dir := &Node{Name: fmt.Sprintf("d%d", d), IsDir: true}
		root.AddChild(dir)
		for f := 0; f < files; f++ {
			dir.AddChild(&Node{Name: fmt.Sprintf("f%d", f), Apparent: int64(f)})
		}
	}
	return root, 1 + dirs + dirs*files
}

// BenchmarkClone measures the allocation cost of deep-cloning a 100k-node tree,
// which is what the streaming TUI does per snapshot. Keeping this cheap (and, in
// scan.go, gated off entirely above snapshotNodeLimit) is what prevents the OOM
// the multi-GB clone storm caused.
func BenchmarkClone(b *testing.B) {
	root, n := buildWideTree(2000, 50) // ~100k nodes
	b.Logf("tree size: %d nodes", n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = root.Clone()
	}
}

// BenchmarkNodeFootprint reports the heap cost of holding a tree resident: it
// builds a tree of a known node count and prints bytes-per-node from the heap
// delta. Run with `go test -run x -bench NodeFootprint -benchmem` to inspect.
func BenchmarkNodeFootprint(b *testing.B) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	root, n := buildWideTree(2000, 50) // ~100k nodes
	runtime.KeepAlive(root)
	runtime.ReadMemStats(&after)
	b.Logf("tree size: %d nodes, heap delta: %.1f MB (%.0f bytes/node)",
		n, float64(after.HeapAlloc-before.HeapAlloc)/(1<<20),
		float64(after.HeapAlloc-before.HeapAlloc)/float64(n))
}
