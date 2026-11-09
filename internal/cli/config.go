package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/KanArtist/ka/internal/render"
)

func cmdConfig(ctx context.Context, x *IO, args []string) int {
	if len(args) == 0 || args[0] != "explain" {
		fmt.Fprintln(x.Err, "usage: ka config explain [--project KEY] [KEY]")
		return 2
	}
	fl, c := newFlags("config explain", "[--project KEY] [KEY]", x)
	project := fl.String("project", "", "project key (default: the workspace's default project)")
	pos, err := parse(fl, args[1:])
	if err != nil {
		return 2
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	key := ""
	if len(pos) > 0 {
		key = pos[0]
	}
	out, err := svc.ConfigExplain(*project, key)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.ConfigExplain(out))
}

func cmdProject(ctx context.Context, x *IO, args []string) int {
	if len(args) > 0 && args[0] == "rename" {
		return projectRename(ctx, x, args[1:])
	}
	if len(args) == 0 || args[0] != "move" {
		fmt.Fprintln(x.Err, "usage: ka project move KEY --to STORE [--dry-run]\n       ka project rename KEY [--key NEW] [--prefix P] [--keep-former-prefix] [--dry-run]")
		return 2
	}
	fl, c := newFlags("project move", "KEY --to STORE [--dry-run]", x)
	to := fl.String("to", "", "the store to move the project into (name or URL; must be in the workspace)")
	dry := fl.Bool("dry-run", false, "report what would move without changing anything")
	pos, err := parse(fl, args[1:])
	if err != nil {
		return 2
	}
	if len(pos) != 1 || *to == "" {
		fl.Usage()
		return 2
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.MoveProject(ctx, caller, pos[0], *to, *dry)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.MoveProject(out))
}

func projectRename(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("project rename", "KEY [--key NEW] [--prefix P] [--keep-former-prefix] [--dry-run]", x)
	newKey := fl.String("key", "", "the new project key")
	prefix := fl.String("prefix", "", "the new ID prefix (default: the new key upper-cased, when the key changes)")
	keep := fl.Bool("keep-former-prefix", false, "record the old prefix in former_prefixes, so old IDs keep resolving")
	dry := fl.Bool("dry-run", false, "report what would change without changing anything")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 || (*newKey == "" && *prefix == "") {
		fl.Usage()
		return 2
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.RenameProject(ctx, caller, pos[0], *newKey, strings.ToUpper(*prefix), *keep, *dry)
	if err != nil {
		return fail(x, c, err)
	}
	var b strings.Builder
	verb := "renamed"
	if out.DryRun {
		verb = "would rename"
	}
	fmt.Fprintf(&b, "%s project %s to %s: %d tickets %s- to %s-\n", verb, out.From, out.To, out.Tickets, out.OldPrefix, out.NewPrefix)
	stores := make([]string, 0, len(out.Files))
	for st := range out.Files {
		stores = append(stores, st)
	}
	sort.Strings(stores)
	for _, st := range stores {
		fmt.Fprintf(&b, "  %s: %d file(s) rewritten", st, out.Files[st])
		if h := out.Commits[st]; h != "" {
			fmt.Fprintf(&b, ", commit %s", h)
		}
		b.WriteString("\n")
	}
	if out.KeptOld {
		fmt.Fprintf(&b, "old IDs (%s-...) still resolve: former_prefixes in project.yml\n", out.OldPrefix)
	}
	if len(out.Skipped) > 0 {
		fmt.Fprintf(&b, "read-only, not rewritten: %s\n", strings.Join(out.Skipped, ", "))
	}
	if len(out.Pushed) > 0 {
		fmt.Fprintf(&b, "pushed: %s\n", strings.Join(out.Pushed, ", "))
	}
	if out.PushError != "" {
		fmt.Fprintf(&b, "push failed: %s (run ka sync)\n", out.PushError)
	}
	if out.From != out.To {
		fmt.Fprintf(&b, "update any .ka.yml or scripts that name project %s\n", out.From)
	}
	return emit(x, c, out, b.String())
}
