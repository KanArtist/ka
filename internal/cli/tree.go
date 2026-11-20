package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/render"
)

func cmdTree(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("tree", "[ID] [--project KEY] [--depth N] [--hide-closed]", x)
	var in engine.TreeIn
	fl.StringVar(&in.Project, "project", "", "without ID: the project whose initiatives and top-level epics to show")
	fl.IntVar(&in.Depth, "depth", 0, "levels of children to show (0: all)")
	fl.BoolVar(&in.HideClosed, "hide-closed", false, "leave out closed tickets (rollups still count them)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) > 1 {
		return usageErr(x, fl, "tree takes at most one ID")
	}
	if len(pos) == 1 {
		in.ID = pos[0]
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Tree(in)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Tree(out))
}

func cmdInitiative(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("initiative", "from-label LABEL [--project KEY] [--title T] [--keep-label] [--dry-run]", x)
	var in engine.FromLabelIn
	fl.StringVar(&in.Project, "project", "", "the project to convert, which also holds the initiative")
	fl.StringVar(&in.Title, "title", "", "the initiative's title (default: the label); an initiative with this title is reused")
	fl.BoolVar(&in.KeepLabel, "keep-label", false, "leave the label on the converted tickets")
	fl.BoolVar(&in.DryRun, "dry-run", false, "report what would change without writing")
	sub, args := subcommand(fl, args)
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if sub != "from-label" {
		fmt.Fprintln(x.Err, "ka: create initiatives with ka new --kind initiative, and epics with ka new --kind epic --parent INITIATIVE")
		fl.Usage()
		return 2
	}
	if len(pos) != 1 {
		return usageErr(x, fl, "from-label needs one LABEL")
	}
	in.Label = pos[0]
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.InitiativeFromLabel(ctx, caller, in)
	if err != nil {
		return fail(x, c, err)
	}
	code := emit(x, c, out, render.FromLabel(out))
	if out.Error != nil {
		return 1
	}
	return code
}

// storeRequire is ka store require VERSION [--store NAME].
func storeRequire(ctx context.Context, x *IO, c *common, fl *flag.FlagSet, store string, pos []string) int {
	if len(pos) != 1 {
		return usageErr(x, fl, "store require needs a version, such as 1.3.0")
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.RequireKa(ctx, caller, store, pos[0])
	if err != nil {
		return fail(x, c, err)
	}
	text := fmt.Sprintf("store %s requires ka %s (already)", out.Store, out.To)
	if out.Changed {
		text = fmt.Sprintf("store %s now requires ka %s", out.Store, out.To)
		if out.From != "" {
			text += " (was " + out.From + ")"
		}
		switch {
		case out.Local:
		case out.Pushed:
			text += ", pushed"
		case out.PushNote != "":
			text += ", NOT pushed (" + out.PushNote + ")"
		default:
			text += ", committed locally"
		}
	}
	return emit(x, c, out, text)
}
