package gitx

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func testRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// A ka run from a hook inherits GIT_DIR and GIT_INDEX_FILE from the calling
// git; they must not redirect ka's own repository.
func TestInheritedRepoEnvIsIgnored(t *testing.T) {
	ours := testRepo(t)
	other := testRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))

	ctx := context.Background()
	r := &Repo{Dir: ours}
	gd, err := r.GitDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(ours, ".git")); !sameFile(gd, want) {
		t.Fatalf("git dir %s, want %s", gd, want)
	}
	// The opt-in keeps them (the merge driver runs inside git's own repository).
	in := &Repo{Dir: ours, InheritGitEnv: true}
	if gd, _ := in.GitDir(ctx); sameFile(gd, filepath.Join(ours, ".git")) {
		t.Fatalf("InheritGitEnv ignored GIT_DIR: %s", gd)
	}
}

func sameFile(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra == rb
}

// Commit messages may contain the ASCII separators the log format used to
// rely on.
func TestLogSurvivesSeparatorsInMessages(t *testing.T) {
	dir := testRepo(t)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "odd \x1e subject \x1f here\n\nbody\n\nKa-Actor: bob")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "second")
	cs, err := (&Repo{Dir: dir}).Log(context.Background(), "HEAD~2..HEAD", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Subject != "odd \x1e subject \x1f here" || cs[0].Trailers["Ka-Actor"] != "bob" || cs[1].Subject != "second" {
		t.Fatalf("log: %+v", cs)
	}
}

func TestUpstream(t *testing.T) {
	dir := testRepo(t)
	ctx := context.Background()
	r := &Repo{Dir: dir}
	if rm, b := r.Upstream(ctx); rm != "" || b != "" {
		t.Fatalf("no upstream: %q %q", rm, b)
	}
	bare := t.TempDir()
	git(t, bare, "init", "-q", "--bare", "-b", "main")
	git(t, dir, "remote", "add", "origin", bare)
	git(t, dir, "push", "-q", "-u", "origin", "main")
	if rm, b := r.Upstream(ctx); rm != "origin" || b != "main" {
		t.Fatalf("upstream: %q %q", rm, b)
	}
	// An unborn branch with an upstream configured (a clone of an empty remote).
	empty := t.TempDir()
	git(t, empty, "init", "-q", "-b", "main")
	git(t, empty, "config", "branch.main.remote", "origin")
	git(t, empty, "config", "branch.main.merge", "refs/heads/main")
	if rm, b := (&Repo{Dir: empty}).Upstream(ctx); rm != "origin" || b != "main" {
		t.Fatalf("unborn upstream: %q %q", rm, b)
	}
}

func TestIsNonFastForward(t *testing.T) {
	for _, s := range []string{
		" ! [rejected]        main -> main (fetch first)",
		" ! [remote rejected] HEAD -> main (incorrect old value provided)",
	} {
		if !IsNonFastForward(&Error{Stderr: s}) {
			t.Errorf("not a lost race: %q", s)
		}
	}
	if IsNonFastForward(&Error{Stderr: " ! [remote rejected] main -> main (pre-receive hook declined)"}) {
		t.Error("a hook rejection taken for a lost race")
	}
}
