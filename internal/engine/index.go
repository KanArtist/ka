package engine

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// Rec is one indexed ticket.
type Rec struct {
	ID       string
	Store    *Store
	Project  *ProjectInfo
	Dir      string // relative to the store root
	T        *kaspec.Ticket
	Raw      []byte
	Comments []*kaspec.CommentEntry
	Invalid  bool   // ticket.md has errors; T may be partial
	text     string // lower-cased title and body, for full-text search
}

// ProjectInfo is one project in the workspace.
type ProjectInfo struct {
	Key      string
	Prefix   string
	Name     string
	Store    *Store
	Dir      string
	File     *kaspec.ProjectFile
	Cfg      *kaspec.Config
	CfgDiags []kaspec.Diagnostic
}

// Index is the in-memory model of a workspace. It is disposable: it is
// rebuilt from the files at startup and updated from changed paths.
type Index struct {
	mu       sync.RWMutex
	stores   []*Store
	ks       map[*Store]*kaspec.Store
	kws      *kaspec.Workspace
	tickets  map[string]*Rec
	projects map[string]*ProjectInfo // by key
	prefixes map[string]*ProjectInfo // by ID prefix
	actors   *kaspec.Actors
	children map[string][]string
	blocks   map[string][]string
	diags    []kaspec.Diagnostic
	heads    map[*Store]*headStamp

	// emit, when set, receives the tickets each refresh changed (with the
	// write lock held), who caused it if known, and stores fully rebuilt.
	emit func(changes []recChange, actor string, rebuilt []*Store)
}

// headStamp remembers the HEAD the index reflects, and the reflog's size and
// time, so a cheap stat detects commits made by other processes.
type headStamp struct {
	head  string
	size  int64
	mtime time.Time
}

func newIndex(stores []*Store) *Index {
	ix := &Index{stores: stores, heads: map[*Store]*headStamp{}}
	for _, s := range stores {
		ix.heads[s] = ix.stamp(s, nil)
	}
	ix.rebuild()
	return ix
}

func (ix *Index) stamp(s *Store, prev *headStamp) *headStamp {
	st := &headStamp{}
	if fi, err := os.Stat(filepath.Join(s.gitDir, "logs", "HEAD")); err == nil {
		st.size, st.mtime = fi.Size(), fi.ModTime()
		if prev != nil && prev.size == st.size && prev.mtime.Equal(st.mtime) {
			st.head = prev.head
			return st
		}
	}
	st.head = s.git.Head(context.Background())
	return st
}

// catchUp reloads whatever changed in the stores since the index last
// looked, including commits made by other processes sharing a clone.
func (ix *Index) catchUp() {
	for _, s := range ix.stores {
		ix.mu.RLock()
		prev := ix.heads[s]
		ix.mu.RUnlock()
		cur := ix.stamp(s, prev)
		if prev != nil && cur.head == prev.head {
			ix.mu.Lock()
			ix.heads[s] = cur
			ix.mu.Unlock()
			continue
		}
		var paths []string
		if prev != nil && prev.head != "" && cur.head != "" {
			changed, err := s.git.ChangedPaths(context.Background(), prev.head, cur.head)
			if err == nil {
				for _, p := range changed {
					if sp, ok := s.storePath(p); ok {
						paths = append(paths, sp)
					}
				}
			} else {
				paths = nil
			}
			if err == nil && len(paths) == 0 {
				paths = []string{}
			}
		}
		if paths == nil || len(paths) > 0 {
			actor := ""
			if ix.emit != nil && prev != nil && prev.head != "" && cur.head != "" {
				actor = ix.commitsActor(s, prev.head, cur.head)
			}
			ix.refreshBy(s, paths, actor)
		}
		ix.mu.Lock()
		ix.heads[s] = cur
		ix.mu.Unlock()
	}
}

// commitsActor names who made the commits in old..new when it is one actor:
// the Ka-Actor trailer, else the git author's name.
func (ix *Index) commitsActor(s *Store, old, new string) string {
	cs, err := s.git.Log(context.Background(), old+".."+new, false)
	if err != nil || len(cs) == 0 {
		return ""
	}
	actor := ""
	for _, c := range cs {
		a := c.Trailers["Ka-Actor"]
		if a == "" {
			a = ix.authorActor(c.Author)
		}
		if actor != "" && a != actor {
			return ""
		}
		actor = a
	}
	return actor
}

