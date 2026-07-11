package fsinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectAndIdentityChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte("abc"), 0o640); err != nil {
		t.Fatal(err)
	}
	first, err := Inspect(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != "file" || first.Size != 3 || first.Path != path {
		t.Fatalf("entry = %+v", first)
	}
	second, err := Inspect(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if !SameObject(first, second) {
		t.Fatal("unchanged file did not retain identity")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	replaced, err := Inspect(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if SameObject(first, replaced) {
		t.Fatal("replacement file matched stale identity")
	}
}

func TestInspectSymlinkDoesNotFollowByDefault(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	e, err := Inspect(link, false)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "symlink" || e.Symlink == "" {
		t.Fatalf("symlink entry = %+v", e)
	}
}

func TestVolumeFor(t *testing.T) {
	v, err := VolumeFor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v.Total == 0 || v.UsedPct < 0 || v.UsedPct > 100 {
		t.Fatalf("volume = %+v", v)
	}
}
