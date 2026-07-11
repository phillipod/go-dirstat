package render

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/phillipod/go-dirstat/internal/agg"
	"github.com/phillipod/go-dirstat/internal/format"
	"github.com/phillipod/go-dirstat/internal/scan"
	"github.com/phillipod/go-dirstat/internal/scope"
	"github.com/phillipod/go-dirstat/internal/tree"
)

func TestTreePlainNoColor(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root+"/a.go", 100)
	mustWrite(t, root+"/big.bin", 5000)
	if err := os.MkdirAll(root+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, root+"/sub/c.md", 50)

	n, _, err := scan.Scan(context.Background(), root, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := Tree(&b, n, TextOptions{Sort: tree.SortSizeDesc, Size: tree.SizeApparent, Bar: true, BarWidth: 10, Counts: true, Files: true, Color: false}); err != nil {
		t.Fatal(err)
	}

	out := b.String()
	for _, want := range []string{"100.0%", "big.bin", "sub/", "a.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("plain output should contain no ANSI codes")
	}
}

func TestTreeEscapesTerminalControlsInNames(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Apparent: 1}
	root.Adopt(&tree.Node{Name: "evil\x1b]52;payload\a\nname", Apparent: 1})
	var b strings.Builder
	if err := Tree(&b, root, TextOptions{Size: tree.SizeApparent, Files: true, Color: false}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "\x1b]52") || strings.Contains(b.String(), "\a") {
		t.Fatalf("tree output contains raw terminal controls: %q", b.String())
	}
	for _, want := range []string{`\x1B]52`, `\x07`, `\nname`} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("tree output missing visible escape %q: %q", want, b.String())
		}
	}
}

func TestTreeDirsOnlyByDefault(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root+"/a.go", 100)
	mustWrite(t, root+"/big.bin", 5000)
	if err := os.MkdirAll(root+"/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, root+"/sub/c.md", 50)

	n, _, err := scan.Scan(context.Background(), root, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	// Default (Files false): du-style, directories only — files folded into aggregates.
	if err := Tree(&b, n, TextOptions{Size: tree.SizeApparent, Color: false, Counts: true}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "big.bin") || strings.Contains(out, "a.go") {
		t.Errorf("default listing should omit file rows\n%s", out)
	}
	if !strings.Contains(out, "sub/") {
		t.Errorf("default listing should show directories\n%s", out)
	}
	// Their bytes are still counted in the root aggregate.
	rootLine := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(rootLine, format.Bytes(n.Apparent)) {
		t.Errorf("root line %q does not contain aggregate size %q", rootLine, format.Bytes(n.Apparent))
	}
}

func TestTreeDepthAndLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		mustWrite(t, root+"/"+string(rune('a'+i))+".txt", (i+1)*100)
	}
	n, _, err := scan.Scan(context.Background(), root, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := Tree(&b, n, TextOptions{Limit: 2, Size: tree.SizeApparent, Files: true, Color: false, Counts: false}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "more entries") {
		t.Errorf("limit should fold remaining entries\n%s", out)
	}
}

func TestBytesAppliesToSummaryAndLimitTotals(t *testing.T) {
	root := &tree.Node{IsDir: true, Apparent: 6000}
	root.Adopt(&tree.Node{Name: "large", Apparent: 3000})
	root.Adopt(&tree.Node{Name: "medium", Apparent: 2000})
	root.Adopt(&tree.Node{Name: "small", Apparent: 1000})
	opts := TextOptions{
		Limit: 1,
		Sort:  tree.SortSizeDesc,
		Size:  tree.SizeApparent,
		Files: true,
		Bytes: true,
	}

	var b strings.Builder
	if err := Tree(&b, root, opts); err != nil {
		t.Fatal(err)
	}
	if err := Summary(&b, root, SummaryData{}, opts); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "3000 total") {
		t.Fatalf("limit total is not raw bytes:\n%s", out)
	}
	if !strings.Contains(out, "Total: 6000") {
		t.Fatalf("summary total is not raw bytes:\n%s", out)
	}
	if strings.Contains(out, "2.93K") || strings.Contains(out, "5.86K") {
		t.Fatalf("byte-mode output contains human size:\n%s", out)
	}
}

