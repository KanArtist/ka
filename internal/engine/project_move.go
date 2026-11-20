package engine

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/KanArtist/ka/internal/gitx"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// MoveProjectOut reports a project move.
type MoveProjectOut struct {
	Project    string   `json:"project"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Files      int      `json:"files"`
	Tickets    int      `json:"tickets"`
	ToCommit   string   `json:"to_commit,omitempty"`
	FromCommit string   `json:"from_commit,omitempty"`
	Pushed     []string `json:"pushed,omitempty" doc:"Stores whose commit was pushed."`
	PushError  string   `json:"push_error,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
}

// moveAttempts bounds how often a move or rename is redone on top of an
// upstream that moved while it was being pushed.
const moveAttempts = 3

// MoveProject moves a project to another store in the workspace (PRD §9.2):
// it copies projects/<key>/ into the target with one commit, then replaces
// it in the source with a project.yml tombstone (moved_to_store) in a
// second commit. Ticket IDs and paths do not change, so references from
// other stores still resolve. The target is pushed first; the source is
// pushed only once the target has the project.
//
// Both stores are locked and synced before anything is read, so the copy is
// complete. If a push is rejected because the remote moved, the commits not
// yet pushed are undone and the move is redone on top of upstream.
func (s *Service) MoveProject(ctx context.Context, c Caller, key, to string, dryRun bool) (*MoveProjectOut, error) {
	s.ix.catchUp()
	src, dst, prefix, err := s.moveStores(key, to)
	if err != nil {
		return nil, err
	}
	// Fetch both before taking any lock; the first attempt integrates that.
	var pre map[*Store]*fetchedUp
	if !dryRun {
		pre = s.prefetch(ctx, []*Store{src, dst})
	}
	hm, err := s.hold(ctx, src, dst)
	if err != nil {
		return nil, err
	}
	defer hm.unlock()
	s.ix.catchUp()
	rel := "projects/" + key
	out := &MoveProjectOut{Project: key, From: src.Name, To: dst.Name, DryRun: dryRun}
	if err := checkMoveTarget(dst, rel); err != nil {
		return nil, err
	}
	if dryRun {
		files, err := regularFiles(src.Root, rel)
		if err != nil {
			return nil, err
		}
		out.Files, out.Tickets = len(files), countTickets(files)
		return out, nil
	}

	unlock, err := lockStores(ctx, []*Store{src, dst})
	if err != nil {
		return nil, err
	}
	defer unlock()
	order := []*Store{dst, src} // the copy is published before the tombstone
	copied := false             // an earlier attempt published the copy
	for attempt := 1; ; attempt++ {
		// Start from the latest of both stores, and check again on it.
		if err := s.syncHeld(ctx, order, pre); err != nil {
			return nil, err
		}
		pre = nil // later attempts fetch again, under the locks
		if attempt == 1 {
			if src, dst, prefix, err = s.moveStores(key, to); err != nil {
				return nil, err
			}
			if err := checkMoveTarget(dst, rel); err != nil {
				return nil, err
			}
		}
		files, err := regularFiles(src.Root, rel)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 || isTombstoneDir(src.Root, rel) {
			return nil, Errf(CodeUnknownProject, "", "store %s has no project %s", src.Name, key)
		}
		out.Files, out.Tickets = len(files), countTickets(files)

		jd, js := newJournal(ctx, dst), newJournal(ctx, src)
		undo := func(jj ...*journal) error {
			var errs []error
			for _, j := range jj {
				errs = append(errs, j.undo(ctx))
			}
			s.ix.refresh(nil, nil)
			return errors.Join(errs...)
		}
		toSHA, fromSHA, err := s.moveOnce(ctx, c, key, prefix, rel, files, jd, js, out.Tickets)
		if err != nil {
			return nil, errors.Join(err, undo(jd, js))
		}
		if toSHA != "" {
			out.ToCommit = short(toSHA)
		}
		if fromSHA != "" {
			out.FromCommit = short(fromSHA)
		}
		pushed, rejected, why := s.pushChain(ctx, order)
		out.Pushed = pushed
		if rejected == nil {
			out.PushError = why
			return out, nil
		}
		if rejected == dst {
			// Take back this attempt in both stores (the source is not
			// pushed before the target).
			if err := undo(jd, js); err != nil {
				return nil, err
			}
			if attempt < moveAttempts {
				continue
			}
			if copied {
				return nil, Errf(CodeGit, "remove "+rel+" from "+dst.Name+" (ka check reports the project in both stores until then), then move it again",
					"the remote of %s kept changing: an incomplete copy of %s is published there, and the source was left as it was", dst.Name, key)
			}
			return nil, Errf(CodeGit, "retry the move once the stores are quieter", "the remote of %s kept changing while the move was pushed; nothing was moved", dst.Name)
		}
		copied = true
		// The copy is published; the tombstone lost a race in the source.
		if attempt == moveAttempts {
			out.PushError = src.Name + ": the remote kept changing; the tombstone is committed locally and the next ka sync publishes it"
			return out, nil
		}
		if err := undo(js); err != nil {
			return nil, err
		}
	}
}

