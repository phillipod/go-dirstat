package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

const fingerprintTestRoot = "/some/path"

func TestSnapshotRoundTripPreservesTotals(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), 100)
	write(t, filepath.Join(root, "b.bin"), 2000)
	write(t, filepath.Join(root, "sub", "c.md"), 50)

	node, stats, err := scan.Scan(context.Background(), root, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint(root, scope.New())

	snap := FromTree(node, fp, stats.RootFS, stats.Files, stats.Dirs, stats.Errors, stats.Complete, time.Now())
	snap.Root = root
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data, fp)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := got.ToTree()
	if rebuilt.Apparent != node.Apparent || rebuilt.Alloc != node.Alloc {
		t.Errorf("rebuilt sizes = %d/%d, want %d/%d", rebuilt.Apparent, rebuilt.Alloc, node.Apparent, node.Alloc)
	}
	if rebuilt.FileCount != node.FileCount || rebuilt.DirCount != node.DirCount {
		t.Errorf("rebuilt counts = %d/%d, want %d/%d", rebuilt.FileCount, rebuilt.DirCount, node.FileCount, node.DirCount)
	}
	if got.Files != stats.Files {
		t.Errorf("snap.Files = %d, want %d", got.Files, stats.Files)
	}
}

func TestFingerprintChangesWithOptions(t *testing.T) {
	root := fingerprintTestRoot
	p1 := scope.New()
	p2 := scope.New(scope.WithCrossDevice(true))
	if Fingerprint(root, p1) == Fingerprint(root, p2) {
		t.Error("fingerprint should differ when cross-device changes")
	}
	if Fingerprint(root, p1) != Fingerprint(root, scope.New()) {
		t.Error("identical options should produce identical fingerprint")
	}
}

func TestFingerprintUsesFullDigestAndUnambiguousFields(t *testing.T) {
	root := fingerprintTestRoot
	oneValue := scope.New(scope.WithExcludeGlobs([]string{"a\ng b"}))
	twoValues := scope.New(scope.WithExcludeGlobs([]string{"a", "b"}))

	got := Fingerprint(root, oneValue)
	if len(got) != sha256.Size*2 {
		t.Fatalf("fingerprint length = %d, want %d", len(got), sha256.Size*2)
	}
	if got == Fingerprint(root, twoValues) {
		t.Fatal("length-prefixed fingerprints collided for delimiter-shaped inputs")
	}
	if legacyFingerprintForTest(root, oneValue) != legacyFingerprintForTest(root, twoValues) {
		t.Fatal("test corpus no longer reproduces the legacy delimiter collision")
	}
}

func TestFingerprintCanonicalizesUnorderedPolicyValues(t *testing.T) {
	p1 := scope.New(
		scope.WithExcludeGlobs([]string{"*.tmp", "*.bak"}),
		scope.WithFilesystems([]string{"ext4", "xfs"}, []string{"proc", "tmpfs"}),
	)
	p2 := scope.New(
		scope.WithExcludeGlobs([]string{"*.bak", "*.tmp"}),
		scope.WithFilesystems([]string{"xfs", "ext4"}, []string{"tmpfs", "proc"}),
	)
	if got, want := Fingerprint("/root", p1), Fingerprint("/root", p2); got != want {
		t.Fatalf("equivalent unordered policies differ:\n got %q\nwant %q", got, want)
	}
}

func TestFingerprintIgnoresDisjointOperationalPathsButKeepsAncestors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scan", "child")
	base := scope.New(scope.WithExcludeVirtual(false))
	disjoint := scope.New(scope.WithExcludeVirtual(false), scope.WithExcludePaths([]string{filepath.Join(filepath.Dir(filepath.Dir(root)), "state")}))
	if got, want := Fingerprint(root, disjoint), Fingerprint(root, base); got != want {
		t.Fatalf("disjoint state path changed fingerprint: got %q want %q", got, want)
	}
	ancestor := scope.New(scope.WithExcludeVirtual(false), scope.WithExcludePaths([]string{filepath.Dir(root)}))
	if Fingerprint(root, ancestor) == Fingerprint(root, base) {
		t.Fatal("ancestor exclusion was dropped from the fingerprint")
	}
}

func TestFingerprintKeepsMissingPathBelowSymlinkAlias(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "real")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	plain := scope.New(scope.WithExcludeVirtual(false))
	aliasPolicy := scope.New(scope.WithExcludeVirtual(false), scope.WithExcludePaths([]string{filepath.Join(alias, "not-created-yet")}))
	if Fingerprint(root, aliasPolicy) == Fingerprint(root, plain) {
		t.Fatal("missing exclusion suffix below a symlink alias was dropped from the fingerprint")
	}
}

