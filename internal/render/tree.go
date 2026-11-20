package render

import (
	"fmt"
	"strings"

	"github.com/KanArtist/ka/internal/engine"
)

// Tree renders initiatives, epics and their tickets, one line each,
// indented by depth:
//
//	JK-9GDPR-gdpr P1 initiative in_progress GDPR · 1/5 done, 1 started, 1 blocked, 1 ready
//	  JK-4EPC1-consent-banner epic backlog Consent banner · 1/2 done, 1 ready
//	    JK-7TK2A-show-banner P1 ready Show the banner
//	    … 1 more
func Tree(out *engine.TreeOut) string {
	var b strings.Builder
	for _, n := range out.Nodes {
		treeNode(&b, n, 0)
	}
	if len(out.Nodes) == 0 {
		b.WriteString("no initiatives or epics\n")
	}
	return b.String()
}

func treeNode(b *strings.Builder, n *engine.TreeNode, depth int) {
	pad := strings.Repeat("  ", depth)
	b.WriteString(pad + or(n.Ref, n.ID))
	if n.Priority != "" {
		b.WriteString(" " + n.Priority)
	}
	if n.Rollup != nil && n.Kind != "" {
		b.WriteString(" " + n.Kind) // work tickets' kinds are in the JSON
	}
	b.WriteString(" " + n.Status)
	if n.Claim != "" {
		b.WriteString(" @" + n.Claim)
	}
	if n.Blocked {
		b.WriteString(" BLOCKED")
	}
	b.WriteString(" " + n.Title)
	if n.Rollup != nil {
		b.WriteString(" · " + Rollup(n.Rollup))
	}
	b.WriteString("\n")
	for _, c := range n.Children {
		treeNode(b, c, depth+1)
	}
	if n.More > 0 {
		fmt.Fprintf(b, "%s  … %d more\n", pad, n.More)
	}
}

// Rollup renders a container's counts: "3/8 done, 2 started, 1 blocked,
// 2 ready" (zero counts left out).
func Rollup(r *engine.Rollup) string {
	if r.Total == 0 {
		return "no tickets"
	}
	parts := []string{fmt.Sprintf("%d/%d done", r.Done, r.Total)}
	for _, p := range []struct {
		n    int
		what string
	}{{r.InProgress, "started"}, {r.Blocked, "blocked"}, {r.Ready, "ready"}, {r.Canceled, "canceled"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	return strings.Join(parts, ", ")
}

// FromLabel renders InitiativeFromLabel's report.
func FromLabel(out *engine.FromLabelOut) string {
	var b strings.Builder
	verb := "reused"
	switch {
	case out.Created && out.DryRun:
		verb = "would create"
	case out.Created:
		verb = "created"
	}
	ref := or(out.Initiative.Ref, out.Initiative.ID)
	if ref == "" {
		ref = "(new)"
	}
	fmt.Fprintf(&b, "initiative %s %s (%s)\n", ref, out.Initiative.Title, verb)
	removed := 0
	for _, t := range out.Tickets {
		line := t.Action + " " + or(t.Ref, t.ID) + " " + t.Title
		switch t.Action {
		case engine.LabelViaEpic:
			line += " (via epic " + t.Via + ")"
		case engine.LabelSkipped:
			line += ": " + t.Reason
		}
		b.WriteString(line + "\n")
		if t.LabelRemoved {
			removed++
		}
	}
	if len(out.Tickets) == 0 {
		fmt.Fprintf(&b, "no tickets carry label %s\n", out.Label)
	}
	if removed > 0 {
		verb := "removed from"
		if out.DryRun {
			verb = "to remove from"
		}
		fmt.Fprintf(&b, "label %s %s %d tickets\n", out.Label, verb, removed)
	}
	if out.DryRun {
		b.WriteString("dry run: nothing written\n")
	} else if out.PushPending {
		b.WriteString("push pending\n")
	} else if note := strings.TrimPrefix(pushNote(out.Pushed, out.Local, ""), ", "); note != "" {
		b.WriteString(note + "\n")
	}
	if out.Error != nil {
		b.WriteString("stopped: " + out.Error.Error() + "\n")
	}
	return b.String()
}
