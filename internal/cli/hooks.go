package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// The optional client hook (PRD §16): catch mistakes in hand-written files
// before the push.

const hookMarker = "# installed by ka hooks install"

// backupSuffix names a foreign hook that ka hooks install --force replaced.
const backupSuffix = ".ka-backup"

const preCommit = `#!/bin/sh
` + hookMarker + ` (remove with ka hooks uninstall)
# Formats staged tickets and comments, then validates the staged stores.
command -v ka >/dev/null 2>&1 || { echo "ka hooks: ka is not on PATH; skipping" >&2; exit 0; }
ka fmt --staged || exit 1
exec ka check --staged
`

func cmdHooks(ctx context.Context, x *IO, args []string) int {
	fl, _ := newFlags("hooks", "install [--force] | uninstall | status", x)
	force := fl.Bool("force", false, "replace an existing pre-commit hook that ka did not install (it is kept as pre-commit.ka-backup)")
	sub, args := subcommand(fl, args)
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	hook, shared, err := hookPath(ctx, x.Dir)
	if err != nil {
		fmt.Fprintln(x.Err, "ka:", err)
		return 1
	}
	cur, readErr := os.ReadFile(hook)
	ours := readErr == nil && bytes.Contains(cur, []byte(hookMarker))
	switch sub {
	case "install":
		if shared != "" && !*force {
			fmt.Fprintf(x.Err, "ka: core.hooksPath is set in your %s git config, so %s would run for every repository using it\nfix: set a hooks path for this repository (git config core.hooksPath .git/hooks), or pass --force to install there anyway\n", shared, hook)
			return 1
		}
		if readErr == nil && !ours && !*force {
			fmt.Fprintf(x.Err, "ka: %s already exists and was not installed by ka\nfix: add `ka fmt --staged && ka check --staged` to it, or pass --force to replace it (it is kept as %s)\n", hook, filepath.Base(hook)+backupSuffix)
			return 1
		}
		if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		if readErr == nil && !ours {
			backup := hook + backupSuffix
			if _, err := os.Lstat(backup); err == nil {
				fmt.Fprintf(x.Err, "ka: %s already exists; move it away, then run this again\n", backup)
				return 1
			}
			if err := os.Rename(hook, backup); err != nil {
				fmt.Fprintln(x.Err, "ka:", err)
				return 1
			}
			fmt.Fprintf(x.Out, "moved the existing hook to %s\n", backup)
		}
		if err := os.WriteFile(hook, []byte(preCommit), 0o755); err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		fmt.Fprintf(x.Out, "installed %s: commits run ka fmt --staged and ka check --staged\n", hook)
	case "uninstall":
		if readErr != nil {
			fmt.Fprintln(x.Out, "no pre-commit hook installed")
			return 0
		}
		if !ours {
			fmt.Fprintf(x.Err, "ka: %s was not installed by ka; leaving it alone\n", hook)
			return 1
		}
		if err := os.Remove(hook); err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		fmt.Fprintf(x.Out, "removed %s\n", hook)
	case "status", "":
		switch {
		case ours:
			fmt.Fprintf(x.Out, "installed: %s\n", hook)
			if shared != "" {
				fmt.Fprintf(x.Out, "note: core.hooksPath comes from your %s git config; the hook runs for every repository using it\n", shared)
			}
		case readErr == nil:
			fmt.Fprintf(x.Out, "another pre-commit hook is installed: %s\n", hook)
		default:
			fmt.Fprintln(x.Out, "not installed (ka hooks install)")
		}
	default:
		fl.Usage()
		return 2
	}
	return 0
}

