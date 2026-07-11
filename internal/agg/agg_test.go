package agg

import (
	"errors"
	"testing"

	"github.com/phillipod/go-dirstat/internal/tree"
)

func nFile(name string, app, alloc int64, depth int) *tree.Node {
	return &tree.Node{Name: name, Apparent: app, Alloc: alloc, Depth: depth}
}

func TestExtensionsGroupAndSort(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("a.go", 100, 200, 1))
	root.Adopt(nFile("b.GO", 50, 80, 1)) // case-insensitive merge with a.go
	root.Adopt(nFile("c.md", 10, 40, 1))
	root.Adopt(nFile("Makefile", 5, 16, 1)) // no ext -> (none)
	root.Adopt(&tree.Node{Name: "unknown.err", Err: errors.New("stat failed")})

	exts := Extensions(root, tree.SizeApparent)
	if exts[0].Ext != ".go" {
		t.Errorf("top ext = %q, want .go", exts[0].Ext)
	}
	var goStat *ExtStat
	for i := range exts {
		if exts[i].Ext == ".go" {
			goStat = &exts[i]
		}
	}
	if goStat == nil || goStat.Count != 2 || goStat.Apparent != 150 {
		t.Errorf(".go bucket = %+v, want count=2 apparent=150", goStat)
	}
	var none *ExtStat
	for i := range exts {
		if exts[i].Ext == "(none)" {
			none = &exts[i]
		}
		if exts[i].Ext == ".err" {
			t.Fatal("stat error was counted as a measured extension")
		}
	}
	if none == nil || none.Count != 1 {
		t.Errorf("(none) bucket = %+v", none)
	}
}

func TestExtensionsSortBySelectedSizeMode(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("logical.go", 1000, 100, 1))
	root.Adopt(nFile("allocated.md", 100, 2000, 1))

	apparent := Extensions(root, tree.SizeApparent)
	if got := apparent[0].Ext; got != ".go" {
		t.Fatalf("apparent-size top extension = %q, want .go", got)
	}
	onDisk := Extensions(root, tree.SizeOnDisk)
	if got := onDisk[0].Ext; got != ".md" {
		t.Fatalf("on-disk top extension = %q, want .md", got)
	}
}

func TestClassifyExtDotfiles(t *testing.T) {
	if got := classifyExt(".gitignore"); got != "(none)" {
		t.Errorf("classifyExt(.gitignore) = %q, want (none)", got)
	}
	if got := classifyExt("archive.tar.gz"); got != ".gz" {
		t.Errorf("classifyExt(archive.tar.gz) = %q, want .gz", got)
	}
}

func TestTopFiles(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("small", 10, 40, 1))
	root.Adopt(nFile("big", 9000, 9000, 1))
	root.Adopt(nFile("mid", 500, 500, 1))
	top := TopFiles(root, tree.SizeApparent, 2)
	if len(top) != 2 || top[0].Name != "big" || top[1].Name != "mid" {
		t.Errorf("TopFiles = %+v", top)
	}
}

func TestTopFilesUsesPathAsDeterministicSizeTieBreak(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("b", 10, 10, 1))
	root.Adopt(nFile("a", 10, 10, 1))
	top := TopFiles(root, tree.SizeApparent, 2)
	if len(top) != 2 || top[0].Rel != "a" || top[1].Rel != "b" {
		t.Fatalf("TopFiles tie order = %+v, want a then b", top)
	}
}

func TestTopFilesSkipsErrorsAndDuplicateHardlinks(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(&tree.Node{Name: "measured", Apparent: 10})
	root.Adopt(&tree.Node{Name: "error", Err: errors.New("stat failed")})
	root.Adopt(&tree.Node{Name: "duplicate", Hardlink: true})

	top := TopFiles(root, tree.SizeApparent, 10)
	if len(top) != 1 || top[0].Name != "measured" {
		t.Fatalf("TopFiles = %+v, want only measured file", top)
	}
}

func TestTopFilesNamesFileRoot(t *testing.T) {
	top := TopFiles(&tree.Node{Name: "single", Apparent: 10}, tree.SizeApparent, 1)
	if len(top) != 1 || top[0].Rel != "." {
		t.Fatalf("TopFiles(file root) = %+v, want relative path .", top)
	}
}

func TestReportFor(t *testing.T) {
	root := &tree.Node{IsDir: true, Apparent: 100, Alloc: 100, FileCount: 1}
	root.Adopt(nFile("a", 100, 100, 1))
	r := ReportFor(root, tree.SizeApparent, 5)
	if r.TotalApparent != 100 || r.FileCount != 1 || len(r.TopFiles) != 1 {
		t.Errorf("ReportFor = %+v", r)
	}
}
