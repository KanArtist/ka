package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEnv is a bare "remote" plus any number of clones of one store.
type testEnv struct {
	t      *testing.T
	root   string
	remote string
	n      int
}

func gitEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KA_CONFIG_DIR", filepath.Join(home, "ka-config"))
	t.Setenv("KA_DATA_DIR", filepath.Join(home, "ka-data"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test User")
	t.Setenv("GIT_AUTHOR_EMAIL", "tester@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test User")
	t.Setenv("GIT_COMMITTER_EMAIL", "tester@example.com")
	t.Setenv("KA_WORKSPACE", "")
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newEnv creates a remote holding a store with project jk (prefix JK) and
// the given extra files.
func newEnv(t *testing.T, files map[string]string) *testEnv {
	t.Helper()
	gitEnv(t)
	e := &testEnv{t: t, root: t.TempDir()}
	e.remote = filepath.Join(e.root, "remote.git")
	run(t, e.root, "init", "-q", "--bare", "-b", "main", e.remote)
	seed := filepath.Join(e.root, "seed")
	if _, err := InitStore(context.Background(), InitOptions{Dir: seed, Name: "test", Project: "jk", Prefix: "JK"}); err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		fp := filepath.Join(seed, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(fp), 0o755)
		os.WriteFile(fp, []byte(c), 0o644)
	}
	run(t, seed, "add", "-A")
	run(t, seed, "commit", "-q", "-m", "init")
	run(t, seed, "remote", "add", "origin", e.remote)
	run(t, seed, "push", "-q", "-u", "origin", "main")
	return e
}

// clone makes a new clone and returns a service over it.
func (e *testEnv) clone() (*Service, string) {
	e.t.Helper()
	e.n++
	dir := filepath.Join(e.root, "clone"+string(rune('a'+e.n-1)))
	run(e.t, e.root, "clone", "-q", e.remote, dir)
	return e.open(dir), dir
}

func (e *testEnv) open(dir string) *Service {
	e.t.Helper()
	ws, err := Open(context.Background(), OpenOptions{Dir: dir})
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := New(context.Background(), ws)
	if err != nil {
		e.t.Fatal(err)
	}
	s.PollStep = 20 * time.Millisecond
	for _, st := range ws.Stores {
		st.sleep = func(time.Duration) {}
	}
	return s
}

func agent(name string) Caller {
	return Caller{Actor: "agent/" + name, OnBehalfOf: "tester", Session: "S-" + name}
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}
