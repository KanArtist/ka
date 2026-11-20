package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
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

	// HEAD moves this process made and already reloaded into the index
	// (ownFrom to ownTo), so catchUp need not diff and re-parse them.
	ownMu          sync.Mutex
	ownFrom, ownTo string
}

// own records that this process moved HEAD from before to after and has
// reloaded the changed paths.
func (s *Store) own(before, after string) {
	if before == after {
		return
	}
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if s.ownTo != before || s.ownFrom == "" {
		s.ownFrom = before
	}
	s.ownTo = after
}

// ownedSince reports whether every HEAD move from prev to cur was this
// process's own, and starts a new chain at cur.
func (s *Store) ownedSince(prev, cur string) bool {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if prev == "" || s.ownFrom != prev || s.ownTo != cur {
		return false
	}
	s.ownFrom = cur
	return true
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
	// Unpreserved describes local changes that lost during replay and could
	// not be recorded as a comment (their ticket is gone, say). The commits
	// are kept under refs/ka/backup/.
	Unpreserved []string `json:"unpreserved,omitempty"`
	// Renamed maps IDs that collided with upstream during replay to their new IDs.
	Renamed map[string]string `json:"renamed,omitempty"`
}

func (r *Result) absorb(ir *integrateResult) {
	r.Replayed += ir.Replayed
	r.Preserved = append(r.Preserved, ir.Preserved...)
	r.Unpreserved = append(r.Unpreserved, ir.Unpreserved...)
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

// inProgress reports a rebase, merge or cherry-pick left in the clone.
func (s *Store) inProgress() string { return gitx.InProgressAt(s.gitDir) }

func (s *Store) busyErr(p string) error {
	return Errf(CodeGitBusy, "finish or abort it (git "+p+" --abort), then retry", "a git %s is in progress in %s", p, s.git.Dir)
}

// Network timeouts. A stalled connection would otherwise hold the write
// lock (and the service's write mutex) until TCP gives up.
var (
	NetTimeout   = 3 * time.Minute  // one fetch or push
	CloneTimeout = 30 * time.Minute // a first clone
)

// net runs a git command that talks to the remote, bounded by NetTimeout.
func (s *Store) net(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, NetTimeout)
	defer cancel()
	return s.git.Run(ctx, args...)
}

func (s *Store) fetch(ctx context.Context, remote, branch string) error {
	_, err := s.net(ctx, "fetch", "-q", remote, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch)
	return err
}

// commitOp writes an operation's changes and commits them with trailers.
func (s *Store) commitOp(ctx context.Context, o *Op, changes []change, subject, author, date string) (string, error) {
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = s.repoPath(c.Path)
	}
	// What is on disk now, to put back if the write or the commit fails: in
	// an attached checkout that may include hand edits not yet committed,
	// which a checkout of HEAD would discard.
	before := make([][]byte, len(changes))
	for i, c := range changes {
		before[i], _ = os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(c.Path)))
	}
	undo := func() {
		s.git.Run(context.WithoutCancel(ctx), append([]string{"--literal-pathspecs", "reset", "-q", "--"}, paths...)...)
		for i, c := range changes {
			p := filepath.Join(s.Root, filepath.FromSlash(c.Path))
			if before[i] == nil {
				os.Remove(p)
			} else if os.MkdirAll(filepath.Dir(p), 0o755) == nil {
				os.WriteFile(p, before[i], 0o644)
			}
		}
	}
	if err := s.writeChanges(changes); err != nil {
		undo()
		return "", err
	}
	sha, parent, err := s.commit(ctx, subject+"\n\n"+trailers(o), author, date, paths)
	if err != nil {
		undo()
		return "", err
	}
	// Every caller reloads the changed paths into the index next.
	s.own(parent, sha)
	return sha, nil
}

