//go:build windows

package scan

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeWindowsFinalPath(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{name: "drive", input: `\\?\C:\data\target`, want: `C:\data\target`},
		{name: "unc", input: `\\?\UNC\server\share\target`, want: `\\server\share\target`},
		{name: "volume", input: `\\?\Volume{abc}\target`, want: `\\?\Volume{abc}\target`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeWindowsFinalPath(test.input); got != test.want {
				t.Fatalf("normalizeWindowsFinalPath(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestIdentityOfPathNoFollowSymlinkUsesReparseHandle(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("Lstat(%q) mode = %s, want symlink", link, linkInfo.Mode())
	}

	targetDev, targetIno, targetOK := identityOfPath(target, targetInfo)
	linkDev, linkIno, linkOK := identityOfPath(link, linkInfo)
	if !targetOK || !linkOK {
		t.Fatalf("identity target=%d/%d/%v link=%d/%d/%v", targetDev, targetIno, targetOK, linkDev, linkIno, linkOK)
	}
	if targetDev == linkDev && targetIno == linkIno {
		t.Fatalf("no-follow symlink metadata followed target: target=%d/%d link=%d/%d", targetDev, targetIno, linkDev, linkIno)
	}
}

func TestResolvedAliasPathFollowsJunctionTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	alias := filepath.Join(root, "junction")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("cmd", "/c", "mklink", "/J", alias, target).Run(); err != nil {
		t.Skipf("junctions unavailable: %v", err)
	}

	resolved, err := resolvedAliasPath(alias)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(target)) {
		t.Fatalf("resolvedAliasPath(%q) = %q, want target %q", alias, resolved, target)
	}
}
