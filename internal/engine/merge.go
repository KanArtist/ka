package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/JumpKickOSS/ka/internal/gitx"
	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// MergeTickets merges two versions of a ticket.md against their common
// ancestor, field by field (PRD §10.3). up is the already published side;
// when both sides changed a scalar differently, up wins and the other value
// is returned in preserved. ok is false when a version does not parse, in
// which case git's text merge should be used instead.
func MergeTickets(ctx context.Context, base, up, local []byte) (merged []byte, preserved []Preserved, ok bool) {
	pb, ds1 := kaspec.ParseTicket("base", base)
	pu, ds2 := kaspec.ParseTicket("up", up)
	pl, ds3 := kaspec.ParseTicket("local", local)
	if pu == nil || pl == nil || kaspec.HasErrors(ds2, false) || kaspec.HasErrors(ds3, false) {
		return nil, nil, false
	}
	if pb == nil || kaspec.HasErrors(ds1, false) {
		pb = &kaspec.Ticket{}
	}
	out := *pu
	lose := func(field, kept, lost string) {
		preserved = append(preserved, Preserved{Field: field, Kept: kept, Lost: lost})
	}
	scalar := func(field string, b, u, l string, dst *string) {
		switch {
		case u == l:
			*dst = u
		case l == b:
			*dst = u
		case u == b:
			*dst = l
		default:
			*dst = u
			lose(field, u, l)
		}
	}
	scalar("title", pb.Title, pu.Title, pl.Title, &out.Title)
	scalar("status", pb.Status, pu.Status, pl.Status, &out.Status)
	scalar("priority", pb.Priority, pu.Priority, pl.Priority, &out.Priority)
	scalar("kind", pb.Kind, pu.Kind, pl.Kind, &out.Kind)
	scalar("assignee", pb.Assignee, pu.Assignee, pl.Assignee, &out.Assignee)
	scalar("owner", pb.Owner, pu.Owner, pl.Owner, &out.Owner)
	scalar("parent", pb.Parent, pu.Parent, pl.Parent, &out.Parent)
	scalar("duplicate_of", pb.DuplicateOf, pu.DuplicateOf, pl.DuplicateOf, &out.DuplicateOf)
	scalar("milestone", pb.Milestone, pu.Milestone, pl.Milestone, &out.Milestone)
	scalar("due", pb.Due, pu.Due, pl.Due, &out.Due)
	scalar("moved_to", pb.MovedTo, pu.MovedTo, pl.MovedTo, &out.MovedTo)
	scalar("created_by", pb.CreatedBy, pu.CreatedBy, pl.CreatedBy, &out.CreatedBy)
	scalar("created", pb.Created, pu.Created, pl.Created, &out.Created)

	claimStr := func(c *kaspec.Claim) string {
		if c == nil {
			return ""
		}
		return c.By + "|" + c.Session + "|" + c.At
	}
	var cs string
	scalar("claim", claimStr(pb.Claim), claimStr(pu.Claim), claimStr(pl.Claim), &cs)
	switch cs {
	case claimStr(pu.Claim):
		out.Claim = pu.Claim
	case claimStr(pl.Claim):
		out.Claim = pl.Claim
	}

	out.Labels = mergeSet(pb.Labels, pu.Labels, pl.Labels)
	out.BlockedBy = mergeSet(pb.BlockedBy, pu.BlockedBy, pl.BlockedBy)
	out.Related = mergeSet(pb.Related, pu.Related, pl.Related)
	out.Links = mergeSet(pb.Links, pu.Links, pl.Links)
	out.Acceptance = mergeAcceptance(pb.Acceptance, pu.Acceptance, pl.Acceptance)

	// Custom fields: per key, like scalars.
	keys := map[string]bool{}
	for _, m := range []map[string]kaspec.FieldValue{pb.Fields, pu.Fields, pl.Fields} {
		for k := range m {
			keys[k] = true
		}
	}
	fv := func(m map[string]kaspec.FieldValue, k string) string {
		v, ok := m[k]
		if !ok {
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	out.Fields = map[string]kaspec.FieldValue{}
	for _, k := range sortedKeys(keys) {
		var res string
		scalar("fields."+k, fv(pb.Fields, k), fv(pu.Fields, k), fv(pl.Fields, k), &res)
		if res == "" {
			continue
		}
		var v kaspec.FieldValue
		json.Unmarshal([]byte(res), &v)
		out.Fields[k] = v
	}
	if len(out.Fields) == 0 {
		out.Fields = nil
	}

	out.Updated = max(pu.Updated, pl.Updated)

	// Body: git's text merge; if it conflicts, upstream wins.
	switch {
	case pu.Body == pl.Body || pl.Body == pb.Body:
		out.Body = pu.Body
	case pu.Body == pb.Body:
		out.Body = pl.Body
	default:
		if m, clean := mergeText(ctx, pb.Body, pu.Body, pl.Body); clean {
			out.Body = m
		} else {
			out.Body = pu.Body
			lose("description", "(the published description)", pl.Body)
		}
	}
	return kaspec.FormatTicket(&out), preserved, true
}

func mergeSet(b, u, l []string) []string {
	var out []string
	add := func(v string) {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	for _, v := range u {
		if slices.Contains(b, v) && !slices.Contains(l, v) {
			continue // removed locally
		}
		add(v)
	}
	for _, v := range l {
		if slices.Contains(b, v) && !slices.Contains(u, v) {
			continue // removed upstream
		}
		add(v)
	}
	return out
}

func mergeAcceptance(b, u, l []kaspec.AcceptanceItem) []kaspec.AcceptanceItem {
	find := func(list []kaspec.AcceptanceItem, text string) int {
		return slices.IndexFunc(list, func(a kaspec.AcceptanceItem) bool { return a.Text == text })
	}
	var out []kaspec.AcceptanceItem
	for _, it := range u {
		bi, li := find(b, it.Text), find(l, it.Text)
		if bi >= 0 && li < 0 {
			continue // removed locally
		}
		if li >= 0 && l[li].Done {
			it.Done = true
		}
		out = append(out, it)
	}
	for _, it := range l {
		if find(out, it.Text) >= 0 {
			continue
		}
		if find(b, it.Text) >= 0 {
			continue // removed upstream
		}
		out = append(out, it)
	}
	return out
}

func mergeText(ctx context.Context, base, up, local string) (string, bool) {
	dir, err := os.MkdirTemp("", "ka-merge-")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(dir)
	files := []string{filepath.Join(dir, "up"), filepath.Join(dir, "base"), filepath.Join(dir, "local")}
	for i, s := range []string{up, base, local} {
		os.WriteFile(files[i], []byte(s+"\n"), 0o644)
	}
	g := &gitx.Repo{Dir: dir}
	out, err := g.RunRaw(ctx, nil, "merge-file", "-p", files[0], files[1], files[2])
	if err != nil {
		return "", false
	}
	return string(out), true
}

// RunMergeDriver implements `ka merge-driver %O %A %B %P`, registered with
// git config merge.ka.driver. It writes the result to %A and returns the
// exit code git expects (0 clean, 1 conflict).
func RunMergeDriver(ctx context.Context, base, ours, theirs, path string) int {
	b, _ := os.ReadFile(base)
	a, err1 := os.ReadFile(ours)
	t, err2 := os.ReadFile(theirs)
	if err1 != nil || err2 != nil {
		return 1
	}
	cwd, _ := os.Getwd()
	g := &gitx.Repo{Dir: cwd}
	// During a rebase or cherry-pick, "ours" is the branch being built on
	// (upstream); during a merge, "theirs" is what was fetched.
	upIsOurs := true
	if p := g.InProgress(ctx); p == "merge" {
		upIsOurs = false
	}
	up, local := a, t
	if !upIsOurs {
		up, local = t, a
	}
	merged, preserved, ok := MergeTickets(ctx, b, up, local)
	if !ok {
		return mergeFileFallback(ctx, g, ours, base, theirs)
	}
	if err := os.WriteFile(ours, merged, 0o644); err != nil {
		return 1
	}
	if len(preserved) > 0 {
		gd, err := g.GitDir(ctx)
		if err == nil {
			commit := ""
			if h, err := g.Run(ctx, "rev-parse", "-q", "--verify", "CHERRY_PICK_HEAD"); err == nil {
				commit = h
			} else if h, err := g.Run(ctx, "rev-parse", "-q", "--verify", "REBASE_HEAD"); err == nil {
				commit = h
			} else if h, err := g.Run(ctx, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
				commit = h
			}
			for i := range preserved {
				preserved[i].Path = path
				preserved[i].Commit = commit
			}
			dir := filepath.Join(gd, "ka-preserved")
			os.MkdirAll(dir, 0o755)
			data, _ := json.MarshalIndent(preserved, "", "  ")
			name := fmt.Sprintf("%d-%s.json", time.Now().UnixNano(), kaspec.RandomBase32(4))
			os.WriteFile(filepath.Join(dir, name), data, 0o644)
		}
	}
	return 0
}

func mergeFileFallback(ctx context.Context, g *gitx.Repo, ours, base, theirs string) int {
	if _, err := g.RunRaw(ctx, nil, "merge-file", ours, base, theirs); err != nil {
		return 1
	}
	return 0
}
