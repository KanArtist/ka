package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// FixOut reports what ka fix changed, per category.
type FixOut struct {
	Categories []FixCategory `json:"categories"`
}

// FixCategory is one kind of repair; each becomes one commit.
type FixCategory struct {
	Name   string   `json:"name"`
	Files  []string `json:"files"`
	Commit string   `json:"commit,omitempty"`
}

// Fix normalizes data in the writable stores (PRD §10.5): timestamps on
// hand-written tickets, actor aliases, and canonical formatting (encoding,
// key order, quoting). Each category is committed separately.
func (s *Service) Fix(ctx context.Context, c Caller, dryRun bool) (*FixOut, error) {
	return s.FixWith(ctx, c, FixOptions{DryRun: dryRun})
}

// FixOptions tunes FixWith.
type FixOptions struct {
	DryRun bool
	// Slugs gives every ticket without a slug one derived from its title,
	// in projects that set slugs: true (spec §4.3).
	Slugs bool
}

// FixWith is Fix with options.
func (s *Service) FixWith(ctx context.Context, c Caller, o FixOptions) (*FixOut, error) {
	dryRun := o.DryRun
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	out := &FixOut{}
	aliases := s.aliasMap()
	var slugProjects map[*Store]map[string]bool
	if o.Slugs {
		var err error
		if slugProjects, err = s.slugProjects(); err != nil {
			return nil, err
		}
	}
	for _, st := range s.WS.Stores {
		if st.ReadOnly {
			continue
		}
		unlock, err := st.lock(ctx)
		if err != nil {
			return nil, err
		}
		cats := []struct {
			name string
			fn   func(rel string, data []byte) []byte
		}{
			{"timestamps", func(rel string, data []byte) []byte { return fixTimestamps(ctx, st, rel, data) }},
			{"actors", func(rel string, data []byte) []byte { return fixActors(rel, data, aliases) }},
			{"format", fixFormat},
		}
		if keys := slugProjects[st]; len(keys) > 0 {
			cats = append(cats, struct {
				name string
				fn   func(rel string, data []byte) []byte
			}{"slugs", func(rel string, data []byte) []byte { return fixSlug(rel, data, keys) }})
		}
		for _, cat := range cats {
			fc := FixCategory{Name: cat.name}
			var paths []string
			filepath.WalkDir(filepath.Join(st.Root, "projects"), func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
					return nil
				}
				rel, _ := filepath.Rel(st.Root, p)
				rel = filepath.ToSlash(rel)
				if !strings.HasSuffix(rel, "/ticket.md") && !strings.Contains(rel, "/comments/") {
					return nil
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return nil
				}
				if nd := cat.fn(rel, data); nd != nil && string(nd) != string(data) {
					fc.Files = append(fc.Files, st.Name+"/"+rel)
					if !dryRun {
						os.WriteFile(p, nd, 0o644)
						paths = append(paths, st.repoPath(rel))
					}
				}
				return nil
			})
			if len(paths) > 0 {
				st.git.Run(ctx, append([]string{"add", "--"}, paths...)...)
				msg := "ka fix: " + cat.name + " (" + itoaN(len(paths)) + " files)\n\nKa-Actor: " + c.Actor + "\nKa-Spec: 1\n"
				if _, err := st.git.RunRaw(ctx, []byte(msg), "commit", "-q", "--no-verify", "-F", "-"); err == nil {
					fc.Commit = short(st.git.Head(ctx))
				}
			}
			if len(fc.Files) > 0 {
				out.Categories = append(out.Categories, fc)
			}
		}
		unlock()
	}
	if !dryRun {
		s.ix.refresh(nil, nil)
		for _, st := range s.WS.Stores {
			if !st.ReadOnly && !s.NoPush && st.PushMode != "manual" {
				st.Sync(ctx, s.hooks(st))
			}
		}
	}
	return out, nil
}

func itoaN(n int) string { return strconv.Itoa(n) }

// slugProjects finds, per writable store, the projects with slugs: true. It
// refuses when such a store does not require ka 1.2.0 or newer, since older
// clients cannot read tickets with slugs.
func (s *Service) slugProjects() (map[*Store]map[string]bool, error) {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	out := map[*Store]map[string]bool{}
	for key, pi := range s.ix.projects {
		if pi.Store.ReadOnly || pi.Cfg == nil || !pi.Cfg.Slugs {
			continue
		}
		if ks := s.ix.ks[pi.Store]; ks == nil || ks.File == nil || !kaspec.VersionAtLeastRelease(ks.File.RequiresKa, kaspec.SlugsMinKa) {
			return nil, Errf(CodeInvalid, "once every client of the store runs ka "+kaspec.SlugsMinKa+" or newer, add `requires_ka: "+kaspec.SlugsMinKa+"` to its .ka/store.yml, commit and push, then run this again",
				"store %s does not require ka %s, so clients older than that would fail to read the slugs", pi.Store.Name, kaspec.SlugsMinKa)
		}
		if out[pi.Store] == nil {
			out[pi.Store] = map[string]bool{}
		}
		out[pi.Store][key] = true
	}
	if len(out) == 0 {
		return nil, Errf(CodeInvalid, "set `slugs: true` in .ka/config.yml or a project's config first", "no project stores slugs")
	}
	return out, nil
}