func TestFingerprintStableVector(t *testing.T) {
	policy := scope.New(
		scope.WithCrossDevice(true),
		scope.WithFollowSymlinks(true),
		scope.WithExcludeVirtual(false),
		scope.WithHidden(false),
		scope.WithSizeThreshold(17, 4096),
		scope.WithExcludeGlobs([]string{"*.tmp", "line\nbreak"}),
		scope.WithFilesystems([]string{"ext4", "xfs"}, []string{"proc", "tmpfs"}),
	)
	const want = "47cd3b673ab7591500fc54383a6fddf25b80e9e1ad5a341e535b8453488c2836"
	if got := Fingerprint("fixture-root", policy); got != want {
		t.Fatalf("Fingerprint() = %q, want stable vector %q", got, want)
	}
}

func TestUnmarshalRejectsLegacyFingerprint(t *testing.T) {
	root := fingerprintTestRoot
	policy := scope.New(scope.WithExcludeGlobs([]string{"*.tmp"}))
	legacy := legacyFingerprintForTest(root, policy)
	current := Fingerprint(root, policy)
	if legacy == current {
		t.Fatal("legacy and current fingerprints unexpectedly match")
	}

	snap := FromTree(&tree.Node{Name: "x", IsDir: true}, legacy, "", 0, 1, 0, true, time.Now())
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unmarshal(data, current); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
	}
}

func TestUnmarshalRejectsWrongFingerprint(t *testing.T) {
	snap := FromTree(&tree.Node{Name: "x", IsDir: true}, "aaaa", "", 0, 1, 0, true, time.Now())
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unmarshal(data, "bbbb"); err == nil {
		t.Error("expected ErrIncompatible for mismatched fingerprint")
	}
}

func TestSnapshotRejectsInvalidParentIndexes(t *testing.T) {
	snap := &Snapshot{
		Fingerprint: "scope-fingerprint",
		Nodes: []FlatNode{
			{Name: "root", IsDir: true, Parent: -1},
			{Name: "child", Parent: 99},
		},
	}
	if got := snap.ToTree(); got != nil {
		t.Fatalf("ToTree() = %#v, want nil for an invalid parent index", got)
	}

	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
	}
}

func TestSnapshotRejectsInvalidNodeStructure(t *testing.T) {
	tests := map[string][]FlatNode{
		"nonzero root depth": {
			{Name: "root", IsDir: true, Depth: 1, Parent: -1},
		},
		"file parent": {
			{Name: "root", Depth: 0, Parent: -1},
			{Name: "child", Depth: 1, Parent: 0},
		},
		"wrong child depth": {
			{Name: "root", IsDir: true, Depth: 0, Parent: -1},
			{Name: "child", Depth: 2, Parent: 0},
		},
		"negative size": {
			{Name: "root", IsDir: true, Depth: 0, Apparent: -1, Parent: -1},
		},
	}
	for name, nodes := range tests {
		t.Run(name, func(t *testing.T) {
			snap := &Snapshot{Fingerprint: "scope-fingerprint", Nodes: nodes}
			data, err := snap.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
			}
		})
	}
}

func TestSnapshotRejectsNegativeSummaryCounters(t *testing.T) {
	snap := &Snapshot{
		Fingerprint: "scope-fingerprint",
		Files:       -1,
		Nodes:       []FlatNode{{Name: "root", IsDir: true, Parent: -1}},
	}
	data, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unmarshal(data, snap.Fingerprint); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("Unmarshal() error = %v, want ErrIncompatible", err)
	}
}

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, size)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// legacyFingerprintForTest preserves the pre-v2 delimiter-based encoding so
// the intentional cache invalidation remains an executable compatibility test.
func legacyFingerprintForTest(root string, p scope.Policy) string {
	h := sha256.New()
	_, _ = fmt.Fprintln(h, root)
	_, _ = fmt.Fprintln(h, p.CrossDevice, p.FollowSymlinks, p.ExcludeVirtual, p.IncludeHidden)
	_, _ = fmt.Fprintln(h, p.MinSize, p.MaxSize)
	writeSorted := func(tag string, values []string) {
		values = append([]string(nil), values...)
		sort.Strings(values)
		for _, value := range values {
			_, _ = fmt.Fprintln(h, tag, value)
		}
	}
	writeKeys := func(tag string, values map[string]bool) {
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		writeSorted(tag, keys)
	}
	writeSorted("g", p.ExcludeGlobs)
	writeSorted("ep", p.ExcludePaths)
	writeSorted("ip", p.IncludePaths)
	writeSorted("vp", p.VirtualPaths)
	writeKeys("ifs", p.IncludeFS)
	writeKeys("efs", p.ExcludeFS)
	return hex.EncodeToString(h.Sum(nil)[:8])
}