// moveStores resolves a move's source and target from the index.
func (s *Service) moveStores(key, to string) (src, dst *Store, prefix string, err error) {
	s.ix.mu.RLock()
	pi := s.ix.projects[key]
	if pi != nil {
		src, prefix = pi.Store, pi.Prefix
	}
	s.ix.mu.RUnlock()
	if pi == nil {
		return nil, nil, "", Errf(CodeUnknownProject, "projects: "+strings.Join(s.Projects(), ", "), "no project %q", key)
	}
	for _, st := range s.WS.Stores {
		if st.Name == to || (st.URL != "" && sameURL(st.URL, to)) {
			dst = st
		}
	}
	switch {
	case dst == nil:
		var names []string
		for _, st := range s.WS.Stores {
			names = append(names, st.Name)
		}
		return nil, nil, "", Errf(CodeInvalid, "stores: "+strings.Join(names, ", ")+" (add the target with ka store add)", "no store %q in the workspace", to)
	case dst == src:
		return nil, nil, "", Errf(CodeInvalid, "", "project %s already lives in %s", key, src.Name)
	case src.ReadOnly || dst.ReadOnly:
		return nil, nil, "", Errf(CodeReadOnly, "add both stores to your workspace to write to them", "store %s is read-only here", map[bool]string{true: src.Name, false: dst.Name}[src.ReadOnly])
	}
	return src, dst, prefix, nil
}

// checkMoveTarget refuses a target that already has the project (anything
// but a tombstone of it).
func checkMoveTarget(dst *Store, rel string) error {
	if _, err := os.Lstat(filepath.Join(dst.Root, filepath.FromSlash(rel))); err == nil && !isTombstoneDir(dst.Root, rel) {
		return Errf(CodeConflict, "remove or rename it first", "%s already has %s", dst.Name, rel)
	}
	return nil
}

// isTombstoneDir reports whether rel holds only a moved_to_store project.yml.
func isTombstoneDir(root, rel string) bool {
	files, err := regularFiles(root, rel)
	if err != nil || len(files) != 1 || files[0] != "project.yml" {
		return false
	}
	data, err := readRegular(filepath.Join(root, filepath.FromSlash(rel), "project.yml"))
	if err != nil {
		return false
	}
	pf, _ := kaspec.ParseProjectFile("project.yml", data)
	return pf != nil && pf.MovedToStore != ""
}

func countTickets(files []string) int {
	n := 0
	for _, f := range files {
		if parts := strings.Split(f, "/"); len(parts) == 3 && parts[0] == "tickets" && parts[2] == "ticket.md" {
			n++
		}
	}
	return n
}

// moveOnce makes a move's two commits from the current state of the stores:
// the target mirrors the source's project directory, and the source keeps a
// tombstone. It is idempotent, so a redo after a lost push race commits only
// what changed upstream meanwhile (or nothing).
func (s *Service) moveOnce(ctx context.Context, c Caller, key, prefix, rel string, files []string, jd, js *journal, tickets int) (toSHA, fromSHA string, err error) {
	src, dst := js.st, jd.st
	want := map[string]bool{}
	for _, f := range files {
		p := rel + "/" + f
		want[p] = true
		data, err := readRegular(js.abs(p))
		if err != nil {
			return "", "", err
		}
		if cur, err := readRegular(jd.abs(p)); err == nil && bytes.Equal(cur, data) {
			continue
		}
		if err := jd.write(p, data); err != nil {
			return "", "", err
		}
	}
	old, err := regularFiles(dst.Root, rel)
	if err != nil {
		return "", "", err
	}
	for _, f := range old {
		if p := rel + "/" + f; !want[p] {
			if err := jd.remove(p); err != nil {
				return "", "", err
			}
		}
	}
	if toSHA, err = jd.commit(ctx, c, "ka: move project "+key+" from "+src.Name+" ("+strconv.Itoa(tickets)+" tickets)",
		[]string{"Ka-Moved-From: " + storeRef(src)}, []string{rel}); err != nil {
		return "", "", err
	}

	for _, f := range files {
		if f != "project.yml" {
			if err := js.remove(rel + "/" + f); err != nil {
				return "", "", err
			}
		}
	}
	tomb := kaspec.EmitYAML([]kaspec.KV{{K: "key", V: key}, {K: "prefix", V: prefix}, {K: "moved_to_store", V: storeRef(dst)}})
	if err := js.write(rel+"/project.yml", []byte(tomb)); err != nil {
		return "", "", err
	}
	if fromSHA, err = js.commit(ctx, c, "ka: move project "+key+" to "+dst.Name,
		[]string{"Ka-Moved-To: " + storeRef(dst)}, []string{rel}); err != nil {
		return "", "", err
	}
	s.ix.refresh(nil, nil)
	return toSHA, fromSHA, nil
}

