// Package gitx runs the system git binary. KanArtist shells out to git rather
// than embedding a git implementation so it inherits the user's credential
// helpers, SSH config and performance (PRD §8.1).
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// MinVersion is the oldest git ka supports.
var MinVersion = [2]int{2, 40}

// Error is a failed git invocation.
type Error struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

// Repo is a git working tree.
type Repo struct {
	Dir string
	// Env is appended to the environment of every invocation.
	Env []string
}

// baseEnv keeps git non-interactive and its output parseable.
var baseEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"LC_ALL=C",
	"GIT_OPTIONAL_LOCKS=0",
}

// Run runs git with args in the repo and returns trimmed stdout.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	out, err := r.RunRaw(ctx, nil, args...)
	return strings.TrimRight(string(out), "\n"), err
}

// RunRaw runs git with optional stdin and returns raw stdout.
func (r *Repo) RunRaw(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(append(os.Environ(), baseEnv...), r.Env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return stdout.Bytes(), &Error{Args: args, Code: code, Stderr: stderr.String()}
	}
	return stdout.Bytes(), nil
}

// OK runs git and reports whether it exited 0.
func (r *Repo) OK(ctx context.Context, args ...string) bool {
	_, err := r.RunRaw(ctx, nil, args...)
	return err == nil
}

// Toplevel returns the root of the working tree containing dir.
func Toplevel(ctx context.Context, dir string) (string, error) {
	r := &Repo{Dir: dir}
	return r.Run(ctx, "rev-parse", "--show-toplevel")
}

// GitDir returns the absolute .git directory.
func (r *Repo) GitDir(ctx context.Context) (string, error) {
	return r.Run(ctx, "rev-parse", "--absolute-git-dir")
}

// Head returns the commit HEAD points at, or "" in an empty repository.
func (r *Repo) Head(ctx context.Context) string {
	h, err := r.Run(ctx, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return h
}

// Branch returns the current branch name, or "" when detached.
func (r *Repo) Branch(ctx context.Context) string {
	b, err := r.Run(ctx, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return b
}

// Upstream returns the remote and remote branch the current branch tracks.
func (r *Repo) Upstream(ctx context.Context) (remote, branch string) {
	b := r.Branch(ctx)
	if b == "" {
		return "", ""
	}
	remote, _ = r.Run(ctx, "config", "--get", "branch."+b+".remote")
	merge, _ := r.Run(ctx, "config", "--get", "branch."+b+".merge")
	if remote == "" || remote == "." || merge == "" {
		return "", ""
	}
	return remote, strings.TrimPrefix(merge, "refs/heads/")
}

// Version returns git's major and minor version.
func Version(ctx context.Context) (int, int, string, error) {
	r := &Repo{}
	out, err := r.Run(ctx, "version")
	if err != nil {
		return 0, 0, "", err
	}
	m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(out)
	if m == nil {
		return 0, 0, out, fmt.Errorf("cannot parse %q", out)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return maj, min, out, nil
}

// InProgress reports a rebase, merge or cherry-pick left in the working tree.
func (r *Repo) InProgress(ctx context.Context) string {
	gd, err := r.GitDir(ctx)
	if err != nil {
		return ""
	}
	for _, p := range []struct{ file, what string }{
		{"rebase-merge", "rebase"}, {"rebase-apply", "rebase"},
		{"MERGE_HEAD", "merge"}, {"CHERRY_PICK_HEAD", "cherry-pick"}, {"REVERT_HEAD", "revert"},
	} {
		if _, err := os.Stat(gd + "/" + p.file); err == nil {
			return p.what
		}
	}
	return ""
}

// Commit is one commit with its message trailers.
type Commit struct {
	SHA      string
	Subject  string
	Body     string
	Author   string // "Name <email>"
	Date     string // author date, strict ISO 8601
	Trailers map[string]string
}

// Log returns commits in rev range (oldest first when reverse is true).
func (r *Repo) Log(ctx context.Context, revRange string, reverse bool, paths ...string) ([]Commit, error) {
	args := []string{"log", "--format=%H%x1f%an <%ae>%x1f%aI%x1f%B%x1e"}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, revRange)
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	out, err := r.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var cs []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) < 4 {
			continue
		}
		msg := strings.TrimRight(f[3], "\n")
		subject, body, _ := strings.Cut(msg, "\n")
		cs = append(cs, Commit{SHA: f[0], Author: f[1], Date: f[2], Subject: subject,
			Body: strings.TrimLeft(body, "\n"), Trailers: ParseTrailers(msg)})
	}
	return cs, nil
}

// ParseTrailers reads "Key: value" lines from the last paragraph of a message.
func ParseTrailers(msg string) map[string]string {
	paras := strings.Split(strings.TrimSpace(msg), "\n\n")
	if len(paras) < 2 {
		return nil
	}
	last := paras[len(paras)-1]
	m := map[string]string{}
	for _, line := range strings.Split(last, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok || strings.ContainsAny(k, " \t") || k == "" {
			return nil
		}
		m[k] = v
	}
	return m
}

// ChangedPaths lists files that differ between two commits.
func (r *Repo) ChangedPaths(ctx context.Context, from, to string) ([]string, error) {
	if from == "" {
		out, err := r.Run(ctx, "ls-tree", "-r", "--name-only", to)
		if err != nil {
			return nil, err
		}
		return splitLines(out), nil
	}
	out, err := r.Run(ctx, "diff", "--name-only", "--no-renames", from, to)
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// Status lists paths with uncommitted changes (porcelain v1, -z).
func (r *Repo) Status(ctx context.Context, paths ...string) ([]string, error) {
	args := append([]string{"status", "--porcelain", "-z", "--untracked-files=all", "--"}, paths...)
	out, err := r.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, rec := range strings.Split(out, "\x00") {
		if len(rec) > 3 {
			files = append(files, rec[3:])
		}
	}
	return files, nil
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// IsNonFastForward reports whether a push failed because the remote moved.
func IsNonFastForward(err error) bool {
	var ge *Error
	if !errors.As(err, &ge) {
		return false
	}
	s := ge.Stderr
	return strings.Contains(s, "non-fast-forward") || strings.Contains(s, "[rejected]") ||
		strings.Contains(s, "fetch first") || strings.Contains(s, "cannot lock ref") ||
		strings.Contains(s, "failed to update ref")
}
