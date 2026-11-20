//go:build unix

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Without a home directory the runtime file goes to ka-<uid> in the
// temporary directory, which must be ours and private: another user could
// have created it first.
func TestRuntimeDirFallbackIsPrivate(t *testing.T) {
	tmp := t.TempDir()
	for k, v := range map[string]string{"KA_RUNTIME_DIR": "", "XDG_RUNTIME_DIR": "", "XDG_CACHE_HOME": "", "HOME": "", "TMPDIR": tmp} {
		t.Setenv(k, v)
	}
	want := filepath.Join(tmp, fmt.Sprintf("ka-%d", os.Getuid()))
	dir, err := MakeRuntimeDir()
	if err != nil || dir != want {
		t.Fatalf("MakeRuntimeDir() = %q, %v; want %q", dir, err, want)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if err := WriteRuntime(&Runtime{WorkspaceHash: "abc", Token: "t"}); err != nil {
		t.Fatal(err)
	}

	// A directory others can write to is refused.
	os.Chmod(dir, 0o777)
	if _, err := MakeRuntimeDir(); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("world-writable: %v", err)
	}
	if err := WriteRuntime(&Runtime{WorkspaceHash: "abc", Token: "t"}); err == nil {
		t.Fatal("runtime file written to a world-writable directory")
	}
	os.Chmod(dir, 0o700)

	// So is a symlink to somewhere else.
	os.RemoveAll(dir)
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := MakeRuntimeDir(); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("symlink: %v", err)
	}
}

// A directory owned by someone else is refused: / belongs to root.
func TestCheckPrivateDirOwner(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, who owns /")
	}
	if err := checkPrivateDir("/"); err == nil || !strings.Contains(err.Error(), "owned by uid 0") {
		t.Fatalf("foreign owner: %v", err)
	}
	if err := checkPrivateDir(t.TempDir()); err != nil {
		t.Fatalf("own private directory: %v", err)
	}
}
