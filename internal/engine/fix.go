package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	out := &FixOut{}
	aliases := s.aliasMap()
	for _, st := range s.WS.Stores {
		if st.ReadOnly {
			continue
		}
		unlock, err := st.lock(ctx)
		if err != nil {
			return nil, err
		}
		for _, cat := range []struct {
			name string
			fn   func(rel string, data []byte) []byte
		}{
			{"timestamps", func(rel string, data []byte) []byte { return fixTimestamps(ctx, st, rel, data) }},
			{"actors", func(rel string, data []byte) []byte { return fixActors(rel, data, aliases) }},
			{"format", fixFormat},
		} {
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
