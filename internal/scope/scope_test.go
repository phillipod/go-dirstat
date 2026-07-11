package scope

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyVirtualAlwaysExcluded(t *testing.T) {
	p := New()
	// Same device, but under a virtual path -> never descend, even crossing on.
	p.CrossDevice = true
	if p.Descend("/proc/123", 9, 9, "proc") {
		t.Error("/proc should be excluded even with same device")
	}
	if p.Descend("/sys/kernel", 9, 9, "sysfs") {
		t.Error("/sys should be excluded")
	}
	if p.Descend("/dev/null-dir", 9, 9, "devtmpfs") {
		t.Error("/dev should be excluded")
	}
}

func TestPolicyCrossDeviceDefault(t *testing.T) {
	p := New()
	if p.Descend("/mnt/extra", 2, 1, "ext4") {
		t.Error("should not cross devices by default")
	}
	p.CrossDevice = true
	if !p.Descend("/mnt/extra", 2, 1, "ext4") {
		t.Error("should cross devices when enabled and not virtual")
	}
}

func TestPolicyAllowsTargetAppliesAllBoundaryRulesToFiles(t *testing.T) {
	p := New(
		WithIncludePaths([]string{"/srv/allowed"}),
		WithFilesystems(nil, []string{"tmpfs"}),
	)
	if !p.AllowsTarget("/srv/allowed/file", 1, 1, "ext4") {
		t.Fatal("in-scope file on the parent filesystem was rejected")
	}
	if p.AllowsTarget("/srv/outside/file", 1, 1, "ext4") {
		t.Fatal("file outside the include path was allowed")
	}
	if p.AllowsTarget("/srv/allowed/file", 2, 1, "ext4") {
		t.Fatal("file on another device was allowed by the default policy")
	}
	p.CrossDevice = true
	if p.AllowsTarget("/srv/allowed/file", 2, 1, "tmpfs") {
		t.Fatal("file on an excluded filesystem was allowed")
	}
}

func TestPolicyPseudoFSTypeSkippedEvenWhenCrossing(t *testing.T) {
	p := New()
	p.CrossDevice = true
	// A proc mount bind-mounted at a non-denylisted path must still be skipped.
	if p.Descend("/srv/proc-bindmount", 5, 1, "proc") {
		t.Error("pseudo fstype must be skipped even when crossing")
	}
	if p.Descend("/srv/data", 5, 1, "ext4") {
		// ok path
	} else {
		t.Error("real fstype should be allowed when crossing")
	}
}

func TestPolicyIncludeExcludeFS(t *testing.T) {
	p := New(WithFilesystems([]string{"ext4", "btrfs"}, nil))
	p.CrossDevice = true
	if !p.Descend("/mnt/x", 2, 1, "ext4") {
		t.Error("ext4 in include list should be allowed")
	}
	if p.Descend("/mnt/y", 2, 1, "xfs") {
		t.Error("xfs not in include list should be excluded")
	}

	p2 := New(WithFilesystems(nil, []string{"tmpfs", "fuse"}))
	p2.CrossDevice = true
	if p2.Descend("/mnt/z", 2, 1, "tmpfs") {
		t.Error("tmpfs in exclude list should be excluded")
	}
	if !p2.Descend("/mnt/z", 2, 1, "ext4") {
		t.Error("ext4 not excluded should be allowed")
	}

	p3 := New(WithFilesystems([]string{" EXT4 "}, nil))
	p3.CrossDevice = true
	if !p3.Descend("/mnt/case", 2, 1, "ext4") {
		t.Error("filesystem filters should be whitespace-trimmed and case-normalized")
	}
}

func TestPolicyAllowsFilesystem(t *testing.T) {
	t.Run("default real filesystem", func(t *testing.T) {
		p := New()
		if !p.AllowsFilesystem("ext4") {
			t.Error("default policy should allow a real filesystem")
		}
	})

	t.Run("pseudo filesystem", func(t *testing.T) {
		p := New()
		if p.AllowsFilesystem("proc") {
			t.Error("default policy should reject a pseudo-filesystem")
		}
	})

	t.Run("include list and unknown filesystem", func(t *testing.T) {
		p := New(WithFilesystems([]string{"ext4"}, nil))
		if p.AllowsFilesystem("") {
			t.Error("unknown filesystem cannot satisfy an include list")
		}
		if p.AllowsFilesystem("xfs") {
			t.Error("filesystem outside include list should be rejected")
		}
	})

	t.Run("exclude list", func(t *testing.T) {
		p := New(WithFilesystems(nil, []string{"tmpfs"}))
		if p.AllowsFilesystem("tmpfs") {
			t.Error("filesystem in exclude list should be rejected")
		}
	})
}

func TestPolicyGlobAndPathAndHidden(t *testing.T) {
	p := New(WithExcludeGlobs([]string{"*.o", "*.tmp"}), WithExcludePaths([]string{"/bad"}), WithIncludePaths([]string{"/good"}))
	if p.Entry("a.o", "a.o", "/good/a.o") {
		t.Error("*.o should be excluded by glob")
	}
	if p.Entry("x.tmp", "x.tmp", "/good/x.tmp") {
		t.Error("*.tmp should be excluded by glob")
	}
	if p.Entry("bad", "bad", "/bad/x") {
		t.Error("/bad path should be excluded")
	}
	if p.Entry("outside", "outside", "/elsewhere/outside") {
		t.Error("path outside include whitelist should be excluded")
	}
	if !p.Entry("a.go", "a.go", "/good/a.go") {
		t.Error("a.go under /good should be included")
	}

	ph := New(WithHidden(false))
	if ph.Entry(".secret", ".secret", "/x/.secret") {
		t.Error("hidden file should be excluded when IncludeHidden=false")
	}
	if !ph.Entry("visible", "visible", "/x/visible") {
		t.Error("normal file should be included")
	}
}

func TestPolicyIncludePathAllowsAncestorsButNotSiblings(t *testing.T) {
	p := New(WithIncludePaths([]string{"/srv/projects/keep"}))

	for _, path := range []string{"/", "/srv", "/srv/projects", "/srv/projects/keep", "/srv/projects/keep/child"} {
		if !p.AllowsPath(path) {
			t.Errorf("AllowsPath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"/srv/project", "/srv/projects/keeper", "/srv/other"} {
		if p.AllowsPath(path) {
			t.Errorf("AllowsPath(%q) = true, want false", path)
		}
	}
}

func TestPolicyPathPrefixUsesComponentBoundaries(t *testing.T) {
	p := New(WithExcludePaths([]string{"/tmp/data"}))
	if p.AllowsPath("/tmp/data/child") {
		t.Fatal("excluded descendant was allowed")
	}
	if !p.AllowsPath("/tmp/database") {
		t.Fatal("lexically similar sibling was excluded")
	}
}

func TestPolicyPathPrefixResolvesSymlinkAliases(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	alias := filepath.Join(base, "alias")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := New(WithExcludePaths([]string{alias}))
	if p.AllowsPath(filepath.Join(target, "child")) {
		t.Fatalf("target reached through symlink alias: alias=%q target=%q", alias, target)
	}
}

func TestPolicyFileSizeWindow(t *testing.T) {
	p := New(WithSizeThreshold(100, 1_000_000))
	if p.File(50) {
		t.Error("50B below min should be excluded")
	}
	if p.File(2_000_000) {
		t.Error("2MB above max should be excluded")
	}
	if !p.File(500) {
		t.Error("500B in window should be included")
	}
}