// writeChanges writes (or, with nil data, removes) files under the store root.
func (s *Store) writeChanges(changes []change) error {
	for _, c := range changes {
		p := filepath.Join(s.Root, filepath.FromSlash(c.Path))
		if c.Data == nil {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, c.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// stage stages exactly paths (repo-relative files or directories):
// additions, edits and deletions, nothing else.
func (s *Store) stage(ctx context.Context, paths []string) error {
	var present, gone []string
	for _, p := range paths {
		if _, err := os.Lstat(filepath.Join(s.git.Dir, filepath.FromSlash(p))); err == nil {
			present = append(present, p)
		} else {
			gone = append(gone, p)
		}
	}
	if len(present) > 0 {
		if _, err := s.git.Run(ctx, append([]string{"--literal-pathspecs", "add", "-A", "--"}, present...)...); err != nil {
			return err
		}
	}
	if len(gone) > 0 {
		if _, err := s.git.Run(ctx, append([]string{"--literal-pathspecs", "rm", "-r", "-q", "--cached", "--ignore-unmatch", "--"}, gone...)...); err != nil {
			return err
		}
	}
	return nil
}

// commit stages paths and commits only them (--only), so whatever else is
// staged in an attached checkout stays staged and out of ka's commit. It
// returns the new HEAD and its parent ("" for a root commit).
func (s *Store) commit(ctx context.Context, msg, author, date string, paths []string) (sha, parent string, err error) {
	if err := s.stage(ctx, paths); err != nil {
		return "", "", err
	}
	args := []string{"--literal-pathspecs", "commit", "-q", "--no-verify", "-F", "-"}
	if author != "" {
		args = append(args, "--author", author)
	}
	g := s.git
	if date != "" {
		g = &gitx.Repo{Dir: s.git.Dir, Env: append(append([]string{}, s.git.Env...), "GIT_AUTHOR_DATE="+date)}
	}
	if _, err := g.RunRaw(ctx, []byte(msg), append(append(args, "--only", "--"), paths...)...); err != nil {
		if strings.Contains(err.Error(), "Please tell me who you are") || strings.Contains(err.Error(), "user.email") {
			return "", "", Errf(CodeGit, "set an identity: git config --global user.name \"Your Name\" && git config --global user.email you@example.com", "git has no commit identity")
		}
		return "", "", err
	}
	out, err := s.git.Run(ctx, "log", "-1", "--format=%H %P", "HEAD")
	if err != nil {
		return "", "", err
	}
	f := append(strings.Fields(out), "", "")
	return f[0], f[1], nil
}

// rollback returns paths (repo-relative files) in the index and working tree
// to HEAD after a failed write or commit, so the next operation, which reads
// tickets from disk, does not see this one as applied.
func (s *Store) rollback(ctx context.Context, paths []string) {
	ctx = context.WithoutCancel(ctx)
	inHead := map[string]bool{}
	if out, err := s.git.Run(ctx, append([]string{"--literal-pathspecs", "ls-tree", "-r", "-z", "--name-only", "HEAD"}, paths...)...); err == nil {
		for _, p := range strings.Split(out, "\x00") {
			inHead[p] = true
		}
	}
	var tracked, fresh []string
	for _, p := range paths {
		if inHead[p] {
			tracked = append(tracked, p)
		} else {
			fresh = append(fresh, p)
		}
	}
	if len(tracked) > 0 {
		s.git.Run(ctx, append([]string{"--literal-pathspecs", "checkout", "-q", "HEAD", "--"}, tracked...)...)
	}
	if len(fresh) > 0 {
		s.git.Run(ctx, append([]string{"--literal-pathspecs", "rm", "-q", "--cached", "--ignore-unmatch", "--"}, fresh...)...)
		for _, p := range fresh {
			os.Remove(filepath.Join(s.git.Dir, filepath.FromSlash(p)))
		}
	}
}

func trailers(o *Op) string {
	return "Ka-Op: " + o.Encode() + "\n" + actorTrailers(o.Actor, o.OnBehalfOf, o.Session, nil)
}

// actorTrailers is the trailer block every ka commit carries, with extra
// "Key: value" lines after it.
func actorTrailers(actor, onBehalfOf, session string, extra []string) string {
	var b strings.Builder
	if actor != "" {
		fmt.Fprintf(&b, "Ka-Actor: %s\n", actor)
	}
	if onBehalfOf != "" {
		fmt.Fprintf(&b, "Ka-On-Behalf-Of: %s\n", onBehalfOf)
	}
	if session != "" {
		fmt.Fprintf(&b, "Ka-Session: %s\n", session)
	}
	b.WriteString("Ka-Spec: 1\n")
	for _, t := range extra {
		b.WriteString(t + "\n")
	}
	return b.String()
}

// guard refuses to write to a store that is read-only here, needs a newer
// ka, or has a merge, rebase or cherry-pick in progress: the checks Do runs.
// Call it with the write lock held, before writing any file.
func (s *Store) guard() error {
	if s.ReadOnly {
		return Errf(CodeReadOnly, "this store is only reachable through extends; add it to your workspace to write to it", "store %s is read-only here", s.Name)
	}
	if err := s.checkVersion(); err != nil {
		return err
	}
	if p := s.inProgress(); p != "" {
		return s.busyErr(p)
	}
	return nil
}

// commitPaths commits a change that is not an operation (a board, ka fix, a
// project move or rename). The caller holds the write lock (s.lock), has
// called guard before writing, and has written or removed the files; paths
// are store-relative files or directories. Only those paths are staged and
// committed: anything else staged in an attached checkout stays staged and
// out of the commit, and untracked files elsewhere are never swept in.
//
// The message is subject plus the standard trailers for c (Ka-Actor,
// Ka-On-Behalf-Of, Ka-Session, Ka-Spec) and then extra ("Key: value" lines,
// such as "Ka-Moved-From: <url>"). It returns the new commit, or "" with no
// error when the paths have no changes. If the commit fails, the paths are
// unstaged again; the working tree is left as the caller wrote it.
//
// The index is not reloaded and nothing is pushed: callers refresh the
// index and push as they need.
func (s *Store) commitPaths(ctx context.Context, c Caller, subject string, extra []string, paths []string) (string, error) {
	if err := s.guard(); err != nil {
		return "", err
	}
	rp := make([]string, len(paths))
	for i, p := range paths {
		switch p = strings.TrimSuffix(p, "/"); {
		case p != "" && p != ".":
			rp[i] = s.repoPath(p)
		case s.sub != "":
			rp[i] = s.sub
		default:
			rp[i] = "."
		}
	}
	if err := s.stage(ctx, rp); err != nil {
		return "", err
	}
	// Nothing to commit for these paths (diff exits 0)?
	if _, err := s.git.Run(ctx, append([]string{"--literal-pathspecs", "diff", "--cached", "--quiet", "--"}, rp...)...); err == nil {
		return "", nil
	}
	msg := subject + "\n\n" + actorTrailers(c.Actor, c.OnBehalfOf, c.Session, extra)
	sha, _, err := s.commit(ctx, msg, "", "", rp)
	if err != nil {
		s.git.Run(context.WithoutCancel(ctx), append([]string{"--literal-pathspecs", "reset", "-q", "--"}, rp...)...)
		return "", err
	}
	return sha, nil
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
	if p := s.inProgress(); p != "" {
		return nil, s.busyErr(p)
	}
	changes, subject, err := apply(h.env(), o)
	if err != nil {
		return nil, err
	}
	remote, branch := s.upstream(ctx)
	res := &Result{HasRemote: remote != ""}
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
	if !push || remote == "" {
		return res, nil
	}
	return res, s.pushLocked(ctx, h, res, sha, remote, branch)
}

// pushLocked pushes, integrating upstream changes on rejection with bounded,
// jittered retries (PRD §10.1). mine is the commit of the caller's
// operation (empty for a plain sync); if its replay fails, the error is
// returned. A push that cannot reach the remote (offline, auth, a failed
// fetch after a rejection, retries exhausted) is not an error: the commits
// stay local and res.PushError says why. It records the outcome for the
// Sync & health view.
func (s *Store) pushLocked(ctx context.Context, h hooks, res *Result, mine, remote, branch string) (err error) {
	if remote == "" {
		return nil
	}
	fetched, lostOp := false, false
	defer func() {
		switch {
		case lostOp:
			// The caller's operation no longer applies: not a network failure.
			s.note(true, false, nil)
		case err != nil:
			s.note(false, false, err)
		case res.PushError != "":
			s.note(false, false, fmt.Errorf("%s", res.PushError))
		default:
			s.note(fetched, res.Pushed, nil)
		}
	}()
	backoff := 50 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			if s.git.Head(ctx) == s.remoteHead(ctx, remote, branch) {
				res.Pushed = true
				return nil
			}
		}
		_, err := s.net(ctx, "push", "-q", remote, "HEAD:refs/heads/"+branch)
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
		if ir != nil && ir.FetchErr != nil {
			// Rejected, and then the fetch failed: as good as offline. The
			// commit exists, so this is not the operation's failure.
			res.PushError = firstLine(AsError(ir.FetchErr).Message)
			return nil
		}
		fetched = fetched || err == nil
		if ir != nil {
			res.absorb(ir)
			if mine != "" {
				if ir.MineErr != nil {
					res.Commit = ""
					lostOp = true
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
		if attempt < 7 {
			s.sleep(backoff + time.Duration(rand.Int64N(int64(backoff))))
			backoff *= 2
		}
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
	FetchErr  error // the fetch failed; nothing else was done
	// Unpreserved describes local changes that lost and could not be
	// recorded as a comment; the pre-replay branch stays in refs/ka/backup.
	Unpreserved []string
	conflicted  bool // a hand edit was cherry-picked with conflicts
}

// keepBackups is how many refs/ka/backup/* refs a clone keeps.
const keepBackups = 20

// integrateLocked fetches and brings upstream commits under local ones.
// Local commits made by ka are replayed as operations; others are
// cherry-picked with the semantic merge driver, and anything that loses is
// preserved as a system comment (PRD §10.4). A replay is all or nothing: if
// any step fails, the branch goes back to where it was.
func (s *Store) integrateLocked(ctx context.Context, h hooks, remote, branch, mine string) (*integrateResult, error) {
	if err := s.fetch(ctx, remote, branch); err != nil {
		ferr := Errf(CodeGit, "check the network and credentials, then run ka sync", "fetch failed: %s", firstLine(err.Error()))
		return &integrateResult{MineSHA: mine, FetchErr: ferr}, ferr
	}
	// From here on the work is local. Finish it even if the caller goes
	// away: a replay stopped halfway would drop local commits.
	ctx = context.WithoutCancel(ctx)
	// Use the fetched commit, not the ref, throughout: a concurrent fetch
	// (another process, or the fetch loop) may move the ref meanwhile.
	upSHA := s.remoteHead(ctx, remote, branch)
	head := s.git.Head(ctx)
	res := &integrateResult{MineSHA: mine}
	if upSHA == "" || upSHA == head {
		return res, nil
	}
	if err := s.checkUpstreamVersion(ctx, upSHA); err != nil {
		return nil, err
	}
	if head == "" {
		if _, err := s.git.Run(ctx, "reset", "-q", "--keep", upSHA); err != nil {
			return nil, err
		}
		res.Moved = true
		h.reloaded(nil)
		return res, nil
	}
	base, _ := s.git.Run(ctx, "merge-base", head, upSHA)
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
		if _, err := s.git.Run(ctx, "merge", "-q", "--ff-only", upSHA); err != nil {
			return nil, Errf(CodeGit, "commit or stash local edits to the store, then run ka sync", "fast-forward failed: %s", firstLine(err.Error()))
		}
		res.Moved = true
		after := s.git.Head(ctx)
		s.reloadRange(ctx, h, head, after)
		s.own(head, after)
		return res, nil
	}

	// Diverged: replay local commits on top of upstream. Merge commits are
	// linearized: both sides are replayed instead.
	local, err := s.git.LogLinear(ctx, upSHA+".."+head)
	if err != nil {
		return nil, err
	}
	backup := "refs/ka/backup/" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if _, err := s.git.Run(ctx, "update-ref", backup, head); err != nil {
		return nil, err
	}
	if _, err := s.git.Run(ctx, "reset", "-q", "--keep", upSHA); err != nil {
		s.git.Run(ctx, "update-ref", "-d", backup)
		return nil, Errf(CodeGit, "commit or stash local edits to the store, then run ka sync", "cannot move to upstream: %s", firstLine(err.Error()))
	}
	res.Moved = true
	s.reloadRange(ctx, h, head, upSHA)
	s.own(head, upSHA)
	if err := s.replay(ctx, h, res, local, mine); err != nil {
		return nil, s.abortReplay(ctx, h, head, backup, err)
	}
	if len(res.Preserved) == 0 && len(res.Unpreserved) == 0 && !res.conflicted {
		// Everything was replayed as it was: the backup holds nothing more.
		s.git.Run(ctx, "update-ref", "-d", backup)
	}
	s.pruneBackups(ctx)
	return res, nil
}

// replay re-applies local commits on top of the current HEAD.
func (s *Store) replay(ctx context.Context, h hooks, res *integrateResult, local []gitx.Commit, mine string) error {
	idMap := map[string]string{}
	res.IDMap = idMap
	var failed []failedOp
	for _, c := range local {
		o := opFromCommit(c)
		if o == nil {
			if err := s.cherryPick(ctx, h, c, res); err != nil {
				return err
			}
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
			return err
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
		} else {
			res.Unpreserved = append(res.Unpreserved, fmt.Sprintf("%s %s by %s (commit %s): %s; the original is kept under refs/ka/backup/",
				f.op.Op, f.op.ID, orDefault(f.op.Actor, "an unknown actor"), short(f.commit.SHA), firstLine(AsError(f.err).Error())))
		}
	}
	res.Preserved = append(res.Preserved, s.flushPreserved(ctx, h)...)
	return nil
}

// abortReplay puts the branch back where it was before a replay that
// failed, so no local commit is lost or published half-replayed.
func (s *Store) abortReplay(ctx context.Context, h hooks, orig, backup string, cause error) error {
	cur := s.git.Head(ctx)
	if _, err := s.git.Run(ctx, "reset", "-q", "--keep", orig); err != nil {
		h.reloaded(nil)
		return Errf(CodeGit, "restore the branch by hand: git reset --keep "+backup,
			"replay failed (%s) and the branch could not be restored: %s", firstLine(AsError(cause).Error()), firstLine(err.Error()))
	}
	s.reloadRange(ctx, h, cur, orig)
	s.own(cur, orig)
	return cause
}

// pruneBackups keeps the newest keepBackups refs/ka/backup/* refs.
func (s *Store) pruneBackups(ctx context.Context) {
	out, err := s.git.Run(ctx, "for-each-ref", "--sort=-refname", "--format=%(refname)", "refs/ka/backup/")
	if err != nil {
		return
	}
	refs := splitNonEmpty(out)
	if len(refs) <= keepBackups {
		return
	}
	var stdin strings.Builder
	for _, r := range refs[keepBackups:] {
		stdin.WriteString("delete " + r + "\n")
	}
	s.git.RunRaw(ctx, []byte(stdin.String()), "update-ref", "--stdin")
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
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
// takes the upstream side and the local version is preserved (in res).
// Values the merge driver preserved are flushed by the caller once the whole
// replay is done.
func (s *Store) cherryPick(ctx context.Context, h hooks, c gitx.Commit, res *integrateResult) error {
	before := s.git.Head(ctx)
	if _, err := s.git.Run(ctx, "cherry-pick", "--allow-empty", "--keep-redundant-commits", c.SHA); err != nil {
		files, _ := s.git.Run(ctx, "diff", "--name-only", "--diff-filter=U")
		s.git.Run(ctx, "cherry-pick", "--abort")
		if _, err := s.git.Run(ctx, "cherry-pick", "--allow-empty", "--keep-redundant-commits", "-X", "ours", c.SHA); err != nil {
			s.git.Run(ctx, "cherry-pick", "--abort")
			return Errf(CodeGit, "resolve by hand, then run ka sync", "cannot replay %s: %s", short(c.SHA), firstLine(err.Error()))
		}
		after := s.git.Head(ctx)
		s.reloadRange(ctx, h, before, after)
		s.own(before, after)
		res.conflicted = true
		for _, p := range strings.Split(files, "\n") {
			if p == "" {
				continue
			}
			content, _ := s.git.Run(ctx, "show", c.SHA+":"+p)
			sp, _ := s.storePath(p)
			if id := ticketIDFromPath(sp); id != "" {
				body := fmt.Sprintf("A local edit to `%s` conflicted with upstream and lost. Upstream was kept; the local version from %s (%s, by %s) is below.\n\n````\n%s\n````\n",
					sp, c.SHA[:12], c.Subject, c.Author, content)
				if q := s.writeSystemComment(ctx, h, id, body, ""); q != "" {
					res.Preserved = append(res.Preserved, q)
					continue
				}
			}
			// Not a ticket (or its ticket is gone): no comment to put it in.
			res.Unpreserved = append(res.Unpreserved, fmt.Sprintf("local edit to %s in %s (%s) conflicted and lost to upstream; the original is kept under refs/ka/backup/",
				sp, short(c.SHA), c.Subject))
		}
		return nil
	}
	after := s.git.Head(ctx)
	s.reloadRange(ctx, h, before, after)
	s.own(before, after)
	return nil
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
	fileIDs := map[string][]string{} // file -> tickets it has values for
	for _, e := range ents {
		f := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ps []Preserved
		if json.Unmarshal(b, &ps) != nil {
			continue // perhaps still being written; kept for the next flush
		}
		fileIDs[f] = []string{}
		for _, p := range ps {
			sp, _ := s.storePath(p.Path)
			if id := ticketIDFromPath(sp); id != "" {
				byTicket[id] = append(byTicket[id], p)
				fileIDs[f] = append(fileIDs[f], id)
			}
		}
	}
	ids := make([]string, 0, len(byTicket))
	for id := range byTicket {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []string
	written := map[string]bool{}
	for _, id := range ids {
		var b strings.Builder
		b.WriteString("Concurrent edits conflicted during a merge. The published value was kept; the other value is preserved here.\n\n")
		for _, p := range byTicket[id] {
			fmt.Fprintf(&b, "- **%s**: kept `%s`, lost:\n\n````\n%s\n````\n\n", p.Field, oneLine(p.Kept), p.Lost)
		}
		if q := s.writeSystemComment(ctx, h, id, b.String(), ""); q != "" {
			out = append(out, q)
			written[id] = true
		}
	}
	// A file goes once every value in it is in a committed comment; one
	// whose comment could not be written stays for the next flush.
	for f, ids := range fileIDs {
		if !slices.ContainsFunc(ids, func(id string) bool { return !written[id] }) {
			os.Remove(f)
		}
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
		// Nothing local to publish: "pushed" means level with upstream.
		res.Pushed = err == nil && s.git.Head(ctx) == s.remoteHead(ctx, remote, branch)
		return res, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.syncLocked(ctx, h, res, remote, branch, push)
}

// syncLocked is sync for a writable store with a remote, with the write lock
// held by the caller.
func (s *Store) syncLocked(ctx context.Context, h hooks, res *Result, remote, branch string, push bool) (*Result, error) {
	if p := s.inProgress(); p != "" {
		return nil, s.busyErr(p)
	}
	ir, err := s.integrateLocked(ctx, h, remote, branch, "")
	if err != nil {
		s.note(false, false, err)
		return res, err
	}
	s.note(true, false, nil)
	res.absorb(ir)
	res.Preserved = append(res.Preserved, s.flushPreserved(ctx, h)...)
	if level := s.git.Head(ctx) == s.remoteHead(ctx, remote, branch); !push || level {
		res.Pushed = level
		return res, nil
	}
	return res, s.pushLocked(ctx, h, res, "", remote, branch)
}

// integrateReadOnly fast-forwards a store this workspace only reads.
func (s *Store) integrateReadOnly(ctx context.Context, h hooks, remote, branch string) (bool, error) {
	if !s.Managed {
		return false, nil // never move someone's attached checkout we do not write to
	}
	before := s.git.Head(ctx)
	if err := s.fetch(ctx, remote, branch); err != nil {
		s.note(false, false, Errf(CodeGit, "check the network and credentials", "fetch failed: %s", firstLine(err.Error())))
		return false, nil
	}
	s.note(true, false, nil)
	up := s.remoteHead(ctx, remote, branch)
	if err := s.checkUpstreamVersion(ctx, up); err != nil {
		return false, err
	}
	if _, err := s.git.Run(ctx, "merge", "-q", "--ff-only", up); err != nil {
		return false, nil
	}
	after := s.git.Head(ctx)
	if after != before {
		s.reloadRange(ctx, h, before, after)
		s.own(before, after)
	}
	return after != before, nil
}

// State reports sync status without touching the network.
func (s *Store) State(ctx context.Context) SyncState {
	st := SyncState{Store: s.Name, Attached: !s.Managed, Head: short(s.git.Head(ctx))}
	if p := s.inProgress(); p != "" {
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
