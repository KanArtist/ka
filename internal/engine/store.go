package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/KanArtist/ka/internal/gitx"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// Store is one store in the workspace: a directory inside a git working tree.
type Store struct {
	Name     string // workspace name (unique)
	Root     string // absolute store root
	URL      string // remote URL, when known
	Managed  bool   // ka owns the clone
	ReadOnly bool   // reached only through extends, or unsupported spec
	PushMode string // immediate (default), manual or debounce
	// PushDelay is the quiet period before a debounced push.
	PushDelay time.Duration
	// FetchInterval is how often a long-running process fetches (0: 15 s).
	FetchInterval time.Duration
	pushSet       bool // the workspace set push; store recommendations do not apply
	fetchSet      bool
	git           *gitx.Repo
	sub           string // store root relative to the repo toplevel, slash-separated, "" at the top
	gitDir        string

	// Simulated clock and backoff for tests.
	now   func() time.Time
	sleep func(time.Duration)

	// Network activity, for the Sync & health view.
	actMu     sync.Mutex
	lastFetch time.Time
	lastPush  time.Time
	lastErr   string
	lastErrAt time.Time
}

// note records the outcome of a fetch or push.
func (s *Store) note(fetched, pushed bool, err error) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	now := time.Now()
	if err != nil {
		s.lastErr, s.lastErrAt = AsError(err).Error(), now
		return
	}
	if fetched {
		s.lastFetch = now
	}
	if pushed {
		s.lastPush = now
	}
	if fetched || pushed {
		s.lastErr = ""
	}
}

// Activity reports when the store last fetched and pushed, and the last
// network error if it is newer than both.
func (s *Store) Activity() (lastFetch, lastPush time.Time, lastErr string) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.lastFetch, s.lastPush, s.lastErr
}