func TestExtensionsView(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root+"/a.go", 100)
	mustWrite(t, root+"/b.go", 200)
	mustWrite(t, root+"/c.md", 50)
	n, _, err := scan.Scan(context.Background(), root, scan.WithPolicy(scope.New()))
	if err != nil {
		t.Fatal(err)
	}
	exts := agg.Extensions(n, tree.SizeApparent)
	var b strings.Builder
	if err := Extensions(&b, exts, TextOptions{Size: tree.SizeApparent, Bar: true, BarWidth: 10, Color: false}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, ".go") || !strings.Contains(out, ".md") {
		t.Errorf("extensions view missing types\n%s", out)
	}
}

func TestExtensionsViewUsesSelectedSizeMode(t *testing.T) {
	root := &tree.Node{IsDir: true}
	root.Adopt(&tree.Node{Name: "logical.go", Apparent: 1000, Alloc: 100})
	root.Adopt(&tree.Node{Name: "allocated.md", Apparent: 100, Alloc: 2000})
	exts := agg.Extensions(root, tree.SizeOnDisk)

	var b strings.Builder
	if err := Extensions(&b, exts, TextOptions{Size: tree.SizeOnDisk, Bytes: true, Color: false}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(b.String(), "\n")
	if got := firstFieldFor(lines, ".md"); got != "2000" {
		t.Errorf("on-disk .md size = %q, want 2000\n%s", got, b.String())
	}
	if got := firstFieldFor(lines, ".go"); got != "100" {
		t.Errorf("on-disk .go size = %q, want 100\n%s", got, b.String())
	}
}

func TestExtensionsVisibleRowsUseFileOnlyDenominator(t *testing.T) {
	root := &tree.Node{Name: "root", IsDir: true, Alloc: 8192}
	root.Adopt(&tree.Node{Name: "only.go", Alloc: 4096})

	var b strings.Builder
	err := Extensions(&b, agg.Extensions(root, tree.SizeOnDisk), TextOptions{
		Size: tree.SizeOnDisk, Bytes: true, Color: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.Contains(got, "100.0%") {
		t.Fatalf("single visible file extension is not 100%% of visible total:\n%s", got)
	}
}

func TestExtensionsNoCounts(t *testing.T) {
	exts := []agg.ExtStat{{Ext: ".go", Count: 123, Apparent: 1024}}
	var b strings.Builder
	if err := Extensions(&b, exts, TextOptions{Size: tree.SizeApparent, Color: false, Counts: false}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "123") {
		t.Fatalf("extension count present with Counts=false:\n%s", b.String())
	}
}

func TestExtensionsShowsZeroByteFiles(t *testing.T) {
	exts := []agg.ExtStat{{Ext: ".empty", Count: 2}}
	var b strings.Builder
	if err := Extensions(&b, exts, TextOptions{Size: tree.SizeApparent, Color: false, Counts: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"By extension", ".empty", "0.0%", "2"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("zero-byte extension output missing %q:\n%s", want, b.String())
		}
	}
}

func TestRichRenderersReturnWriterErrors(t *testing.T) {
	wantErr := errors.New("output closed")
	w := errorWriter{err: wantErr}
	root := &tree.Node{IsDir: true, Apparent: 1}

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "tree", run: func() error { return Tree(w, root, TextOptions{Size: tree.SizeApparent}) }},
		{name: "summary", run: func() error { return Summary(w, root, SummaryData{}, TextOptions{Size: tree.SizeApparent}) }},
		{name: "extensions", run: func() error {
			return Extensions(w, []agg.ExtStat{{Ext: ".go", Count: 1, Apparent: 1}}, TextOptions{Size: tree.SizeApparent})
		}},
		{name: "top files", run: func() error {
			return TopFiles(w, []agg.FileRef{{Rel: "a", Apparent: 1}}, tree.SizeApparent, TextOptions{})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); !errors.Is(err, wantErr) {
				t.Fatalf("renderer error = %v, want wrapped %v", err, wantErr)
			}
		})
	}
}

func firstFieldFor(lines []string, suffix string) string {
	for _, line := range lines {
		if strings.Contains(line, suffix) {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func mustWrite(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(strings.TrimSuffix(path, "/"+base(path)), 0o755); err != nil {
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

func base(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return p
	}
	return p[i+1:]
}
