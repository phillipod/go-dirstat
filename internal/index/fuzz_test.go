package index

import (
	"testing"
	"time"
)

func FuzzSnapshotUnmarshal(f *testing.F) {
	valid := &Snapshot{
		Root: "/tmp/root", Fingerprint: "scope", ScannedAt: time.Unix(1, 0), Dirs: 1,
		Nodes: []FlatNode{{Name: "root", IsDir: true, Parent: -1}},
	}
	data, err := valid.Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data, valid.Fingerprint)
	f.Add([]byte("not a snapshot"), "scope")
	f.Fuzz(func(t *testing.T, data []byte, fingerprint string) {
		if len(data) > 1<<20 || len(fingerprint) > 4096 {
			return
		}
		snapshot, err := Unmarshal(data, fingerprint)
		if err == nil && snapshot == nil {
			t.Fatal("successful unmarshal returned a nil snapshot")
		}
	})
}
