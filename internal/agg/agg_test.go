package agg

import (
	"errors"
	"testing"

	"github.com/phillipod/go-dirstat/internal/fileclass"
	"github.com/phillipod/go-dirstat/internal/tree"
)

const goExtension = ".go"

func nFile(name string, app, alloc int64) *tree.Node {
	return &tree.Node{Name: name, Apparent: app, Alloc: alloc, Depth: 1}
}

func TestExtensionsGroupAndSort(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("a.go", 100, 200))
	root.Adopt(nFile("b.GO", 50, 80)) // case-insensitive merge with a.go
	root.Adopt(nFile("c.md", 10, 40))
	root.Adopt(nFile("Makefile", 5, 16)) // no ext -> (none)
	root.Adopt(nFile(".env", 3, 8))      // leading-only dot -> (none)
	root.Adopt(&tree.Node{Name: "unknown.err", Err: errors.New("stat failed")})

	exts := Extensions(root, tree.SizeApparent)
	if exts[0].Ext != goExtension {
		t.Errorf("top ext = %q, want %s", exts[0].Ext, goExtension)
	}
	var goStat *ExtStat
	for i := range exts {
		if exts[i].Ext == goExtension {
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
	if none == nil || none.Count != 2 {
		t.Errorf("(none) bucket = %+v", none)
	}
}

func TestExtensionsSortBySelectedSizeMode(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("logical.go", 1000, 100))
	root.Adopt(nFile("allocated.md", 100, 2000))

	apparent := Extensions(root, tree.SizeApparent)
	if got := apparent[0].Ext; got != goExtension {
		t.Fatalf("apparent-size top extension = %q, want .go", got)
	}
	onDisk := Extensions(root, tree.SizeOnDisk)
	if got := onDisk[0].Ext; got != ".md" {
		t.Fatalf("on-disk top extension = %q, want .md", got)
	}
}

func TestClassifyExtDotfiles(t *testing.T) {
	if got := fileclass.Extension(".gitignore"); got != "" {
		t.Errorf("Extension(.gitignore) = %q, want empty", got)
	}
	if got := fileclass.Extension("archive.tar.gz"); got != ".gz" {
		t.Errorf("Extension(archive.tar.gz) = %q, want .gz", got)
	}
}

func TestTopFiles(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("small", 10, 40))
	root.Adopt(nFile("big", 9000, 9000))
	root.Adopt(nFile("mid", 500, 500))
	top := TopFiles(root, tree.SizeApparent, 2)
	if len(top) != 2 || top[0].Name != "big" || top[1].Name != "mid" {
		t.Errorf("TopFiles = %+v", top)
	}
}

func TestTopFilesUsesPathAsDeterministicSizeTieBreak(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(nFile("b", 10, 10))
	root.Adopt(nFile("a", 10, 10))
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
	root.Adopt(nFile("a", 100, 100))
	r := ReportFor(root, tree.SizeApparent, 5)
	if r.TotalApparent != 100 || r.FileCount != 1 || len(r.TopFiles) != 1 {
		t.Errorf("ReportFor = %+v", r)
	}
}
