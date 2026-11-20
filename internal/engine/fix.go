package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// Every writable store, locked in name order for the local work; the
	// pushes run after.
	var writable []*Store
	for _, st := range s.WS.Stores {
		if !st.ReadOnly {
			writable = append(writable, st)
		}
	}
	hm, err := s.hold(ctx, writable...)
	if err != nil {
		return nil, err
	}
	defer hm.unlock()
	s.ix.catchUp()
	out := &FixOut{}
	aliases := s.aliasMap()
	var slugProjects map[*Store]map[string]bool
	if o.Slugs {
		var err error
		if slugProjects, err = s.slugProjects(); err != nil {
			return nil, err
		}
	}
	var touched []*Store
	for _, st := range s.WS.Stores {
		if st.ReadOnly {
			continue
		}
		cats, committed, err := s.fixStore(ctx, c, st, aliases, slugProjects[st], o.DryRun)
		out.Categories = append(out.Categories, cats...)
		if committed {
			touched = append(touched, st)
		}
		if err != nil {
			if len(touched) > 0 {
				s.ix.refresh(nil, nil)
			}
			return out, err
		}
	}
	if len(touched) == 0 {
		return out, nil
	}
	s.ix.refresh(nil, nil)
	hm.unlock()
	for _, st := range touched {
		if _, err := s.push(ctx, st); err != nil {
			return out, Errf(CodeGit, "the fixes are committed locally; ka sync retries the push", "push %s: %v", st.Name, err)
		}
	}
	return out, nil
}

func itoaN(n int) string { return strconv.Itoa(n) }

// fixCats are ka fix's categories, in the order they are committed.
var fixCats = []string{"actors", "timestamps", "format", "slugs"}

// fixStore runs every fixer over one store in a single walk, parsing each
// file once, then commits each category that changed something.
func (s *Service) fixStore(ctx context.Context, c Caller, st *Store, aliases map[string]string, slugKeys map[string]bool, dryRun bool) (cats []FixCategory, committed bool, err error) {
	unlock, err := st.lock(ctx)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	if !dryRun {
		if err := st.guard(); err != nil {
			return nil, false, err
		}
	}
	type fixed struct {
		rel    string
		stages [][]byte // the file after each category; nil: unchanged by it
	}
	var files []fixed
	var added map[string]string // ticket.md -> when it was first committed
	addedAt := func(rel string) string {
		if added == nil {
			added = fileAddedTimes(ctx, st)
		}
		return added[rel]
	}
	walkErr := filepath.WalkDir(filepath.Join(st.Root, "projects"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(p, ".md") {
			return nil // links and other non-regular files are never followed
		}
		rel, _ := filepath.Rel(st.Root, p)
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, "/ticket.md") && !strings.Contains(rel, "/comments/") {
			return nil
		}
		data, err := readRegular(p)
		if err != nil {
			return nil // reported by ka check
		}
		if stages := fixFile(rel, data, aliases, slugKeys, func() string { return addedAt(rel) }, st.now); stages != nil {
			files = append(files, fixed{rel, stages})
		}
		return nil
	})
	if walkErr != nil {
		return nil, false, walkErr
	}
	for i, name := range fixCats {
		fc := FixCategory{Name: name}
		var paths []string
		for _, f := range files {
			if f.stages[i] == nil {
				continue
			}
			fc.Files = append(fc.Files, st.Name+"/"+f.rel)
			if dryRun {
				continue
			}
			paths = append(paths, f.rel)
			if err = os.WriteFile(filepath.Join(st.Root, filepath.FromSlash(f.rel)), f.stages[i], 0o644); err != nil {
				break
			}
		}
		if len(paths) > 0 {
			var sha string
			if err == nil {
				before := st.git.Head(ctx)
				if sha, err = st.commitPaths(ctx, c, fmt.Sprintf("ka fix: %s (%d files)", name, len(paths)), nil, paths); err == nil && sha != "" {
					st.own(before, sha)
				}
			}
			if err != nil {
				rp := make([]string, len(paths))
				for k, p := range paths {
					rp[k] = st.repoPath(p)
				}
				st.rollback(ctx, rp)
				return cats, committed, err
			}
			fc.Commit = short(sha)
			committed = committed || sha != ""
		}
		if len(fc.Files) > 0 {
			cats = append(cats, fc)
		}
	}
	return cats, committed, nil
}

