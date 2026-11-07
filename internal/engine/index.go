package engine

import (
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

	"github.com/JumpKickOSS/ka/pkg/kaspec"
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
			ix.refresh(s, paths)
		}
		ix.mu.Lock()
		ix.heads[s] = cur
		ix.mu.Unlock()
	}
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
		return nil
	}
	for _, s := range stores {
		if s.URL != "" && sameURL(s.URL, p.URL) {
			return s
		}
	}
	return nil
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
	ix.derive()
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
func (ix *Index) refresh(s *Store, paths []string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if paths == nil {
		ix.rebuild()
		return
	}
	dirs := map[string]bool{}
	for _, p := range paths {
		if strings.HasPrefix(p, ".ka/") || strings.HasSuffix(p, "/project.yml") || strings.HasPrefix(p, "boards/") {
			ix.rebuild()
			return
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
			return
		}
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
}

// projectFor maps a ticket ID to its project.
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
