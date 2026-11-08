package cli

import (
	"context"
	"fmt"

	"github.com/JumpKickOSS/ka/internal/render"
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
	if len(args) == 0 || args[0] != "move" {
		fmt.Fprintln(x.Err, "usage: ka project move KEY --to STORE [--dry-run]")
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