// fixFile runs the fixers over one ticket.md or comment, in fixCats order,
// and returns the file after each (nil where a fixer changed nothing), or
// nil if nothing changes. A file that does not parse cleanly is left as it
// is, byte for byte: rewriting a partial parse would drop whatever failed
// to parse (B3). Missing timestamps and actor aliases are exceptions,
// because they are what the fixers repair.
func fixFile(rel string, raw []byte, aliases map[string]string, slugKeys map[string]bool, addedAt func() string, now func() time.Time) [][]byte {
	isTicket := strings.HasSuffix(rel, "/ticket.md")
	stages := make([][]byte, len(fixCats))
	cur, changed := raw, false
	next := func(i int, b []byte) {
		if !bytes.Equal(b, cur) {
			stages[i], cur, changed = b, b, true
		}
	}
	// actors: an alias ("Bryan Sant") is not a valid actor, so it is mapped
	// in the text, before parsing would drop it.
	next(0, fixActorLines(cur, isTicket, aliases))
	norm := kaspec.NormalizeEncoding(cur)
	if !isTicket {
		cm, ds := kaspec.ParseComment(rel, norm)
		if cm == nil || kaspec.HasErrors(ds, false) {
			return nil
		}
		next(2, kaspec.FormatComment(cm))
	} else {
		t, ds := kaspec.ParseTicket(rel, norm)
		if t == nil || slices.ContainsFunc(ds, func(d kaspec.Diagnostic) bool {
			return d.Severity == kaspec.SevError && !(d.Code == "missing_field" && (strings.Contains(d.Message, `"created"`) || strings.Contains(d.Message, `"updated"`)))
		}) {
			return nil
		}
		if t.Created == "" || t.Updated == "" {
			ts := ""
			if at := addedAt(); at != "" {
				ts, _ = parseISO(at)
			}
			if ts == "" {
				ts = now().UTC().Format("2006-01-02T15:04:05Z")
			}
			if t.Created == "" {
				t.Created = ts
			}
			if t.Updated == "" {
				t.Updated = t.Created
			}
			next(1, kaspec.FormatTicket(t))
		}
		next(2, kaspec.FormatTicket(t))
		parts := strings.Split(rel, "/") // projects/<key>/tickets/<ID>/ticket.md
		if len(parts) == 5 && slugKeys[parts[1]] && t.Slug == "" && t.MovedTo == "" {
			if t.Slug = kaspec.DeriveSlug(t.Title); t.Slug != "" {
				next(3, kaspec.FormatTicket(t))
			}
		}
	}
	if !changed {
		return nil
	}
	return stages
}

// fileAddedTimes maps each ticket.md under projects/ to the author date of
// the newest commit that added it, in one git log.
func fileAddedTimes(ctx context.Context, st *Store) map[string]string {
	out := map[string]string{}
	log, err := st.git.Run(ctx, "-c", "core.quotePath=false", "log", "--diff-filter=A", "--no-renames", "--name-only", "--format=%x00%aI", "--", st.repoPath("projects"))
	if err != nil {
		return out
	}
	at := ""
	for _, l := range strings.Split(log, "\n") {
		if d, ok := strings.CutPrefix(l, "\x00"); ok {
			at = d
			continue
		}
		if sp, ok := st.storePath(l); ok && strings.HasSuffix(sp, "/ticket.md") {
			if _, seen := out[sp]; !seen {
				out[sp] = at
			}
		}
	}
	return out
}

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
			return nil, Errf(CodeInvalid, requireHint(kaspec.SlugsMinKa)+", then run this again",
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

func parseISO(s string) (string, error) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return t.UTC().Format("2006-01-02T15:04:05Z"), nil
}

// aliasMap is the actor registry's lookup (actorKey -> handle).
func (s *Service) aliasMap() map[string]string {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	return maps.Clone(s.ix.actorBy)
}

// actorKeys are the frontmatter keys that hold actors: top-level ones, and
// claim's by (indented).
var (
	ticketActorKeys  = map[string]bool{"assignee": true, "owner": true, "created_by": true}
	commentActorKeys = map[string]bool{"author": true, "on_behalf_of": true, "to": true}
	actorLineRe      = regexp.MustCompile(`^([ \t]*)([a-z_]+):[ \t]*(.*?)[ \t]*(\r?)$`)
)

// fixActorLines maps actor values in a document's frontmatter through
// aliases, editing just those lines; everything else stays byte for byte.
func fixActorLines(data []byte, isTicket bool, aliases map[string]string) []byte {
	if len(aliases) == 0 {
		return data
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSuffix(strings.TrimPrefix(lines[0], "\uFEFF"), "\r") != "---" {
		return data
	}
	keys := commentActorKeys
	if isTicket {
		keys = ticketActorKeys
	}
	changed := false
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			break
		}
		m := actorLineRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		indent, key, v, cr := m[1], m[2], m[3], m[4]
		if !(indent == "" && keys[key]) && !(indent != "" && isTicket && key == "by") {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"') {
			if u, err := strconv.Unquote(v); err == nil {
				v = u
			}
		} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		}
		if h, ok := aliases[actorKey(v)]; ok && h != v {
			lines[i] = indent + key + ": " + h + cr
			changed = true
		}
	}
	if !changed {
		return data
	}
	return []byte(strings.Join(lines, "\n"))
}
