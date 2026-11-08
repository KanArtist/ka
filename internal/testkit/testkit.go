// Package testkit builds throwaway git remotes and clones of stores for
// tests: the multi-clone simulator, scenarios and token budgets.
package testkit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JumpKickOSS/ka/internal/engine"
)

// GitEnv isolates git and ka from the developer's configuration.
func GitEnv(t testing.TB) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KA_CONFIG_DIR", filepath.Join(home, "ka-config"))
	t.Setenv("KA_DATA_DIR", filepath.Join(home, "ka-data"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test User")
	t.Setenv("GIT_AUTHOR_EMAIL", "tester@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test User")
	t.Setenv("GIT_COMMITTER_EMAIL", "tester@example.com")
	t.Setenv("KA_WORKSPACE", "")
	t.Setenv("KA_ACTOR", "")
	return home
}

// Git runs git and fails the test on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Remote is a bare repository holding a store.
type Remote struct {
	t    testing.TB
	Root string // scratch directory
	Path string // the bare repository
	n    int
}

// NewRemote creates a bare remote whose store has project jk (prefix JK)
// plus the given files (store-relative path -> content).
func NewRemote(t testing.TB, files map[string]string) *Remote {
	t.Helper()
	return NewStoreRemote(t, engine.InitOptions{Name: "test", Project: "jk", Prefix: "JK"}, files)
}

// NewStoreRemote creates a bare remote holding a store made with o (o.Dir
// is ignored) plus the given extra files.
func NewStoreRemote(t testing.TB, o engine.InitOptions, files map[string]string) *Remote {
	t.Helper()
	r := &Remote{t: t, Root: t.TempDir()}
	r.Path = filepath.Join(r.Root, "remote.git")
	Git(t, r.Root, "init", "-q", "--bare", "-b", "main", r.Path)
	seed := filepath.Join(r.Root, "seed")
	o.Dir = seed
	if _, err := engine.InitStore(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		fp := filepath.Join(seed, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(fp), 0o755)
		if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, seed, "add", "-A")
	Git(t, seed, "commit", "-q", "-m", "init")
	Git(t, seed, "remote", "add", "origin", r.Path)
	Git(t, seed, "push", "-q", "-u", "origin", "main")
	return r
}

// CountPushes installs a hook that counts pushes received; read with Pushes.
func (r *Remote) CountPushes() {
	hook := filepath.Join(r.Path, "hooks", "post-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\necho push >> \"$GIT_DIR/pushes.log\"\n"), 0o755)
}

// Pushes returns how many pushes the remote has received since CountPushes.
func (r *Remote) Pushes() int {
	b, _ := os.ReadFile(filepath.Join(r.Path, "pushes.log"))
	return strings.Count(string(b), "push")
}

// Clone makes a new working clone and returns its directory.
func (r *Remote) Clone() string {
	r.t.Helper()
	r.n++
	dir := filepath.Join(r.Root, "clone"+itoa(r.n))
	Git(r.t, r.Root, "clone", "-q", r.Path, dir)
	return dir
}

// Service opens a service over a clone.
func Service(t testing.TB, dir string) *engine.Service {
	t.Helper()
	ws, err := engine.Open(context.Background(), engine.OpenOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	s, err := engine.New(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
