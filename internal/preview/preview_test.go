package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadHeadTailAndBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(path, []byte("abcdefghij"), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := Read(path, Options{Limit: 4})
	if err != nil || head.Text != "abcd" || !head.Truncated || head.Offset != 0 {
		t.Fatalf("head = %+v, %v", head, err)
	}
	tail, err := Read(path, Options{Limit: 4, Tail: true})
	if err != nil || tail.Text != "ghij" || tail.Offset != 6 {
		t.Fatalf("tail = %+v, %v", tail, err)
	}
	bin := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(bin, []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Read(bin, Options{})
	if err != nil || !b.Binary || !strings.Contains(b.Hex, "00 01 02") {
		t.Fatalf("binary = %+v, %v", b, err)
	}
}
