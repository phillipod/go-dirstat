package query

import (
	"strconv"
	"testing"

	"github.com/phillipod/go-dirstat/internal/tree"
)

func BenchmarkMillionEntryQuery(b *testing.B) {
	const entries = 1_000_000
	root := &tree.Node{Name: "root", IsDir: true, FileCount: entries}
	root.Children = make([]*tree.Node, 0, entries)
	for i := 0; i < entries; i++ {
		root.AddChild(&tree.Node{Name: "file-" + strconv.Itoa(i), Apparent: int64(i), Alloc: int64(i)})
	}
	filter := Filter{Kinds: []Kind{KindFile}}

	b.Run("bounded-top-100", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			records, err := Build(root, "/benchmark", Options{
				Filter: filter, Limit: 100,
				Sort: []SortKey{{Field: SortApparent, Desc: true}},
			})
			if err != nil || len(records) != 100 {
				b.Fatalf("Build() records=%d error=%v", len(records), err)
			}
		}
	})

	b.Run("unsorted-stream", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			count := 0
			err := Stream(root, "/benchmark", Options{Filter: filter}, func(Record) error {
				count++
				return nil
			})
			if err != nil || count != entries {
				b.Fatalf("Stream() count=%d error=%v", count, err)
			}
		}
	})
}
