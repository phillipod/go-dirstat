package query

import (
	"testing"

	"github.com/phillipod/go-dirstat/internal/tree"
)

func FuzzQueryFilter(f *testing.F) {
	f.Add("*.log", `old\.log$`, "log", "file")
	f.Add("[", "(", "", "unknown")
	f.Fuzz(func(t *testing.T, glob, expression, extension, kind string) {
		if len(glob)+len(expression)+len(extension)+len(kind) > 64<<10 {
			return
		}
		root := &tree.Node{Name: "root", IsDir: true}
		root.Adopt(&tree.Node{Name: "candidate.log", Apparent: 1})
		_, _ = Build(root, t.TempDir(), Options{Filter: Filter{
			PathGlob: glob, PathRegexp: expression, Extensions: []string{extension}, Kinds: []Kind{Kind(kind)},
		}})
	})
}
