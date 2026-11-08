package render

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/JumpKickOSS/ka/internal/engine"
)

// ConfigExplain renders `ka config explain`.
func ConfigExplain(o *engine.ConfigExplainOut) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project %s (store %s)\nlayers: %s\n\n", o.Project, o.Store, strings.Join(o.Layers, " → "))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, e := range o.Entries {
		src := e.Source
		if e.Line > 0 {
			src = fmt.Sprintf("%s:%d", src, e.Line)
		}
		var notes []string
		if e.Note == "deleted" {
			src = "<deleted by> " + src
		} else if e.Note != "" {
			notes = append(notes, e.Note)
		}
		if e.LockedBy != "" {
			notes = append(notes, "locked by "+e.LockedBy)
		}
		val := e.Value
		if len(val) > 60 {
			val = val[:57] + "..."
		}
		extra := ""
		if len(notes) > 0 {
			extra = "(" + strings.Join(notes, "; ") + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Key, val, src, extra)
	}
	tw.Flush()
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// MoveProject renders `ka project move`.
func MoveProject(o *engine.MoveProjectOut) string {
	if o.DryRun {
		return fmt.Sprintf("would move project %s (%d tickets, %d files) from %s to %s", o.Project, o.Tickets, o.Files, o.From, o.To)
	}
	s := fmt.Sprintf("moved project %s (%d tickets, %d files) from %s to %s\n%s: %s (project added)\n%s: %s (tombstone left)",
		o.Project, o.Tickets, o.Files, o.From, o.To, o.To, o.ToCommit, o.From, o.FromCommit)
	if o.PushError != "" {
		s += "\npush failed: " + o.PushError + "\nfix: run ka sync once the remote is reachable (the source is pushed only after the target)"
	} else if len(o.Pushed) > 0 {
		s += "\npushed: " + strings.Join(o.Pushed, ", ")
	}
	return s
}