// storeRef names a store portably: its remote URL, else its name.
func storeRef(st *Store) string {
	if st.URL != "" {
		return st.URL
	}
	return st.Name
}

// ---- shared by project move and rename -----------------------------------------

// mayPush reports whether st may be pushed now (D2): not under --no-push,
// not read-only, and not push: manual. Debounced stores may be pushed;
// callers that defer their pushes check s.debounced as well.
func (s *Service) mayPush(st *Store) bool {
	return !s.NoPush && !st.ReadOnly && st.PushMode != "manual"
}

// lockStores takes the write locks of several stores, in name order so two
// multi-store writes cannot deadlock, and runs each store's write guards.
func lockStores(ctx context.Context, stores []*Store) (unlock func(), err error) {
	sorted := append([]*Store(nil), stores...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var unlocks []func()
	unlock = func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
		unlocks = nil
	}
	for _, st := range sorted {
		u, err := st.lock(ctx)
		if err != nil {
			unlock()
			return nil, err
		}
		unlocks = append(unlocks, u)
		if err := st.guard(); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

// syncHeld brings each store level with its remote (fetch, then replay local
// commits on top) without pushing, with the stores' write locks held by the
// caller. A multi-store rewrite starts from it, so nothing that already
// reached a remote is missed. Under --no-push nothing is fetched. pre holds
// fetches made before the locks were taken (prefetch), integrated instead of
// fetching again; a store without one is fetched with the locks held.
func (s *Service) syncHeld(ctx context.Context, stores []*Store, pre map[*Store]*fetchedUp) error {
	if s.NoPush {
		return nil
	}
	for _, st := range stores {
		remote, branch := st.upstream(ctx)
		if remote == "" {
			continue
		}
		res, err := st.syncLocked(ctx, s.hooks(st), &Result{HasRemote: true}, remote, branch, pre[st])
		if err != nil {
			return err
		}
		s.syncEvents(st, res)
	}
	return nil
}

// prefetch fetches stores without any lock, for a multi-store rewrite to
// integrate once it holds the locks (syncHeld). A failed fetch is not
// recorded: the locked sync then fetches again and reports it.
func (s *Service) prefetch(ctx context.Context, stores []*Store) map[*Store]*fetchedUp {
	if s.NoPush {
		return nil
	}
	pre := map[*Store]*fetchedUp{}
	for _, st := range stores {
		if remote, branch := st.upstream(ctx); remote != "" && !st.ReadOnly {
			f := &fetchedUp{}
			if f.prev, f.up, f.err = st.fetchUp(ctx, remote, branch); f.err == nil {
				pre[st] = f
			}
		}
	}
	return pre
}

// pushChain pushes stores in order, each only once the one before it has
// reached its remote, so a later store never publishes something that
// depends on an earlier one left behind: a move's tombstone without its copy,
// a rename's references without the renamed tickets. Nothing is replayed: a
// push the remote rejects because it moved returns that store as rejected,
// for the caller to redo its change on top of upstream. Any other store not
// pushed (push: manual, no remote, --no-push, an offline or refused push)
// stops the chain, and why says which stores were held back.
func (s *Service) pushChain(ctx context.Context, stores []*Store) (pushed []string, rejected *Store, why string) {
	for i, st := range stores {
		remote, branch := st.upstream(ctx)
		reason := ""
		switch {
		case s.NoPush:
			reason = "not pushed (--no-push)"
		case !s.mayPush(st):
			reason = "not pushed (push: manual)"
		case remote == "":
			reason = "not pushed (no remote)"
		default:
			_, err := st.net(ctx, "push", "-q", remote, "HEAD:refs/heads/"+branch)
			if err == nil {
				st.git.Run(ctx, "update-ref", "refs/remotes/"+remote+"/"+branch, "HEAD")
				st.note(false, true, nil)
				s.syncEvents(st, &Result{HasRemote: true, Pushed: true, Commit: st.git.Head(ctx)})
				pushed = append(pushed, st.Name)
				continue
			}
			if gitx.IsNonFastForward(err) {
				return pushed, st, ""
			}
			st.note(false, false, err)
			reason = firstLine(err.Error())
		}
		why = st.Name + ": " + reason
		if rest := stores[i+1:]; len(rest) > 0 {
			names := make([]string, len(rest))
			for j, r := range rest {
				names[j] = r.Name
			}
			why += "; " + strings.Join(names, ", ") + " held back until it is pushed: push " + st.Name + " first (ka sync), then the rest"
		}
		return pushed, nil, why
	}
	return pushed, nil, ""
}

// journal records the files a multi-file rewrite changes in one store, so a
// failure can put everything back (B17): each file as it was, and the
// branch and those index entries as of head. Files and commits the user made
// elsewhere in the checkout are not touched.
type journal struct {
	st    *Store
	head  string
	orig  map[string][]byte // store-relative path -> original bytes; nil: absent
	paths []string
}

func newJournal(ctx context.Context, st *Store) *journal {
	return &journal{st: st, head: st.git.Head(ctx), orig: map[string][]byte{}}
}

func (j *journal) abs(rel string) string { return filepath.Join(j.st.Root, filepath.FromSlash(rel)) }

// keep remembers rel's content before its first change.
func (j *journal) keep(rel string) error {
	if _, ok := j.orig[rel]; ok {
		return nil
	}
	data, err := readRegular(j.abs(rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = nil
	case err != nil:
		return err
	case data == nil:
		data = []byte{}
	}
	j.orig[rel] = data
	j.paths = append(j.paths, rel)
	return nil
}

func (j *journal) write(rel string, data []byte) error {
	if err := j.keep(rel); err != nil {
		return err
	}
	p := j.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (j *journal) remove(rel string) error {
	if err := j.keep(rel); err != nil {
		return err
	}
	if err := os.Remove(j.abs(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	pruneEmpty(j.st.Root, path.Dir(rel))
	return nil
}

// commit commits paths (store-relative) with the standard trailers. It
// records the HEAD move as this process's own, for the index.
func (j *journal) commit(ctx context.Context, c Caller, subject string, extra, paths []string) (string, error) {
	before := j.st.git.Head(ctx)
	sha, err := j.st.commitPaths(ctx, c, subject, extra, paths)
	if err == nil && sha != "" {
		j.st.own(before, sha)
	}
	return sha, err
}

// undo restores every file the journal changed and resets the branch, and
// those files' index entries, to where they were.
func (j *journal) undo(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	g := j.st.git
	var errs []error
	if j.head != "" && g.Head(ctx) != j.head {
		if _, err := g.Run(ctx, "reset", "-q", "--soft", j.head); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rel := range j.paths {
		p := j.abs(rel)
		if data := j.orig[rel]; data == nil {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			pruneEmpty(j.st.Root, path.Dir(rel))
		} else if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			errs = append(errs, err)
		} else if err := os.WriteFile(p, data, 0o644); err != nil {
			errs = append(errs, err)
		}
	}
	if len(j.paths) > 0 && j.head != "" {
		rp := make([]string, len(j.paths))
		for i, p := range j.paths {
			rp[i] = j.st.repoPath(p)
		}
		if _, err := g.Run(ctx, append([]string{"--literal-pathspecs", "reset", "-q", j.head, "--"}, rp...)...); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Errf(CodeGit, "check the store with git status; the original files are in "+short(j.head), "could not undo the change in %s: %v", j.st.Name, err)
	}
	return nil
}

// pruneEmpty removes rel and its parents while they are empty directories,
// stopping at the store root.
func pruneEmpty(root, rel string) {
	for rel != "." && rel != "" && rel != "/" {
		if os.Remove(filepath.Join(root, filepath.FromSlash(rel))) != nil {
			return
		}
		rel = path.Dir(rel)
	}
}

// readRegular reads a regular file without following a symbolic link (or a
// Windows junction) to somewhere else.
func readRegular(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, irregular(p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi2, err := f.Stat(); err != nil {
		return nil, err
	} else if !os.SameFile(fi, fi2) {
		return nil, irregular(p) // swapped since the Lstat
	}
	var b bytes.Buffer
	if _, err := b.ReadFrom(f); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func irregular(p string) error {
	return Errf(CodeInvalid, "replace it with a regular file (ka never follows links out of a store)", "%s is not a regular file", p)
}

// regularFiles lists the files under root/rel (relative to it,
// slash-separated, sorted). An absent directory has none. Anything that is
// neither a directory nor a regular file (a symbolic link, a junction, a
// device) is an error: copying or rewriting it would follow it.
func regularFiles(root, rel string) ([]string, error) {
	dir := filepath.Join(root, filepath.FromSlash(rel))
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return irregular(p)
		}
		r, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(r))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