// hookPath is the repository's pre-commit hook, honoring core.hooksPath.
// shared is the config scope (global, system) of a core.hooksPath that is
// not this repository's own: a hook there runs for other repositories too.
func hookPath(ctx context.Context, dir string) (hook, shared string, err error) {
	hp, err := gitOut(ctx, dir, "rev-parse", "--git-path", "hooks")
	if err != nil || hp == "" {
		return "", "", fmt.Errorf("not in a git repository")
	}
	if !filepath.IsAbs(hp) {
		hp = filepath.Join(dir, hp)
	}
	if scope, err := gitOut(ctx, dir, "config", "--show-scope", "--get", "core.hooksPath"); err == nil {
		if s, _, _ := strings.Cut(scope, "\t"); s == "global" || s == "system" {
			shared = s
		}
	}
	return filepath.Join(hp, "pre-commit"), shared, nil
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// stagedStores exports the stores in the index (what the commit will
// contain) to a temporary directory and returns a workspace over them. Only
// the store subtrees are exported, not the whole repository. Keys are the
// stores' real directories, so local config parents resolve, and labels are
// their paths in the repository.
func stagedStores(ctx context.Context, dir string, maps mapFlag) (*kaspec.Workspace, func(), error) {
	top, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, fmt.Errorf("--staged needs a git repository")
	}
	list, err := gitOut(ctx, top, "ls-files", "-z", "--", ".ka/store.yml", "*/.ka/store.yml")
	if err != nil {
		return nil, nil, fmt.Errorf("git ls-files: %v", err)
	}
	var roots []string // repository-relative, slash-separated; "." is the top
	for _, f := range strings.Split(list, "\x00") {
		if f == "" || slices.Contains(strings.Split(f, "/"), "node_modules") {
			continue
		}
		roots = append(roots, path.Dir(path.Dir(f)))
	}
	ws := newKaspecWorkspace(nil, maps)
	if len(roots) == 0 {
		return ws, func() {}, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	base = filepath.Join(base, "ka")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, nil, err
	}
	tmp, err := os.MkdirTemp(base, "staged-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	files, err := gitOut(ctx, top, append([]string{"ls-files", "-z", "--"}, roots...)...)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("git ls-files: %v", err)
	}
	cmd := exec.CommandContext(ctx, "git", "checkout-index", "-f", "-z", "--stdin", "--prefix="+filepath.ToSlash(tmp)+"/")
	cmd.Dir = top
	cmd.Stdin = strings.NewReader(files)
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("git checkout-index: %v: %s", err, out)
	}
	for _, rel := range roots {
		label := rel
		if label == "." {
			label = ""
		}
		root := filepath.Join(tmp, filepath.FromSlash(rel))
		ws.Add(os.DirFS(root), filepath.Join(top, filepath.FromSlash(rel)), label)
	}
	return ws, cleanup, nil
}

// formatFile canonicalizes a ticket or comment; ok is false when the file
// has errors and cannot be formatted. rel is store-relative.
func formatFile(rel string, data []byte) (out []byte, isStoreFile, ok bool) {
	parts := strings.Split(rel, "/")
	isTicket := len(parts) == 5 && parts[0] == "projects" && parts[2] == "tickets" && parts[4] == "ticket.md"
	isComment := len(parts) == 6 && parts[0] == "projects" && parts[2] == "tickets" && parts[4] == "comments" && strings.HasSuffix(rel, ".md")
	if !isTicket && !isComment {
		return nil, false, false
	}
	norm := kaspec.NormalizeEncoding(data)
	var diags []kaspec.Diagnostic
	if isTicket {
		var t *kaspec.Ticket
		t, diags = kaspec.ParseTicket(rel, norm)
		if t != nil {
			out = kaspec.FormatTicket(t)
		}
	} else {
		var cm *kaspec.Comment
		cm, diags = kaspec.ParseComment(rel, norm)
		if cm != nil {
			out = kaspec.FormatComment(cm)
		}
	}
	if kaspec.HasErrors(diags, false) || out == nil {
		return nil, true, false
	}
	return out, true, true
}

// fmtStaged formats staged tickets and comments and stages the result.
// A file that also has unstaged edits is reported, not touched, so a
// commit never picks up changes the user did not stage.
func fmtStaged(ctx context.Context, x *IO) int {
	top, err := gitOut(ctx, x.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintln(x.Err, "ka: --staged needs a git repository")
		return 2
	}
	list, err := gitOut(ctx, top, "diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z")
	if err != nil {
		fmt.Fprintln(x.Err, "ka:", err)
		return 1
	}
	dirty := map[string]bool{}
	if un, err := gitOut(ctx, top, "diff", "--name-only", "-z"); err == nil {
		for _, f := range strings.Split(un, "\x00") {
			dirty[f] = true
		}
	}
	code := 0
	for _, f := range strings.Split(list, "\x00") {
		if f == "" || !strings.HasSuffix(f, ".md") {
			continue
		}
		i := strings.Index("/"+f, "/projects/")
		if i < 0 {
			continue
		}
		rel := f[i:] // "/"+f shifted the index by one, so f[i:] starts at projects/
		data, err := exec.CommandContext(ctx, "git", "-C", top, "show", ":"+f).Output()
		if err != nil {
			continue
		}
		out, isStore, ok := formatFile(rel, data)
		if !isStore {
			continue
		}
		if !ok {
			fmt.Fprintf(x.Err, "ka fmt: %s has errors; ka check --staged explains them\n", f)
			continue
		}
		if bytes.Equal(out, data) {
			continue
		}
		if dirty[f] {
			fmt.Fprintf(x.Err, "ka fmt: %s is not canonical and also has unstaged edits; run ka fmt and stage it again\n", f)
			code = 1
			continue
		}
		if err := os.WriteFile(filepath.Join(top, filepath.FromSlash(f)), out, 0o644); err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		if _, err := gitOut(ctx, top, "add", "--", f); err != nil {
			fmt.Fprintln(x.Err, "ka: git add", f, err)
			return 1
		}
		fmt.Fprintln(x.Out, "formatted", f)
	}
	return code
}