func (ix *Index) resolver(from *kaspec.Store, p kaspec.Parent) (fs.FS, string, string, error) {
	var base string
	for s, k := range ix.ks {
		if k == from {
			base = s.Root
		}
	}
	if st := findParent(ix.stores, base, p); st != nil {
		return os.DirFS(st.Root), st.Root, st.Name, nil
	}
	return nil, "", "", fmt.Errorf("no local copy of %s (add it to the workspace)", p.URL)
}

// findParent locates the workspace store an extends entry names.
func findParent(stores []*Store, base string, p kaspec.Parent) *Store {
	if dir, ok := localParentPath(base, p.URL); ok {
		for _, s := range stores {
			if samePath(s.Root, dir) {
				return s
			}
		}
		if isStoreDir(dir) {
			return nil
		}
		// A local path that is not a store checkout (a bare repository, say)
		// is a remote URL like any other.
	}
	for _, s := range stores {
		if s.URL != "" && sameURL(s.URL, p.URL) {
			return s
		}
	}
	return nil
}

// isStoreDir reports whether dir is a store checkout.
func isStoreDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".ka", "store.yml"))
	return err == nil
}

func localParentPath(base, url string) (string, bool) {
	switch {
	case strings.HasPrefix(url, "file://"):
		return filepath.FromSlash(strings.TrimPrefix(url, "file://")), true
	case strings.HasPrefix(url, ".") || filepath.IsAbs(url) || strings.HasPrefix(url, "/"):
		if filepath.IsAbs(url) {
			return filepath.Clean(url), true
		}
		return filepath.Clean(filepath.Join(base, filepath.FromSlash(url))), true
	}
	return "", false
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil {
		return ra == rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func sameURL(a, b string) bool {
	n := func(s string) string { return strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git") }
	return n(a) == n(b)
}

// rebuild reloads everything from disk.
func (ix *Index) rebuild() {
	ix.ks = map[*Store]*kaspec.Store{}
	ix.kws = kaspec.NewWorkspace(ix.resolver)
	type res struct {
		s  *Store
		ks *kaspec.Store
	}
	out := make([]res, len(ix.stores))
	var wg sync.WaitGroup
	for i, s := range ix.stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = res{s, kaspec.LoadStore(os.DirFS(s.Root), s.Name)}
		}()
	}
	wg.Wait()
	for _, r := range out {
		ix.ks[r.s] = r.ks
		ix.kws.AddLoaded(r.ks, r.s.Root)
	}
	ix.tickets = map[string]*Rec{}
	ix.projects = map[string]*ProjectInfo{}
	ix.prefixes = map[string]*ProjectInfo{}
	ix.actors = &kaspec.Actors{Humans: map[string]kaspec.Human{}, Agents: map[string]string{}, Roles: map[string][]string{}}
	ix.diags = nil
	for _, s := range ix.stores {
		ks := ix.ks[s]
		ix.diags = append(ix.diags, ks.Diags...)
		if a := ks.Actors; a != nil {
			for k, v := range a.Humans {
				if _, ok := ix.actors.Humans[k]; !ok {
					ix.actors.Humans[k] = v
				}
			}
			for k, v := range a.Agents {
				if _, ok := ix.actors.Agents[k]; !ok {
					ix.actors.Agents[k] = v
				}
			}
			for k, v := range a.Roles {
				ix.actors.Roles[k] = append(ix.actors.Roles[k], v...)
			}
		}
		for _, key := range ks.Keys {
			pr := ks.Projects[key]
			if _, dup := ix.projects[key]; dup {
				continue // reported by ka check (duplicate_project)
			}
			cfg, cds := kaspec.ProjectConfig(ix.kws, ks, pr)
			pi := &ProjectInfo{Key: key, Store: s, Dir: pr.Dir, File: pr.File, Cfg: cfg, CfgDiags: cds}
			pi.Prefix = pr.File.Prefix
			if pi.Prefix == "" {
				pi.Prefix = kaspec.DefaultPrefix(key)
			}
			pi.Name = pr.File.Name
			ix.projects[key] = pi
			if _, dup := ix.prefixes[pi.Prefix]; !dup {
				ix.prefixes[pi.Prefix] = pi
			}
			for _, id := range pr.IDs {
				ix.put(pi, pr.Tickets[id])
			}
		}
	}
	// Former prefixes (after a rename) find the project too, unless an
	// active prefix took them over.
	for _, pi := range ix.projects {
		for _, fp := range pi.File.FormerPrefixes {
			if _, taken := ix.prefixes[fp]; !taken {
				ix.prefixes[fp] = pi
			}
		}
	}
	ix.derive()
}

// canon normalizes a ticket ID: upper case, any slug dropped
// (JK-7QK3M-compiler-warnings is JK-7QK3M), and a former project prefix
// replaced by the current one (KA-7QK3M is KAN-7QK3M after a rename).
func (ix *Index) canon(id string) string {
	id = kaspec.RefID(id)
	if _, ok := ix.tickets[id]; ok {
		return id
	}
	pfx, sfx, ok := kaspec.ParseID(id)
	if !ok {
		return id
	}
	if pi, ok := ix.prefixes[pfx]; ok && pi.Prefix != pfx {
		return pi.Prefix + "-" + sfx
	}
	return id
}

// Ref is the ticket's reference: ID-slug, with a slug derived from the
// title when none is stored (spec §4.3).
func (r *Rec) Ref() string { return kaspec.Ref(r.ID, kaspec.DisplaySlug(r.T)) }

func (r *Rec) ref() Ref { return Ref{ID: r.ID, Ref: r.Ref(), Title: r.T.Title, Status: r.T.Status} }

// refOf returns the reference of a ticket by ID (the ID if unknown).
func (ix *Index) refOf(id string) string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if r, ok := ix.tickets[id]; ok {
		return r.Ref()
	}
	return id
}

