package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/render"
)

// cmdCompact squashes a store's history (docs/engine.md "Compacting
// history"). Without --yes it only reports what would happen.
func cmdCompact(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("compact", "--store NAME [--summary-file F | --summary TEXT] [--dry-run] --yes", x)
	store := fl.String("store", "", "the store to compact (its workspace name)")
	summary := fl.String("summary", "", "the narrative for the summary file (Markdown)")
	summaryFile := fl.String("summary-file", "", "read the narrative from a file (- for stdin)")
	dry := fl.Bool("dry-run", false, "report what would happen; change nothing")
	yes := fl.Bool("yes", false, "really compact: rewrite the shared history (not undoable on the remote)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) > 0 || *store == "" {
		return usageErr(x, fl, "name the store with --store")
	}
	if *summary != "" && *summaryFile != "" {
		return usageErr(x, fl, "use --summary or --summary-file, not both")
	}
	text := *summary
	if *summaryFile == "-" {
		text = readText(x, "-")
	} else if *summaryFile != "" {
		b, err := os.ReadFile(*summaryFile)
		if err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		text = string(b)
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	in := engine.CompactIn{Store: *store, Summary: text, DryRun: *dry || !*yes}
	if *yes && !*dry {
		in.Confirm = *store
	}
	out, err := svc.Compact(ctx, caller, in)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Compact(out))
}

// cmdHistory lists history summaries, or shows one.
func cmdHistory(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("history", "[--store NAME] [FILE]", x)
	store := fl.String("store", "", "only this store")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) > 1 {
		return usageErr(x, fl, "name at most one summary file")
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	if len(pos) == 1 {
		out, err := svc.HistorySummary(*store, pos[0])
		if err != nil {
			return fail(x, c, err)
		}
		return emit(x, c, out, render.HistorySummary(out))
	}
	out, err := svc.History(ctx, *store)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.History(out))
}
