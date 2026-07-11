package tree

import "testing"

func TestShallowCloneRespectsDepth(t *testing.T) {
	// root -> a/ -> b/ -> leaf
	root := &Node{Name: "root", IsDir: true}
	a := &Node{Name: "a", IsDir: true}
	b := &Node{Name: "b", IsDir: true}
	leaf := &Node{Name: "leaf", Apparent: 10}
	root.Adopt(a)
	a.Adopt(b)
	b.Adopt(leaf)

	// depth 1: root + direct children only (a), not b/leaf.
	c1 := root.ShallowClone(1, 100)
	if len(c1.Children) != 1 || c1.Children[0].Name != "a" {
		t.Fatalf("depth1 children = %+v", c1.Children)
	}
	if len(c1.Children[0].Children) != 0 {
		t.Errorf("depth1 should not clone grandchildren, got %d", len(c1.Children[0].Children))
	}
	// depth 2 reaches b but not leaf.
	c2 := root.ShallowClone(2, 100)
	if len(c2.Children[0].Children) != 1 || c2.Children[0].Children[0].Name != "b" {
		t.Fatalf("depth2 wrong: %+v", c2.Children[0].Children)
	}
	if len(c2.Children[0].Children[0].Children) != 0 {
		t.Errorf("depth2 should not clone depth-3 leaf")
	}
}

func TestShallowCloneRespectsCap(t *testing.T) {
	// root with 50 direct file children; cap=10 stops at 10.
	root := &Node{Name: "root", IsDir: true}
	for i := 0; i < 50; i++ {
		root.Adopt(&Node{Name: "f", Apparent: 1})
	}
	c := root.ShallowClone(1, 10)
	// 1 root + 9 children = 10 nodes used, then cap hit.
	if got := countNodes(c); got > 10 {
		t.Errorf("cap exceeded: cloned %d nodes, cap 10", got)
	} else if got < 2 {
		t.Errorf("cap should still allow some children, got %d", got)
	}
}

func TestShallowCloneCopiesAggregates(t *testing.T) {
	root := &Node{Name: "root", IsDir: true, Apparent: 150, Alloc: 200, FileCount: 7, DirCount: 2}
	root.Adopt(&Node{Name: "a", Apparent: 100})
	c := root.ShallowClone(5, 100)
	if c.Apparent != 150 || c.Alloc != 200 || c.FileCount != 7 || c.DirCount != 2 {
		t.Errorf("cloned aggregates = %+v, want originals", c)
	}
}

func TestShallowCloneNilAndZero(t *testing.T) {
	if (*Node)(nil).ShallowClone(2, 10) != nil {
		t.Error("nil ShallowClone should be nil")
	}
	if (&Node{Name: "x"}).ShallowClone(2, 0) != nil {
		t.Error("cap<=0 should yield nil")
	}
}

func countNodes(n *Node) int {
	if n == nil {
		return 0
	}
	c := 1
	for _, ch := range n.Children {
		c += countNodes(ch)
	}
	return c
}