// SyncState summarizes a store's relation to its remote.
type SyncState struct {
	Store    string `json:"store"`
	Remote   string `json:"remote,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	State    string `json:"state"` // local (no remote), synced, ahead, behind, diverged, degraded, busy
	Detail   string `json:"detail,omitempty"`
	Head     string `json:"head,omitempty"`
	Attached bool   `json:"attached"`
}

// Result describes the outcome of a write.
type Result struct {
	Commit    string   `json:"commit,omitempty"`
	Pushed    bool     `json:"pushed"`
	HasRemote bool     `json:"has_remote"`
	Replayed  int      `json:"replayed,omitempty"`
	Preserved []string `json:"preserved,omitempty"`
	PushError string   `json:"push_error,omitempty"`
	// PushPending: committed locally; a debounced push follows shortly.
	PushPending bool `json:"push_pending,omitempty"`
	// Renamed maps IDs that collided with upstream during replay to their new IDs.
	Renamed map[string]string `json:"renamed,omitempty"`
}

func (r *Result) absorb(ir *integrateResult) {
	r.Replayed += ir.Replayed
	r.Preserved = append(r.Preserved, ir.Preserved...)
	for k, v := range ir.IDMap {
		if r.Renamed == nil {
			r.Renamed = map[string]string{}
		}
		r.Renamed[k] = v
	}
}

// OpenStore opens a store rooted at dir (which must be inside a git working tree).
func OpenStore(ctx context.Context, name, dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	top, err := gitx.Toplevel(ctx, abs)
	if err != nil {
		return nil, Errf(CodeGit, "run `git init` in the store, or point at a git checkout", "%s is not inside a git working tree", abs)
	}
	top, _ = filepath.EvalSymlinks(top)
	real, _ := filepath.EvalSymlinks(abs)
	sub, err := filepath.Rel(top, real)
	if err != nil {
		return nil, err
	}
	sub = filepath.ToSlash(sub)
	if sub == "." {
		sub = ""
	}
	g := &gitx.Repo{Dir: top}
	gd, err := g.GitDir(ctx)
	if err != nil {
		return nil, err
	}
	s := &Store{Name: name, Root: abs, git: g, sub: sub, gitDir: gd, PushMode: "immediate",
		now: time.Now, sleep: time.Sleep}
	return s, nil
}

// Git exposes the store's repository.
func (s *Store) Git() *gitx.Repo { return s.git }

func (s *Store) repoPath(rel string) string {
	if s.sub == "" {
		return rel
	}
	return s.sub + "/" + rel
}

func (s *Store) storePath(repoRel string) (string, bool) {
	if s.sub == "" {
		return repoRel, true
	}
	return strings.CutPrefix(repoRel, s.sub+"/")
}

// lock takes the per-clone write lock (flock on Unix, LockFileEx on Windows).
// LockTimeout bounds how long a write waits for another process's write to
// the same clone (which may include push retries over a slow network).
var LockTimeout = 2 * time.Minute

func (s *Store) lock(ctx context.Context) (func(), error) {
	fl := flock.New(filepath.Join(s.gitDir, "ka.lock"))
	// A blocking lock waits in the OS's queue, so a waiter cannot be starved
	// by other writers the way polling can.
	got := make(chan error, 1)
	go func() { got <- fl.Lock() }()
	timer := time.NewTimer(LockTimeout)
	defer timer.Stop()
	select {
	case err := <-got:
		if err != nil {
			return nil, Errf(CodeGitBusy, "check permissions on the .git directory", "could not lock %s: %v", s.Name, err)
		}
		return func() { fl.Unlock() }, nil
	case <-ctx.Done():
		go func() {
			if <-got == nil {
				fl.Unlock()
			}
		}()
		return nil, ctx.Err()
	case <-timer.C:
		go func() {
			if <-got == nil {
				fl.Unlock()
			}
		}()
		return nil, Errf(CodeGitBusy, "another ka process has been writing to this store for "+LockTimeout.String()+"; retry", "timed out waiting for the write lock on %s", s.Name)
	}
}

func (s *Store) upstream(ctx context.Context) (remote, branch string) {
	return s.git.Upstream(ctx)
}

// commitOp writes an operation's changes and commits them with trailers.
func (s *Store) commitOp(ctx context.Context, o *Op, changes []change, subject, author, date string) (string, error) {
	var paths []string
	for _, c := range changes {
		p := filepath.Join(s.Root, filepath.FromSlash(c.Path))
		if c.Data == nil {
			os.Remove(p)
		} else {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, c.Data, 0o644); err != nil {
				return "", err
			}
		}
		paths = append(paths, s.repoPath(c.Path))
	}
	if _, err := s.git.Run(ctx, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return "", err
	}
	msg := subject + "\n\n" + trailers(o)
	args := []string{"commit", "-q", "--no-verify", "-F", "-"}
	if author != "" {
		args = append(args, "--author", author)
	}
	g := s.git
	if date != "" {
		g = &gitx.Repo{Dir: s.git.Dir, Env: append(append([]string{}, s.git.Env...), "GIT_AUTHOR_DATE="+date)}
	}
	if _, err := g.RunRaw(ctx, []byte(msg), append(args, "--")...); err != nil {
		if strings.Contains(err.Error(), "Please tell me who you are") || strings.Contains(err.Error(), "user.email") {
			return "", Errf(CodeGit, "set an identity: git config --global user.name \"Your Name\" && git config --global user.email you@example.com", "git has no commit identity")
		}
		return "", err
	}
	return s.git.Head(ctx), nil
}

func trailers(o *Op) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ka-Op: %s\n", o.Encode())
	if o.Actor != "" {
		fmt.Fprintf(&b, "Ka-Actor: %s\n", o.Actor)
	}
	if o.OnBehalfOf != "" {
		fmt.Fprintf(&b, "Ka-On-Behalf-Of: %s\n", o.OnBehalfOf)
	}
	if o.Session != "" {
		fmt.Fprintf(&b, "Ka-Session: %s\n", o.Session)
	}
	b.WriteString("Ka-Spec: 1\n")
	return b.String()
}

func opFromCommit(c gitx.Commit) *Op {
	v, ok := c.Trailers["Ka-Op"]
	if !ok {
		return nil
	}
	o, err := DecodeOp(v)
	if err != nil {
		return nil
	}
	o.Actor = c.Trailers["Ka-Actor"]
	o.OnBehalfOf = c.Trailers["Ka-On-Behalf-Of"]
	o.Session = c.Trailers["Ka-Session"]
	return o
}

// hooks connect the store to the service's index during writes and replays.
type hooks struct {
	env      func() applyEnv // the apply environment for the current tree
	reloaded func([]string)  // store-relative paths changed in the working tree; nil means everything
}

// Do applies and commits one operation, then pushes it when the store has a
// remote and push is requested. If the remote moved, local commits are
// replayed on top of it, which re-checks this operation's preconditions; an
// operation that no longer applies (for example a claim someone else won)
// returns its error and leaves no commit behind.
func (s *Store) Do(ctx context.Context, h hooks, o *Op, push bool) (*Result, error) {
	if s.ReadOnly {
		return nil, Errf(CodeReadOnly, "this store is only reachable through extends; add it to your workspace to write to it", "store %s is read-only here", s.Name)
	}
	if err := s.checkVersion(); err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if p := s.git.InProgress(ctx); p != "" {
		return nil, Errf(CodeGitBusy, "finish or abort it (git "+p+" --abort), then retry", "a git %s is in progress in %s", p, s.git.Dir)
	}
	changes, subject, err := apply(h.env(), o)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	if len(changes) == 0 {
		return res, nil
	}
	sha, err := s.commitOp(ctx, o, changes, subject, "", "")
	if err != nil {
		return nil, err
	}
	res.Commit = sha
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	h.reloaded(paths)
	remote, _ := s.upstream(ctx)
	res.HasRemote = remote != ""
	if !push || remote == "" {
		return res, nil
	}
	if err := s.pushLocked(ctx, h, res, sha); err != nil {
		s.note(false, false, err)
		return res, err
	}
	s.note(true, true, nil)
	return res, nil
}

// pushLocked pushes, integrating upstream changes on rejection with bounded,
// jittered retries (PRD §10.1). mine is the commit of the caller's
// operation (empty for a plain sync); if its replay fails, the error is returned.
func (s *Store) pushLocked(ctx context.Context, h hooks, res *Result, mine string) error {
	remote, branch := s.upstream(ctx)
	if remote == "" {
		return nil
	}
	backoff := 50 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if s.git.Head(ctx) == s.remoteHead(ctx, remote, branch) && attempt > 0 {
			res.Pushed = true
			return nil
		}
		_, err := s.git.Run(ctx, "push", "-q", remote, "HEAD:refs/heads/"+branch)
		if err == nil {
			res.Pushed = true
			s.git.Run(ctx, "update-ref", "refs/remotes/"+remote+"/"+branch, "HEAD")
			return nil
		}
		lastErr = err
		if !gitx.IsNonFastForward(err) {
			// Offline or auth failure: the commit stays local and unconfirmed.
			res.PushError = firstLine(err.Error())
			return nil
		}
		ir, err := s.integrateLocked(ctx, h, remote, branch, mine)
		if ir != nil {
			res.absorb(ir)
			if mine != "" {
				if ir.MineErr != nil {
					res.Commit = ""
					return ir.MineErr
				}
				res.Commit = ir.MineSHA
				mine = ir.MineSHA
			}
		}
		if IsCode(err, CodeKaOutdated) {
			// The remote now needs a newer ka: keep the commit local,
			// like an offline push, and say why.
			res.PushError = AsError(err).Message + "; " + AsError(err).Fix
			return nil
		}
		if err != nil {
			return err
		}
		s.sleep(backoff + time.Duration(rand.Int64N(int64(backoff))))
		backoff *= 2
	}
	res.PushError = "push rejected 8 times: " + firstLine(lastErr.Error())
	return nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func (s *Store) remoteHead(ctx context.Context, remote, branch string) string {
	h, _ := s.git.Run(ctx, "rev-parse", "-q", "--verify", "refs/remotes/"+remote+"/"+branch)
	return h
}

// integrateResult reports what an integration did.
type integrateResult struct {
	Replayed  int
	Preserved []string
	MineSHA   string
	MineErr   error
	Moved     bool // HEAD changed to include upstream commits
	IDMap     map[string]string
}

// integrateLocked fetches and brings upstream commits under local ones.
// Local commits made by ka are replayed as operations; others are
// cherry-picked with the semantic merge driver, and anything that loses is
// preserved as a system comment (PRD §10.4).
func (s *Store) integrateLocked(ctx context.Context, h hooks, remote, branch, mine string) (*integrateResult, error) {
	if _, err := s.git.Run(ctx, "fetch", "-q", remote, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch); err != nil {
		return nil, Errf(CodeGit, "check the network and credentials, then run ka sync", "fetch failed: %s", firstLine(err.Error()))
	}
	up := "refs/remotes/" + remote + "/" + branch
	upSHA := s.remoteHead(ctx, remote, branch)
	head := s.git.Head(ctx)
	res := &integrateResult{MineSHA: mine}
	if upSHA == "" || upSHA == head {
		return res, nil
	}
	if err := s.checkUpstreamVersion(ctx, up); err != nil {
		return nil, err
	}
	if head == "" {
		if _, err := s.git.Run(ctx, "reset", "-q", "--keep", up); err != nil {
			return nil, err
		}
		res.Moved = true
		h.reloaded(nil)
		return res, nil
	}
	base, _ := s.git.Run(ctx, "merge-base", head, up)
	if base == "" {
		// No common commit: the remote is a different repository (renamed
		// or moved, and its old URL now names something else). Replaying
		// the store onto it would publish the store there, so stop.
		u, _ := s.git.Run(ctx, "remote", "get-url", remote)
		return nil, Errf(CodeGit, "check the store's remote: ka store set-url, or git remote set-url for an attached checkout",
			"%s shares no history with store %s; it is not this store's repository", strings.TrimSpace(u)+" "+branch, s.Name)
	}
	switch base {
	case upSHA: // we are ahead
		return res, nil
	case head: // fast-forward
		before := head
		if _, err := s.git.Run(ctx, "merge", "-q", "--ff-only", up); err != nil {
			return nil, Errf(CodeGit, "commit or stash local edits to the store, then run ka sync", "fast-forward failed: %s", firstLine(err.Error()))
		}
		res.Moved = true
		s.reloadRange(ctx, h, before, s.git.Head(ctx))
		return res, nil
	}

	// Diverged: replay local commits on top of upstream.
	local, err := s.git.Log(ctx, up+".."+head, true)
	if err != nil {
		return nil, err
	}
	s.git.Run(ctx, "update-ref", "refs/ka/backup/"+time.Now().UTC().Format("20060102T150405.000000000Z"), head)
	if _, err := s.git.Run(ctx, "reset", "-q", "--keep", up); err != nil {
		return nil, Errf(CodeGit, "commit or stash local edits to the store, then run ka sync", "cannot move to upstream: %s", firstLine(err.Error()))
	}
	res.Moved = true
	s.reloadRange(ctx, h, base, upSHA)
	idMap := map[string]string{}
	res.IDMap = idMap
	var failed []failedOp
	for _, c := range local {
		o := opFromCommit(c)
		if o == nil {
			pres, err := s.cherryPick(ctx, h, c)
			if err != nil {
				return res, err
			}
			res.Preserved = append(res.Preserved, pres...)
			res.Replayed++
			continue
		}
		remapOp(o, idMap)
		changes, subject, err := apply(h.env(), o)
		if IsCode(err, CodeIDTaken) && o.Op == "create" {
			// Someone published the same random ID first: take a new one.
			old := o.ID
			pfx, sfx, _ := kaspec.ParseID(old)
			o.ID = kaspec.NewID(pfx, len(sfx))
			idMap[old] = o.ID
			changes, subject, err = apply(h.env(), o)
		}
		if err != nil {
			if c.SHA == mine {
				res.MineErr = err
				res.MineSHA = ""
			} else {
				failed = append(failed, failedOp{o, c, err})
			}
			continue
		}
		if len(changes) == 0 {
			if c.SHA == mine {
				res.MineSHA = s.git.Head(ctx)
			}
			continue
		}
		sha, err := s.commitOp(ctx, o, changes, subject, c.Author, c.Date)
		if err != nil {
			return res, err
		}
		paths := make([]string, len(changes))
		for i, ch := range changes {
			paths[i] = ch.Path
		}
		h.reloaded(paths)
		res.Replayed++
		if c.SHA == mine {
			res.MineSHA = sha
		}
	}
	for _, f := range failed {
		if p := s.preserveFailed(ctx, h, f); p != "" {
			res.Preserved = append(res.Preserved, p)
		}
	}
	res.Preserved = append(res.Preserved, s.flushPreserved(ctx, h)...)
	return res, nil
}

type failedOp struct {
	op     *Op
	commit gitx.Commit
	err    error
}

// remapOp rewrites references to IDs that were re-assigned during replay.
func remapOp(o *Op, m map[string]string) {
	if len(m) == 0 {
		return
	}
	r := func(s string) string {
		if n, ok := m[s]; ok {
			return n
		}
		return s
	}
	o.ID = r(o.ID)
	if o.New != nil {
		o.New.Parent = r(o.New.Parent)
		for i := range o.New.BlockedBy {
			o.New.BlockedBy[i] = r(o.New.BlockedBy[i])
		}
		for i := range o.New.Related {
			o.New.Related[i] = r(o.New.Related[i])
		}
	}
	for k, v := range o.Set {
		if v != nil && (k == "parent" || k == "duplicate_of") {
			n := r(*v)
			o.Set[k] = &n
		}
	}
	for _, mm := range []map[string][]string{o.Add, o.Remove} {
		for k, vs := range mm {
			for i := range vs {
				vs[i] = r(vs[i])
			}
			mm[k] = vs
		}
	}
}

func (s *Store) reloadRange(ctx context.Context, h hooks, from, to string) {
	paths, err := s.git.ChangedPaths(ctx, from, to)
	if err != nil {
		h.reloaded(nil)
		return
	}
	var rel []string
	for _, p := range paths {
		if sp, ok := s.storePath(p); ok {
			rel = append(rel, sp)
		}
	}
	h.reloaded(rel)
}

// preserveFailed records an operation that could not be replayed as a system
// comment on its ticket, so the intent is never silently dropped.
func (s *Store) preserveFailed(ctx context.Context, h hooks, f failedOp) string {
	b, _ := json.MarshalIndent(f.op, "", "  ")
	body := fmt.Sprintf("An operation by %s could not be applied after syncing and was not published.\n\n"+
		"- Reason: %s\n- Original commit: %s (%s)\n- To apply it now: ka restore %s <this comment's file name>, or Restore in Sync & health\n\n```json\n%s\n```\n",
		orDefault(f.op.Actor, "an unknown actor"), AsError(f.err).Error(), f.commit.SHA[:12], f.commit.Subject, f.op.ID, b)
	return s.writeSystemComment(ctx, h, f.op.ID, body, f.op.Actor)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// SystemActor authors comments written by ka itself.
const SystemActor = "agent/ka"

func (s *Store) writeSystemComment(ctx context.Context, h hooks, id, body, onBehalf string) string {
	now := s.now().UTC().Format("2006-01-02T15:04:05Z")
	name := kaspec.CommentFilename(now, kaspec.RandomBase32(4))
	o := &Op{Op: "comment", ID: id, At: now, Actor: SystemActor, CommentName: name,
		Comment: &kaspec.Comment{Type: "system", Author: SystemActor, Created: now, Body: body}}
	if onBehalf != "" && kaspec.HumanRe.MatchString(onBehalf) {
		o.Comment.OnBehalfOf = onBehalf
	}
	changes, subject, err := apply(h.env(), o)
	if err != nil || len(changes) == 0 {
		return ""
	}
	if _, err := s.commitOp(ctx, o, changes, subject, "", ""); err != nil {
		return ""
	}
	h.reloaded([]string{changes[0].Path})
	return changes[0].Path
}

// cherryPick applies a commit that ka did not make (a hand edit). The merge
// driver resolves ticket.md field by field; anything else that conflicts
// takes the upstream side and the local version is preserved.
func (s *Store) cherryPick(ctx context.Context, h hooks, c gitx.Commit) ([]string, error) {
	before := s.git.Head(ctx)
	_, err := s.git.Run(ctx, "cherry-pick", "--allow-empty", "--keep-redundant-commits", c.SHA)
	var preserved []string
	if err != nil {
		conflicted, _ := s.git.Run(ctx, "diff", "--name-only", "--diff-filter=U")
		s.git.Run(ctx, "cherry-pick", "--abort")
		if _, err := s.git.Run(ctx, "cherry-pick", "--allow-empty", "--keep-redundant-commits", "-X", "ours", c.SHA); err != nil {
			s.git.Run(ctx, "cherry-pick", "--abort")
			return nil, Errf(CodeGit, "resolve by hand: the original is at refs/ka/backup/*", "cannot replay %s: %s", c.SHA[:12], firstLine(err.Error()))
		}
		s.reloadRange(ctx, h, before, s.git.Head(ctx))
		for _, p := range strings.Split(conflicted, "\n") {
			if p == "" {
				continue
			}
			content, _ := s.git.Run(ctx, "show", c.SHA+":"+p)
			sp, _ := s.storePath(p)
			if id := ticketIDFromPath(sp); id != "" {
				body := fmt.Sprintf("A local edit to `%s` conflicted with upstream and lost. Upstream was kept; the local version from %s (%s, by %s) is below.\n\n````\n%s\n````\n",
					sp, c.SHA[:12], c.Subject, c.Author, content)
				if q := s.writeSystemComment(ctx, h, id, body, ""); q != "" {
					preserved = append(preserved, q)
				}
			}
		}
	} else {
		s.reloadRange(ctx, h, before, s.git.Head(ctx))
	}
	preserved = append(preserved, s.flushPreserved(ctx, h)...)
	return preserved, nil
}

// ticketIDFromPath extracts the ticket ID from projects/<key>/tickets/<ID>/...
func ticketIDFromPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "tickets" {
		return parts[3]
	}
	return ""
}

// Preserved is a value that lost a semantic merge, recorded by the merge
// driver (which cannot write commits itself) and turned into a system comment
// by the next ka write or sync.
type Preserved struct {
	Path   string `json:"path"`
	Field  string `json:"field"`
	Lost   string `json:"lost"`
	Kept   string `json:"kept"`
	Commit string `json:"commit,omitempty"`
}

func (s *Store) flushPreserved(ctx context.Context, h hooks) []string {
	dir := filepath.Join(s.gitDir, "ka-preserved")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	byTicket := map[string][]Preserved{}
	var files []string
	for _, e := range ents {
		f := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		files = append(files, f)
		var ps []Preserved
		if json.Unmarshal(b, &ps) != nil {
			continue
		}
		for _, p := range ps {
			sp, _ := s.storePath(p.Path)
			if id := ticketIDFromPath(sp); id != "" {
				byTicket[id] = append(byTicket[id], p)
			}
		}
	}
	ids := make([]string, 0, len(byTicket))
	for id := range byTicket {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		var b strings.Builder
		b.WriteString("Concurrent edits conflicted during a merge. The published value was kept; the other value is preserved here.\n\n")
		for _, p := range byTicket[id] {
			fmt.Fprintf(&b, "- **%s**: kept `%s`, lost:\n\n````\n%s\n````\n\n", p.Field, oneLine(p.Kept), p.Lost)
		}
		if q := s.writeSystemComment(ctx, h, id, b.String(), ""); q != "" {
			out = append(out, q)
		}
	}
	for _, f := range files {
		os.Remove(f)
	}
	return out
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// Sync fetches, integrates and pushes (ka sync).
func (s *Store) Sync(ctx context.Context, h hooks) (*Result, error) { return s.sync(ctx, h, true) }

// FetchOnly brings in upstream changes (replaying local commits on top) but
// does not push: for manual and debounced stores in the fetch loop.
func (s *Store) FetchOnly(ctx context.Context, h hooks) (*Result, error) {
	return s.sync(ctx, h, false)
}

func (s *Store) sync(ctx context.Context, h hooks, push bool) (*Result, error) {
	res := &Result{}
	remote, branch := s.upstream(ctx)
	res.HasRemote = remote != ""
	if remote == "" {
		return res, nil
	}
	if s.ReadOnly {
		_, err := s.integrateReadOnly(ctx, h, remote, branch)
		return res, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if p := s.git.InProgress(ctx); p != "" {
		return nil, Errf(CodeGitBusy, "finish or abort it (git "+p+" --abort), then retry", "a git %s is in progress in %s", p, s.git.Dir)
	}
	ir, err := s.integrateLocked(ctx, h, remote, branch, "")
	if err != nil {
		s.note(false, false, err)
		return res, err
	}
	s.note(true, false, nil)
	res.absorb(ir)
	res.Preserved = append(res.Preserved, s.flushPreserved(ctx, h)...)
	if !push {
		res.Pushed = s.git.Head(ctx) == s.remoteHead(ctx, remote, branch)
		return res, nil
	}
	if s.git.Head(ctx) != s.remoteHead(ctx, remote, branch) {
		if err := s.pushLocked(ctx, h, res, ""); err != nil {
			s.note(false, false, err)
			return res, err
		}
		s.note(false, true, nil)
	} else {
		res.Pushed = true
	}
	return res, nil
}

// integrateReadOnly fast-forwards a store this workspace only reads.
func (s *Store) integrateReadOnly(ctx context.Context, h hooks, remote, branch string) (bool, error) {
	if !s.Managed {
		return false, nil // never move someone's attached checkout we do not write to
	}
	before := s.git.Head(ctx)
	if _, err := s.git.Run(ctx, "fetch", "-q", remote, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch); err != nil {
		return false, nil
	}
	if err := s.checkUpstreamVersion(ctx, "refs/remotes/"+remote+"/"+branch); err != nil {
		return false, err
	}
	if _, err := s.git.Run(ctx, "merge", "-q", "--ff-only", "refs/remotes/"+remote+"/"+branch); err != nil {
		return false, nil
	}
	after := s.git.Head(ctx)
	if after != before {
		s.reloadRange(ctx, h, before, after)
	}
	return after != before, nil
}

// State reports sync status without touching the network.
func (s *Store) State(ctx context.Context) SyncState {
	st := SyncState{Store: s.Name, Attached: !s.Managed, Head: short(s.git.Head(ctx))}
	if p := s.git.InProgress(ctx); p != "" {
		st.State, st.Detail = "busy", "git "+p+" in progress"
		return st
	}
	remote, branch := s.upstream(ctx)
	if remote == "" {
		st.State = "local"
		return st
	}
	st.Remote, st.Branch = remote, branch
	up := "refs/remotes/" + remote + "/" + branch
	out, err := s.git.Run(ctx, "rev-list", "--left-right", "--count", "HEAD..."+up)
	if err == nil {
		fmt.Sscanf(out, "%d\t%d", &st.Ahead, &st.Behind)
	}
	_, _, lastErr := s.Activity()
	switch {
	case lastErr != "":
		// The last fetch or push failed: work continues locally (PRD §10.1).
		st.State, st.Detail = "degraded", lastErr
	case st.Ahead == 0 && st.Behind == 0:
		st.State = "synced"
	case st.Behind == 0:
		st.State = "ahead"
	case st.Ahead == 0:
		st.State = "behind"
	default:
		st.State = "diverged"
	}
	return st
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