// fixSlug gives a ticket in one of the given projects a slug derived from
// its title, if it has none.
func fixSlug(rel string, data []byte, keys map[string]bool) []byte {
	parts := strings.Split(rel, "/") // projects/<key>/tickets/<ID>/ticket.md
	if len(parts) != 5 || parts[0] != "projects" || parts[4] != "ticket.md" || !keys[parts[1]] {
		return nil
	}
	t, ds := kaspec.ParseTicket(rel, data)
	if t == nil || kaspec.HasErrors(ds, false) || t.Slug != "" || t.MovedTo != "" {
		return nil
	}
	if t.Slug = kaspec.DeriveSlug(t.Title); t.Slug == "" {
		return nil
	}
	return kaspec.FormatTicket(t)
}

func parseISO(s string) (string, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return t.UTC().Format("2006-01-02T15:04:05Z"), nil
}

func (s *Service) aliasMap() map[string]string {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	m := map[string]string{}
	for h, hu := range s.ix.actors.Humans {
		for _, a := range hu.Aliases {
			m[a] = h
			m[strings.ToLower(a)] = h
		}
		for _, e := range hu.Emails {
			m[e] = h
			m[strings.ToLower(e)] = h
		}
	}
	return m
}

func fixFormat(rel string, data []byte) []byte {
	norm := kaspec.NormalizeEncoding(data)
	if strings.HasSuffix(rel, "/ticket.md") {
		t, _ := kaspec.ParseTicket(rel, norm)
		if t == nil {
			return norm
		}
		return kaspec.FormatTicket(t)
	}
	c, _ := kaspec.ParseComment(rel, norm)
	if c == nil {
		return norm
	}
	return kaspec.FormatComment(c)
}

func fixTimestamps(ctx context.Context, st *Store, rel string, data []byte) []byte {
	if !strings.HasSuffix(rel, "/ticket.md") {
		return nil
	}
	t, _ := kaspec.ParseTicket(rel, kaspec.NormalizeEncoding(data))
	if t == nil || (t.Created != "" && t.Updated != "") {
		return nil
	}
	ts := ""
	if out, err := st.git.Run(ctx, "log", "--diff-filter=A", "--format=%aI", "-1", "--", st.repoPath(rel)); err == nil && out != "" {
		if tt, err := parseISO(out); err == nil {
			ts = tt
		}
	}
	if ts == "" {
		ts = st.now().UTC().Format("2006-01-02T15:04:05Z")
	}
	if t.Created == "" {
		t.Created = ts
	}
	if t.Updated == "" {
		t.Updated = t.Created
	}
	return kaspec.FormatTicket(t)
}

func fixActors(rel string, data []byte, aliases map[string]string) []byte {
	if len(aliases) == 0 {
		return nil
	}
	norm := kaspec.NormalizeEncoding(data)
	m := func(s string) string {
		if s == "" {
			return s
		}
		if h, ok := aliases[s]; ok {
			return h
		}
		if h, ok := aliases[strings.ToLower(s)]; ok {
			return h
		}
		return s
	}
	if strings.HasSuffix(rel, "/ticket.md") {
		t, _ := kaspec.ParseTicket(rel, norm)
		if t == nil {
			return nil
		}
		before := string(kaspec.FormatTicket(t))
		t.Assignee, t.Owner, t.CreatedBy = m(t.Assignee), m(t.Owner), m(t.CreatedBy)
		if t.Claim != nil {
			t.Claim.By = m(t.Claim.By)
		}
		if after := kaspec.FormatTicket(t); string(after) != before {
			return after
		}
		return nil
	}
	c, _ := kaspec.ParseComment(rel, norm)
	if c == nil {
		return nil
	}
	before := string(kaspec.FormatComment(c))
	c.Author, c.OnBehalfOf, c.To = m(c.Author), m(c.OnBehalfOf), m(c.To)
	if after := kaspec.FormatComment(c); string(after) != before {
		return after
	}
	return nil
}
