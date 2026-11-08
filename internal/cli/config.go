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