func (ix *Index) put(pi *ProjectInfo, te *kaspec.TicketEntry) {
	id := filepath.Base(te.Dir)
	r := &Rec{ID: id, Store: pi.Store, Project: pi, Dir: te.Dir, T: te.Ticket, Raw: te.Raw, Comments: te.Comments}
	if r.T == nil {
		r.T = &kaspec.Ticket{ID: id}
		r.Invalid = true
	}
	r.text = strings.ToLower(r.T.Title + "\n" + r.T.Body)
	ix.tickets[id] = r
}

// derive recomputes inverse relations.
func (ix *Index) derive() {
	ix.children = map[string][]string{}
	ix.blocks = map[string][]string{}
	for id, r := range ix.tickets {
		if r.T.Parent != "" {
			ix.children[r.T.Parent] = append(ix.children[r.T.Parent], id)
		}
		for _, b := range r.T.BlockedBy {
			ix.blocks[b] = append(ix.blocks[b], id)
		}
	}
	for _, m := range []map[string][]string{ix.children, ix.blocks} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
}

// refresh reloads the given store-relative paths of one store; nil reloads
// everything. Config, project and actor changes trigger a full rebuild.
func (ix *Index) refresh(s *Store, paths []string) { ix.refreshBy(s, paths, "") }

// refreshBy is refresh with the actor who caused the change, for events.
func (ix *Index) refreshBy(s *Store, paths []string, actor string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.emit == nil {
		ix.reload(s, paths)
		return
	}
	before := ix.tickets
	touched, full := ix.reload(s, paths)
	var changes []recChange
	var rebuilt []*Store
	if full {
		if s != nil {
			rebuilt = []*Store{s}
		} else {
			rebuilt = ix.stores
		}
		ids := make([]string, 0, len(ix.tickets))
		for id, r := range ix.tickets {
			if o := before[id]; o == nil || !sameRec(o, r) {
				ids = append(ids, id)
			}
		}
		for id := range before {
			if _, ok := ix.tickets[id]; !ok {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			changes = append(changes, recChange{id: id, old: before[id], new: ix.tickets[id]})
		}
	} else {
		for _, tc := range touched {
			if n := ix.tickets[tc.id]; tc.old != n && (tc.old == nil || n == nil || !sameRec(tc.old, n)) {
				changes = append(changes, recChange{id: tc.id, old: tc.old, new: n})
			}
		}
	}
	if len(changes) > 0 || len(rebuilt) > 0 {
		ix.emit(changes, actor, rebuilt)
	}
}

func sameRec(a, b *Rec) bool {
	if !bytes.Equal(a.Raw, b.Raw) || len(a.Comments) != len(b.Comments) {
		return false
	}
	for i := range a.Comments {
		if a.Comments[i].Name != b.Comments[i].Name {
			return false
		}
	}
	return true
}

// reload does the work of refresh. It returns the tickets it reloaded with
// their previous records, or full when it rebuilt everything.
func (ix *Index) reload(s *Store, paths []string) (touched []recChange, full bool) {
	if paths == nil {
		ix.rebuild()
		return nil, true
	}
	dirs := map[string]bool{}
	for _, p := range paths {
		if strings.HasPrefix(p, ".ka/") || strings.HasSuffix(p, "/project.yml") || strings.HasPrefix(p, "boards/") {
			ix.rebuild()
			return nil, true
		}
		parts := strings.Split(p, "/")
		if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "tickets" {
			dirs[strings.Join(parts[:4], "/")] = true
		}
	}
	ks := ix.ks[s]
	for dir := range dirs {
		// Drop the ticket's old parse diagnostics; LoadTicket returns fresh ones.
		pfx := dir + "/"
		if ks.Label != "" {
			pfx = ks.Label + "/" + pfx
		}
		ks.Diags = slices.DeleteFunc(ks.Diags, func(d kaspec.Diagnostic) bool {
			return strings.HasPrefix(d.Path, pfx) || d.Path == strings.TrimSuffix(pfx, "/")
		})
		parts := strings.Split(dir, "/")
		pi := ix.projects[parts[1]]
		id := parts[3]
		if pi == nil || pi.Store != s {
			ix.rebuild()
			return nil, true
		}
		touched = append(touched, recChange{id: id, old: ix.tickets[id]})
		if _, err := os.Stat(filepath.Join(s.Root, filepath.FromSlash(dir), "ticket.md")); err != nil {
			delete(ix.tickets, id)
			if kp := ks.Projects[pi.Key]; kp != nil {
				delete(kp.Tickets, id)
			}
			continue
		}
		te, tds := ks.LoadTicket(dir)
		ks.Diags = append(ks.Diags, tds...)
		if te == nil {
			continue
		}
		if kp := ks.Projects[pi.Key]; kp != nil {
			if _, ok := kp.Tickets[id]; !ok {
				kp.IDs = append(kp.IDs, id)
				slices.Sort(kp.IDs)
			}
			kp.Tickets[id] = te
		}
		ix.put(pi, te)
	}
	ix.derive()
	return touched, false
}

// projectFor maps a ticket ID to its project.
// movedTo returns where a project tombstoned in this workspace now lives.
func (ix *Index) movedTo(key string) string {
	for _, ks := range ix.ks {
		if to := ks.Moved[key]; to != "" {
			return to
		}
	}
	return ""
}

func (ix *Index) projectFor(id string) (*ProjectInfo, bool) {
	pfx, _, ok := kaspec.ParseID(id)
	if !ok {
		return nil, false
	}
	pi, ok := ix.prefixes[pfx]
	return pi, ok
}

// ixEnv adapts the index to applyEnv for one store.
type ixEnv struct {
	ix *Index
	s  *Store
}

func (e ixEnv) root() string { return e.s.Root }

func (e ixEnv) project(id string) (string, *kaspec.Config, error) {
	e.ix.mu.RLock()
	defer e.ix.mu.RUnlock()
	pi, ok := e.ix.projectFor(id)
	if !ok {
		return "", nil, Errf(CodeUnknownProject, "check the ID prefix; ka_schema lists projects", "no project has the prefix of %s", id)
	}
	if pi.Store != e.s {
		return "", nil, Errf(CodeInvalid, "", "%s belongs to store %s, not %s", id, pi.Store.Name, e.s.Name)
	}
	return pi.Dir, pi.Cfg, nil
}

func (e ixEnv) ref(id string) (bool, bool) {
	e.ix.mu.RLock()
	defer e.ix.mu.RUnlock()
	if _, ok := e.ix.tickets[id]; ok {
		return true, true
	}
	_, known := e.ix.projectFor(id)
	return false, known
}

// Workspace returns the kaspec workspace for validation (ka check).
func (ix *Index) Workspace() *kaspec.Workspace { return ix.kws }

// authorActor maps a git author ("Name <email>") written without a Ka-Actor
// trailer to an actor handle through the registry's emails and aliases, so a
// hand-made commit by "Bryan Sant" shows as bsant. Unknown authors keep their
// name.
func (ix *Index) authorActor(author string) string {
	name, email, _ := strings.Cut(author, " <")
	email = strings.TrimSuffix(email, ">")
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.actors == nil {
		return name
	}
	for _, h := range sortedKeys(ix.actors.Humans) {
		hu := ix.actors.Humans[h]
		if strings.EqualFold(h, name) {
			return h
		}
		for _, e := range hu.Emails {
			if email != "" && strings.EqualFold(e, email) {
				return h
			}
		}
		for _, a := range hu.Aliases {
			if strings.EqualFold(a, name) {
				return h
			}
		}
	}
	return name
}
