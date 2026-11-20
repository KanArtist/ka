package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// Compact renders a compaction, or with DryRun what one would do.
func Compact(o *engine.CompactOut) string {
	var b strings.Builder
	span := dateSpan(o.FirstCommit, o.LastCommit)
	if o.DryRun {
		fmt.Fprintf(&b, "dry run: compacting store %s (%s/%s) would\n", o.Store, o.Remote, o.Branch)
		fmt.Fprintf(&b, "  drop %d commits%s%s\n", o.Commits, span, actorCounts(o.Actors))
		if o.TicketsCreated+o.TicketsClosed > 0 {
			fmt.Fprintf(&b, "  (that history created %d tickets and closed %d; the tickets themselves stay)\n", o.TicketsCreated, o.TicketsClosed)
		}
		fmt.Fprintf(&b, "  shrink this clone's objects from about %s to roughly one snapshot of the store\n", bytesHuman(o.SizeBefore))
		fmt.Fprintf(&b, "  write a summary to %s (the suffix is picked at run time)", o.SummaryFile)
		if o.Previous != "" {
			fmt.Fprintf(&b, ", after %s", o.Previous)
		}
		b.WriteString("\n")
		if o.RaisesKa {
			fmt.Fprintf(&b, "  raise requires_ka from %s to %s: older ka clients stop and ask to be upgraded\n", or(o.RequiresKaWas, "none"), o.RequiresKa)
		}
		fmt.Fprintf(&b, "  replace %s/%s with one root commit (a force push with a lease)\n", o.Remote, o.Branch)
		if o.BackupRefs > 0 {
			fmt.Fprintf(&b, "  delete %d replay backup ref(s) in this clone (refs/ka/backup/*, originals of earlier syncs)\n", o.BackupRefs)
		}
		fmt.Fprintf(&b, "Other clones need nothing: ka %s or newer adopts the new history on its next sync and replays unpushed work.\n", kaspec.CompactionMinKa)
		b.WriteString("Forks, other branches and clones made with plain git keep the old history; re-clone those.\n")
		fmt.Fprintf(&b, "This rewrites the shared history and cannot be undone on the remote. To run it: ka compact --store %s --yes [--summary-file F]\n", o.Store)
		return b.String()
	}
	fmt.Fprintf(&b, "compacted store %s: %d commits%s are now one root commit %s on %s/%s\n", o.Store, o.Commits, span, short(o.Root), o.Remote, o.Branch)
	fmt.Fprintf(&b, "summary: %s\n", o.SummaryFile)
	if o.RaisesKa {
		fmt.Fprintf(&b, "requires_ka raised from %s to %s\n", or(o.RequiresKaWas, "none"), o.RequiresKa)
	}
	if o.SizeAfter > 0 {
		fmt.Fprintf(&b, "clone objects: %s → %s\n", bytesHuman(o.SizeBefore), bytesHuman(o.SizeAfter))
	}
	for _, w := range o.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	b.WriteString("Other ka clients adopt the new history on their next sync.\n")
	return b.String()
}

// History renders a store's compaction summaries, one line each.
func History(h *engine.HistoryOut) string {
	if len(h.Summaries) == 0 {
		return "no history summaries: the history has not been compacted\n"
	}
	var b strings.Builder
	for _, e := range h.Summaries {
		fmt.Fprintf(&b, "%s %s: compacted %s by %s, %d commits%s", e.Store, e.Path, day(e.CompactedAt), e.By, e.Commits, dateSpan(e.FirstCommit, e.LastCommit))
		if e.Created+e.Closed > 0 {
			fmt.Fprintf(&b, ", %d tickets created, %d closed", e.Created, e.Closed)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// HistorySummary renders one summary with its narrative.
func HistorySummary(e *engine.HistoryEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\ncompacted %s by %s", e.Store, e.Path, e.CompactedAt, e.By)
	if e.OnBehalfOf != "" {
		fmt.Fprintf(&b, " for %s", e.OnBehalfOf)
	}
	fmt.Fprintf(&b, "\n%d commits%s, up to %s (tree %s)\n", e.Commits, dateSpan(e.FirstCommit, e.LastCommit), short(e.From), short(e.Tree))
	if len(e.Actors) > 0 {
		counts := map[string]int{}
		for k, v := range e.Actors {
			counts[k] = int(v)
		}
		fmt.Fprintf(&b, "by%s\n", strings.TrimPrefix(actorCounts(counts), ","))
	}
	if e.Created+e.Closed > 0 {
		fmt.Fprintf(&b, "tickets: %d created, %d closed\n", e.Created, e.Closed)
	}
	if e.Previous != "" {
		fmt.Fprintf(&b, "previous: %s\n", e.Previous)
	}
	if e.Body != "" {
		b.WriteString("\n" + e.Body + "\n")
	}
	return b.String()
}

func dateSpan(first, last string) string {
	switch {
	case first == "" && last == "":
		return ""
	case day(first) == day(last):
		return " on " + day(last)
	}
	return " from " + day(first) + " to " + day(last)
}

func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// actorCounts renders ", by a 12, b 3" (busiest first).
func actorCounts(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if m[names[i]] != m[names[j]] {
			return m[names[i]] > m[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s %d", n, m[n])
	}
	return ", by " + strings.Join(parts, ", ")
}

func bytesHuman(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
