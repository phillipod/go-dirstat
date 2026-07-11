package fsinfo

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
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

func TestCapturePathRecordsAbsenceAndIdentity(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "destination")
	absent, err := CapturePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if absent.Path != path || absent.Exists || absent.Entry != nil {
		t.Fatalf("absent expectation = %#v", absent)
	}
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	present, err := CapturePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if present.Path != path || !present.Exists || present.Entry == nil || present.Entry.Size != 7 {
		t.Fatalf("present expectation = %#v", present)
	}
}

func TestVolumeFor(t *testing.T) {
	v, err := VolumeFor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v.Total == 0 || v.UsedPct < 0 || v.UsedPct > 100 || v.ResolvedPath == "" {
		t.Fatalf("volume = %+v", v)
	}
}

func TestFinalizeVolumeSeparatesPhysicalUseFromCallerPressure(t *testing.T) {
	volume := Volume{Total: 1000, Free: 200, Available: 100, Inodes: 10, InodesFree: 2}
	finalizeVolume(&volume)
	if volume.PhysicalUsed != 800 || volume.Reserved != 100 || volume.CallerCapacity != 900 {
		t.Fatalf("volume byte accounting = %+v", volume)
	}
	if volume.Used != volume.PhysicalUsed || volume.UsedPct != volume.PhysicalUsedPct {
		t.Fatalf("compatibility aliases diverged: %+v", volume)
	}
	if math.Abs(volume.PhysicalUsedPct-80) > 0.0001 || math.Abs(volume.CallerPressurePct-88.888888) > 0.0001 {
		t.Fatalf("volume percentages = %+v", volume)
	}
	if volume.InodePct != 80 {
		t.Fatalf("inode pressure = %f, want 80", volume.InodePct)
	}
}

func TestVolumeForUsesSymlinkTargetIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fixture uses the Linux proc mount")
	}
	link := filepath.Join(t.TempDir(), "proc-alias")
	if err := os.Symlink("/proc", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	volume, err := VolumeFor(link)
	if err != nil {
		t.Fatal(err)
	}
	if volume.Path != link || volume.ResolvedPath != "/proc" || volume.MountPoint != "/proc" || volume.Filesystem != "proc" {
		t.Fatalf("symlink volume identity = %+v", volume)
	}
}

func TestVolumeForRejectsMissingPath(t *testing.T) {
	if _, err := VolumeFor(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing volume path was accepted")
	}
}
