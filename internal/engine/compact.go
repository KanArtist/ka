package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KanArtist/ka/internal/gitx"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// History compaction (docs/engine.md "Compacting history"). A compaction
// replaces a store's branch with one root commit whose tree is the tree of
// the old tip, after publishing a summary of what is dropped in history/.
// Clients recognize the new history by its trailers and adopt it.

// Trailers of compaction commits (spec §10).
const (
	// TrailerCompactedFrom, on the new root commit, names the last commit of
	// the history it replaces.
	TrailerCompactedFrom = "Ka-Compacted-From"
	// TrailerCompactedTree, on the new root commit, is that commit's tree.
	TrailerCompactedTree = "Ka-Compacted-Tree"
	// TrailerCompactionSummary names the summary file a commit added (on the
	// summary commit) or describes (on the root commit).
	TrailerCompactionSummary = "Ka-Compaction-Summary"
)

// AdoptThreshold is the share of the tickets a client last saw upstream that
// must exist in a compacted history it never saw the start of, for the
// client to adopt it (docs/engine.md). Tickets are never deleted in normal
// operation, so a real compaction of the same store has all of them.
const AdoptThreshold = 0.95

// GCTimeout bounds the git gc that drops old history after a compaction.
var GCTimeout = 10 * time.Minute

// beforeForcePush runs (in tests) between the summary push and the force
// push, so a test can move the remote in between.
var beforeForcePush func()

// CompactIn asks for a compaction.
type CompactIn struct {
	Store string `json:"store"`
	// Summary is the narrative body of the summary file (Markdown); ka never
	// writes one itself.
	Summary string `json:"summary,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
	// Confirm must equal Store for a real run.
	Confirm string `json:"confirm,omitempty"`
}

// CompactOut reports a compaction, or what one would do.
type CompactOut struct {
	Store          string         `json:"store"`
	DryRun         bool           `json:"dry_run,omitempty"`
	Remote         string         `json:"remote"`
	Branch         string         `json:"branch"`
	From           string         `json:"from" doc:"The tip of the history being compacted (after the summary commit, on a real run)."`
	Root           string         `json:"root,omitempty" doc:"The new root commit."`
	Tree           string         `json:"tree,omitempty"`
	Commits        int            `json:"commits" doc:"Commits in the history being dropped."`
	FirstCommit    string         `json:"first_commit,omitempty"`
	LastCommit     string         `json:"last_commit,omitempty"`
	Actors         map[string]int `json:"actors,omitempty" doc:"Commits per actor."`
	TicketsCreated int            `json:"tickets_created,omitempty"`
	TicketsClosed  int            `json:"tickets_closed,omitempty"`
	SizeBefore     int64          `json:"size_before" doc:"Approximate size of the clone's object store, in bytes."`
	SizeAfter      int64          `json:"size_after,omitempty"`
	SummaryFile    string         `json:"summary_file" doc:"Store-relative path of the summary file (a dry run shows a sample name)."`
	Previous       string         `json:"previous,omitempty" doc:"The previous summary file, if the store was compacted before."`
	RequiresKa     string         `json:"requires_ka" doc:"requires_ka after the compaction."`
	RequiresKaWas  string         `json:"requires_ka_was,omitempty" doc:"requires_ka before, when the compaction raises it (empty: none)."`
	RaisesKa       bool           `json:"raises_requires_ka,omitempty" doc:"The summary commit raises requires_ka to requires_ka."`
	// BackupRefs counts the refs/ka/backup/* refs (originals of earlier
	// replays) that the compaction deletes along with the old history.
	BackupRefs int      `json:"backup_refs,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// CompactedEvent is the payload of store.compacted.
type CompactedEvent struct {
	From    string `json:"from,omitempty" doc:"The last commit of the replaced history."`
	Root    string `json:"root,omitempty" doc:"The new root commit."`
	Summary string `json:"summary,omitempty" doc:"The summary file, store-relative."`
	// Summaries (activity entries only) names every summary file in the
	// store at that commit, oldest first.
	Summaries []string `json:"summaries,omitempty"`
	// Adopted is true when this clone took in a compaction made elsewhere.
	Adopted bool `json:"adopted,omitempty"`
}

// Compact squashes a store's whole history into one root commit and
// force-pushes it (with a lease), after publishing a summary of the history
// it drops. Nothing irreversible happens before the force push succeeds: on
// any failure the clone's branch is where it was after the summary push.
func (s *Service) Compact(ctx context.Context, c Caller, in CompactIn) (*CompactOut, error) {
	if !in.DryRun && in.Confirm != in.Store {
		return nil, Errf(CodeInvalid, "run a dry run first, then confirm with the store name (ka compact --store "+in.Store+" --yes; ka_compact confirm: \""+in.Store+"\")",
			"compacting %s rewrites its shared history and cannot be undone on the remote; it needs explicit confirmation", in.Store)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	st, err := s.writableStore(in.Store)
	if err != nil {
		return nil, err
	}
	remote, branch := st.upstream(ctx)
	if remote == "" {
		return nil, Errf(CodeInvalid, "compaction replaces a shared history; a store without a remote can be squashed with plain git", "store %s has no remote", st.Name)
	}
	if st.sub != "" {
		return nil, Errf(CodeInvalid, "compact only stores that have a repository of their own",
			"store %s is the directory %s of a larger repository; compacting would rewrite the whole repository's history", st.Name, st.sub)
	}
	if s.NoPush {
		return nil, Errf(CodeInvalid, "run it without --no-push or --offline", "compaction must push")
	}
	localBranch := st.git.Branch(ctx)
	if localBranch == "" {
		return nil, Errf(CodeGit, "check out the store's branch", "store %s is not on a branch", st.Name)
	}
	unlock, err := st.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := st.guard(); err != nil {
		return nil, err
	}
	h := s.hooksBy(st, c.Actor)

	// 1. Level with upstream, and nothing uncommitted.
	res, err := st.syncLocked(ctx, h, &Result{HasRemote: true}, remote, branch, true)
	if res != nil {
		s.syncEvents(st, res)
	}
	if err != nil {
		return nil, err
	}
	head := st.git.Head(ctx)
	if up := st.remoteHead(ctx, remote, branch); head == "" || head != up || !res.Pushed {
		why := "it has commits that are not pushed"
		if res.PushError != "" {
			why = res.PushError
		}
		return nil, Errf(CodeGit, "run ka sync and resolve what it reports, then retry", "store %s is not level with %s/%s: %s", st.Name, remote, branch, why)
	}
	if dirty, _ := st.git.Run(ctx, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		return nil, Errf(CodeGit, "commit or stash local edits to the store, then retry",
			"store %s has uncommitted changes: %s", st.Name, firstLine(dirty))
	}

	out := &CompactOut{Store: st.Name, DryRun: in.DryRun, Remote: remote, Branch: branch}
	if err := s.compactFacts(ctx, st, head, out); err != nil {
		return nil, err
	}
	now := s.now()
	name := summaryName(now, out.Previous, func(n string) bool { return st.exists(path.Join(kaspec.HistoryDir, n)) })
	out.SummaryFile = kaspec.HistoryDir + "/" + name
	storeYML, _ := os.ReadFile(filepath.Join(st.Root, ".ka", "store.yml"))
	out.RequiresKa = requiresKa(storeYML)
	raise := !kaspec.VersionAtLeastRelease(out.RequiresKa, kaspec.CompactionMinKa)
	if raise {
		out.RequiresKaWas, out.RequiresKa, out.RaisesKa = out.RequiresKa, kaspec.CompactionMinKa, true
	}
	if in.DryRun {
		return out, nil
	}

	// 2. Publish the summary (and the raised requires_ka) as a normal commit.
	sum := st.pendingSummary(ctx, head, in.Summary)
	if sum != "" {
		// An earlier attempt published its summary and then failed: use it.
		out.SummaryFile, out.RequiresKaWas, out.RaisesKa, out.Previous = sum, "", false, ""
		if names := st.summaryNames(ctx, "HEAD"); len(names) >= 2 {
			out.Previous = names[len(names)-2]
		}
	} else {
		paths := []string{out.SummaryFile}
		hs := &kaspec.HistorySummary{CompactedAt: now, By: c.Actor, OnBehalfOf: c.OnBehalfOf, From: head, Tree: out.Tree,
			Commits: int64(out.Commits), FirstCommit: out.FirstCommit, LastCommit: out.LastCommit, Actors: map[string]int64{},
			Created: int64(out.TicketsCreated), Closed: int64(out.TicketsClosed), Previous: out.Previous, Body: in.Summary}
		if !kaspec.HumanRe.MatchString(hs.OnBehalfOf) {
			hs.OnBehalfOf = ""
		}
		for a, n := range out.Actors {
			hs.Actors[a] = int64(n)
		}
		files := []change{{Path: out.SummaryFile, Data: kaspec.FormatHistorySummary(hs)}}
		if raise {
			files = append(files, change{Path: ".ka/store.yml", Data: setRequiresKa(storeYML, kaspec.CompactionMinKa)})
			paths = append(paths, ".ka/store.yml")
		}
		repoPaths := make([]string, len(paths))
		for i, p := range paths {
			repoPaths[i] = st.repoPath(p)
		}
		if err := st.writeChanges(files); err != nil {
			st.rollback(ctx, repoPaths)
			return nil, err
		}
		subject := fmt.Sprintf("Compaction summary: %s (%d commits)", out.SummaryFile, out.Commits)
		sha, err := st.commitPaths(ctx, c, subject, []string{TrailerCompactionSummary + ": " + out.SummaryFile}, paths)
		if err != nil || sha == "" {
			st.rollback(ctx, repoPaths)
			if err == nil {
				err = Errf(CodeGit, "", "the summary commit was empty")
			}
			return nil, err
		}
		st.own(head, sha)
		h.reloaded(paths)
		if _, err := st.net(ctx, "push", "-q", remote, "HEAD:refs/heads/"+branch); err != nil {
			// Not published: take the summary commit back.
			st.git.Run(context.WithoutCancel(ctx), "reset", "-q", "--keep", head)
			st.own(sha, head)
			h.reloaded(paths)
			if gitx.IsNonFastForward(err) {
				return nil, Errf(CodeGitBusy, "run ka compact again", "someone pushed to %s/%s while compacting %s; nothing was changed", remote, branch, st.Name)
			}
			return nil, Errf(CodeGit, "check the network and credentials, then retry", "cannot push the compaction summary: %s", firstLine(err.Error()))
		}
		st.git.Run(ctx, "update-ref", "refs/remotes/"+remote+"/"+branch, "HEAD")
		st.note(true, true, nil)
	}
	from := st.git.Head(ctx)
	out.From = from

	// 3. One root commit with the same tree.
	ctx = context.WithoutCancel(ctx) // from here on, finish or restore
	tree, err := st.git.Run(ctx, "rev-parse", from+"^{tree}")
	if err != nil {
		return nil, err
	}
	out.Tree = tree
	upto := out.LastCommit
	if len(upto) >= 10 {
		upto = upto[:10]
	}
	msg := fmt.Sprintf("Compact history: %d commits up to %s\n\nThe history before this commit was compacted; %s summarizes it.\n\n", out.Commits, upto, out.SummaryFile) +
		actorTrailers(c.Actor, c.OnBehalfOf, c.Session, []string{
			TrailerCompactedFrom + ": " + from,
			TrailerCompactedTree + ": " + tree,
			TrailerCompactionSummary + ": " + out.SummaryFile,
		})
	rootRaw, err := st.git.RunRaw(ctx, []byte(msg), "commit-tree", tree, "-F", "-")
	if err != nil {
		return nil, Errf(CodeGit, "", "cannot create the new root commit: %s", firstLine(err.Error()))
	}
	root := strings.TrimSpace(string(rootRaw))
	ref := "refs/heads/" + localBranch
	if _, err := st.git.Run(ctx, "update-ref", "-m", "ka compact", ref, root, from); err != nil {
		return nil, Errf(CodeGit, "", "cannot move %s to the new root: %s", localBranch, firstLine(err.Error()))
	}

	// 4. Replace the remote branch, only if it is still at from.
	if beforeForcePush != nil {
		beforeForcePush()
	}
	if _, err := st.net(ctx, "push", "-q", "--force-with-lease=refs/heads/"+branch+":"+from, remote, root+":refs/heads/"+branch); err != nil {
		if _, rerr := st.git.Run(ctx, "update-ref", "-m", "ka compact: restore", ref, from, root); rerr != nil {
			return nil, Errf(CodeGit, "restore the branch by hand: git update-ref "+ref+" "+from,
				"the force push failed (%s) and the branch could not be restored: %s", firstLine(err.Error()), firstLine(rerr.Error()))
		}
		return nil, forcePushError(err, st.Name, remote, branch)
	}
	st.git.Run(ctx, "update-ref", "refs/remotes/"+remote+"/"+branch, root)
	st.own(from, root) // the same tree: nothing to reload
	out.Root = root
	st.note(true, true, nil)

	// 5. Drop the old history from this clone.
	out.Warnings = st.dropOldHistory(ctx, "", from)
	out.SizeAfter = st.objectBytes(ctx)
	s.publish(Event{Type: EvStoreCompacted, At: s.now(), Store: st.Name, Actor: c.Actor,
		Data: CompactedEvent{From: from, Root: root, Summary: out.SummaryFile}})
	return out, nil
}

// summaryName picks a summary file name for time now that is not taken and
// sorts after previous, so names stay in compaction order even within one
// second (or with a clock that went back).
func summaryName(now, previous string, taken func(string) bool) string {
	t, _ := time.Parse("2006-01-02T15:04:05Z", now)
	if len(previous) >= 16 {
		if pt, err := time.Parse("20060102T150405Z", previous[:16]); err == nil && pt.After(t) {
			t = pt
		}
	}
	for i := 0; ; i++ {
		name := kaspec.HistoryFilename(t.Format("2006-01-02T15:04:05Z"), kaspec.RandomBase32(4))
		if name > previous && !taken(name) {
			return name
		}
		if i%16 == 15 {
			t = t.Add(time.Second)
		}
	}
}

// forcePushError explains a refused force push. The branch has already
// been put back.
func forcePushError(err error, store, remote, branch string) error {
	msg := firstLine(err.Error())
	var ge *gitx.Error
	if errors.As(err, &ge) {
		msg = strings.TrimSpace(ge.Stderr)
	}
	switch {
	case strings.Contains(msg, "stale info") || strings.Contains(msg, "fetch first") || strings.Contains(msg, "incorrect old value") ||
		strings.Contains(msg, "cannot lock ref") || strings.Contains(msg, "failed to update ref"):
		return Errf(CodeGitBusy, "run ka compact again", "someone pushed to %s/%s during the compaction of %s; nothing was rewritten (the summary commit stays published)", remote, branch, store)
	case strings.Contains(msg, "rejected") || strings.Contains(msg, "denied") || strings.Contains(msg, "denying") ||
		strings.Contains(msg, "protected") || strings.Contains(msg, "declined"):
		return Errf(CodeGit, "allow force pushes to "+branch+" for this operation, or compact on the host",
			"the host refused to replace %s/%s: %s; nothing was rewritten", remote, branch, lastLine(msg))
	}
	return Errf(CodeGit, "check the network and credentials, then run ka compact again", "the force push failed: %s; nothing was rewritten", lastLine(msg))
}

func lastLine(s string) string {
	ls := splitNonEmpty(strings.TrimSpace(s))
	if len(ls) == 0 {
		return s
	}
	return strings.TrimSpace(ls[len(ls)-1])
}

// compactFacts fills in what the history up to head holds.
func (s *Service) compactFacts(ctx context.Context, st *Store, head string, out *CompactOut) error {
	log, err := st.git.Run(ctx, "log", "--format=%aI%x00%an <%ae>%x00%(trailers:key=Ka-Actor,valueonly,separator=%x01)%x00%(trailers:key=Ka-Op,valueonly,separator=%x01)%x00", head, "--")
	if err != nil {
		return Errf(CodeGit, "", "cannot read the history: %s", firstLine(err.Error()))
	}
	out.From = head
	out.Actors = map[string]int{}
	f := strings.Split(log, "\x00")
	for i := 0; i+3 < len(f); i += 4 {
		date := strings.TrimLeft(f[i], "\n")
		if date == "" {
			continue
		}
		out.Commits++
		if t, err := time.Parse(time.RFC3339, date); err == nil {
			date = t.UTC().Format("2006-01-02T15:04:05Z")
		}
		if out.LastCommit == "" {
			out.LastCommit = date
		}
		out.FirstCommit = date
		actor, _, _ := strings.Cut(f[i+2], "\x01")
		if actor = strings.TrimSpace(actor); actor == "" {
			actor = s.ix.authorActor(f[i+1])
		}
		out.Actors[actor]++
		op, _, _ := strings.Cut(f[i+3], "\x01")
		switch {
		case strings.HasPrefix(op, "create "):
			out.TicketsCreated++
		case strings.HasPrefix(op, "transition ") && strings.Contains(op, `"type":"resolution"`):
			out.TicketsClosed++
		}
	}
	out.Tree, _ = st.git.Run(ctx, "rev-parse", head+"^{tree}")
	out.SizeBefore = st.objectBytes(ctx)
	out.BackupRefs = len(splitNonEmpty(must1(st.git.Run(ctx, "for-each-ref", "--format=%(refname)", "refs/ka/backup/"))))
	if names := st.summaryNames(ctx, "HEAD"); len(names) > 0 {
		out.Previous = names[len(names)-1]
	}
	return nil
}

// exists reports whether a store-relative path exists in the working tree.
func (s *Store) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(s.Root, filepath.FromSlash(rel)))
	return err == nil
}

// summaryNames lists the summary files (names, oldest first) at rev.
func (s *Store) summaryNames(ctx context.Context, rev string) []string {
	out, err := s.git.Run(ctx, "ls-tree", "--name-only", rev, "--", s.repoPath(kaspec.HistoryDir)+"/")
	if err != nil {
		return nil
	}
	var names []string
	for _, p := range splitNonEmpty(out) {
		if n := path.Base(p); kaspec.HistoryFileRe.MatchString(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// pendingSummary returns the summary file an earlier, failed compaction
// published, when HEAD is still its summary commit (and the caller gave no
// different narrative): a retry then reuses it instead of adding another.
func (s *Store) pendingSummary(ctx context.Context, head, narrative string) string {
	c, err := s.git.CommitInfo(ctx, head)
	if err != nil {
		return ""
	}
	file := c.Trailers[TrailerCompactionSummary]
	if file == "" || c.Trailers[TrailerCompactedFrom] != "" {
		return ""
	}
	data, err := s.git.Run(ctx, "show", head+":"+s.repoPath(file))
	if err != nil {
		return ""
	}
	h, _ := kaspec.ParseHistorySummary(file, []byte(data+"\n"))
	if h == nil {
		return ""
	}
	if parent, _ := s.git.Run(ctx, "rev-parse", "-q", "--verify", head+"^"); parent == "" || parent != h.From {
		return ""
	}
	if strings.TrimSpace(narrative) != "" && strings.TrimSpace(narrative) != strings.TrimSpace(h.Body) {
		return ""
	}
	return file
}

var requiresLine = regexp.MustCompile(`(?m)^requires_ka:.*$`)

// setRequiresKa sets requires_ka in store.yml text, keeping everything else.
func setRequiresKa(data []byte, v string) []byte {
	s := string(kaspec.NormalizeEncoding(data))
	if requiresLine.MatchString(s) {
		return []byte(requiresLine.ReplaceAllString(s, "requires_ka: "+v))
	}
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "name:") {
			if !strings.HasSuffix(l, "\n") {
				lines[i] += "\n"
			}
			return []byte(strings.Join(lines[:i+1], "") + "requires_ka: " + v + "\n" + strings.Join(lines[i+1:], ""))
		}
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s + "requires_ka: " + v + "\n")
}

// objectBytes approximates the size of the clone's object store.
func (s *Store) objectBytes(ctx context.Context) int64 {
	out, err := s.git.Run(ctx, "count-objects", "-v")
	if err != nil {
		return 0
	}
	var kib int64
	for _, l := range splitNonEmpty(out) {
		k, v, _ := strings.Cut(l, ": ")
		if k == "size" || k == "size-pack" {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			kib += n
		}
	}
	return kib * 1024
}

// dropOldHistory removes what pins the history a compaction replaced (ka's
// backup refs except keep, ORIG_HEAD, the reflogs) and prunes it. Failures
// are warnings: the new history is already in place.
func (s *Store) dropOldHistory(ctx context.Context, keep, oldTip string) []string {
	var warnings []string
	if out, err := s.git.Run(ctx, "for-each-ref", "--format=%(refname)", "refs/ka/"); err == nil {
		var stdin strings.Builder
		for _, r := range splitNonEmpty(out) {
			if r != keep {
				stdin.WriteString("delete " + r + "\n")
			}
		}
		if stdin.Len() > 0 {
			s.git.RunRaw(ctx, []byte(stdin.String()), "update-ref", "--stdin")
		}
	}
	s.git.Run(ctx, "update-ref", "-d", "ORIG_HEAD")
	os.Remove(filepath.Join(s.gitDir, "FETCH_HEAD"))
	if keep == "" {
		if out, err := s.git.Run(ctx, "for-each-ref", "--format=%(refname)", "--contains", oldTip); err == nil && out != "" {
			warnings = append(warnings, "these refs still reach the old history, so it stays on disk: "+strings.Join(splitNonEmpty(out), ", ")+" (delete them, then run git gc --prune=now)")
		}
	}
	if _, err := s.git.Run(ctx, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all"); err != nil {
		warnings = append(warnings, "git reflog expire failed: "+firstLine(err.Error()))
	}
	gctx, cancel := context.WithTimeout(ctx, GCTimeout)
	defer cancel()
	if _, err := s.git.Run(gctx, "gc", "--quiet", "--prune=now"); err != nil {
		warnings = append(warnings, "git gc failed ("+firstLine(err.Error())+"); the old history is unreachable and a later git gc removes it")
	}
	return warnings
}

// ---- adopting a compaction made elsewhere -------------------------------------------

type compactionStep struct{ from, tree string }

// compactionChain reads the compactions recorded in the history at up: the
// root commit's trailers (the newest), then every summary file, newest
// first. ok is false when the root is not a compaction root.
func (s *Store) compactionChain(ctx context.Context, up string) (steps []compactionStep, ok bool) {
	roots := splitNonEmpty(must1(s.git.Run(ctx, "rev-list", "--max-parents=0", up)))
	if len(roots) != 1 {
		return nil, false
	}
	c, err := s.git.CommitInfo(ctx, roots[0])
	if err != nil || c.Trailers[TrailerCompactedFrom] == "" {
		return nil, false
	}
	steps = append(steps, compactionStep{c.Trailers[TrailerCompactedFrom], c.Trailers[TrailerCompactedTree]})
	names := s.summaryNames(ctx, up)
	for i := len(names) - 1; i >= 0; i-- {
		data, err := s.git.Run(ctx, "show", up+":"+s.repoPath(kaspec.HistoryDir+"/"+names[i]))
		if err != nil {
			continue
		}
		if h, _ := kaspec.ParseHistorySummary(names[i], []byte(data+"\n")); h != nil && kaspec.CommitRe.MatchString(h.From) {
			steps = append(steps, compactionStep{h.From, h.Tree})
		}
	}
	return steps, true
}

func must1(s string, _ error) string { return s }

// rootCommit is HEAD's root commit ("" when there are several, or none).
func (s *Store) rootCommit(ctx context.Context) string {
	if roots := splitNonEmpty(must1(s.git.Run(ctx, "rev-list", "--max-parents=0", "HEAD"))); len(roots) == 1 {
		return roots[0]
	}
	return ""
}

// adoptedEvent describes a compaction this clone adopted.
func (s *Store) adoptedEvent() CompactedEvent {
	ctx := context.Background()
	e := CompactedEvent{Root: s.rootCommit(ctx), Adopted: true}
	if c, err := s.git.CommitInfo(ctx, e.Root); err == nil && e.Root != "" {
		e.From, e.Summary = c.Trailers[TrailerCompactedFrom], c.Trailers[TrailerCompactionSummary]
	}
	return e
}

// compactionBase decides whether up, a history that shares no commit with
// head, is a compaction of this store, and returns the commit of the old
// history that it includes (local commits after it are replayed). why
// explains a refusal.
func (s *Store) compactionBase(ctx context.Context, head, up, lastUp string) (base, why string) {
	steps, ok := s.compactionChain(ctx, up)
	if !ok {
		return "", "its first commit is not a ka history compaction"
	}
	if a, b := s.storeNameAt(ctx, head), s.storeNameAt(ctx, up); a != b {
		return "", fmt.Sprintf("it holds store %q, not %q", b, a)
	}
	// Exact: this clone has a commit the compaction recorded.
	for _, st := range steps {
		if !s.git.OK(ctx, "cat-file", "-e", st.from+"^{commit}") {
			continue
		}
		if st.from != head {
			if mb, _ := s.git.Run(ctx, "merge-base", st.from, head); mb == "" {
				continue
			}
		}
		tree, _ := s.git.Run(ctx, "rev-parse", st.from+"^{tree}")
		if st.tree != "" && tree != st.tree {
			return "", fmt.Sprintf("it records commit %s with tree %s, but here that commit has tree %s", short(st.from), short(st.tree), short(tree))
		}
		return st.from, ""
	}
	// Behind: this clone never saw where the compaction started. Compare
	// what it last saw upstream with the new tree.
	if lastUp == "" {
		if !s.ReadOnly {
			return "", "this clone has no record of what it last fetched, so it cannot tell which local commits are unpublished"
		}
		lastUp = head
	}
	had, have := s.ticketDirsAt(ctx, lastUp), s.ticketDirsAt(ctx, up)
	n := 0
	for d := range had {
		if have[d] {
			n++
		}
	}
	if len(had) > 0 && float64(n) < AdoptThreshold*float64(len(had)) {
		return "", fmt.Sprintf("it has only %d of the %d tickets this clone last saw upstream (%.0f%% required)", n, len(had), AdoptThreshold*100)
	}
	return lastUp, ""
}

// lastUpstream finds the upstream commit this clone knew before the
// compaction: prev (the tracking ref before this fetch) or, when a fetch
// already replaced it, the newest earlier value in the tracking ref's
// reflog that belongs to the old history.
func (s *Store) lastUpstream(ctx context.Context, remote, branch, head, up, prev string) string {
	cands := []string{prev}
	if out, err := s.git.Run(ctx, "reflog", "show", "--format=%H", "refs/remotes/"+remote+"/"+branch, "--"); err == nil {
		cands = append(cands, splitNonEmpty(out)...)
	}
	seen := map[string]bool{}
	for _, c := range cands {
		if c == "" || seen[c] || len(seen) > 200 {
			continue
		}
		seen[c] = true
		if !s.git.OK(ctx, "cat-file", "-e", c+"^{commit}") {
			continue
		}
		if mb, _ := s.git.Run(ctx, "merge-base", c, up); mb != "" {
			continue // part of the new history
		}
		if c == head {
			return c
		}
		if mb, _ := s.git.Run(ctx, "merge-base", c, head); mb != "" {
			return c
		}
	}
	return ""
}

func (s *Store) storeNameAt(ctx context.Context, rev string) string {
	data, err := s.git.Run(ctx, "show", rev+":"+s.repoPath(".ka/store.yml"))
	if err != nil {
		return ""
	}
	sf, _ := kaspec.ParseStoreFile(".ka/store.yml", []byte(data+"\n"))
	if sf == nil {
		return ""
	}
	return sf.Name
}

// ticketDirsAt lists the ticket directories (projects/<key>/tickets/<ID>) at rev.
func (s *Store) ticketDirsAt(ctx context.Context, rev string) map[string]bool {
	out := map[string]bool{}
	ls, err := s.git.Run(ctx, "ls-tree", "-r", "--name-only", rev, "--", s.repoPath("projects")+"/")
	if err != nil {
		return out
	}
	for _, p := range splitNonEmpty(ls) {
		sp, ok := s.storePath(p)
		if !ok || !strings.HasSuffix(sp, "/ticket.md") {
			continue
		}
		if parts := strings.Split(sp, "/"); len(parts) == 5 && parts[2] == "tickets" {
			out[strings.TrimSuffix(sp, "/ticket.md")] = true
		}
	}
	return out
}

// adoptLocked takes in a compacted upstream history in place of the old
// one: the branch moves to it, local commits that were never published are
// replayed on top (all or nothing), and the old history is dropped. A
// remote that is not a compaction of this store is refused as before.
func (s *Store) adoptLocked(ctx context.Context, h hooks, res *integrateResult, remote, branch, head, up, prev, mine string) (*integrateResult, error) {
	lastUp := s.lastUpstream(ctx, remote, branch, head, up, prev)
	base, why := s.compactionBase(ctx, head, up, lastUp)
	if base == "" {
		u, _ := s.git.Run(ctx, "remote", "get-url", remote)
		return nil, Errf(CodeGit, "check the store's remote: ka store set-url, or git remote set-url for an attached checkout",
			"%s shares no history with store %s; it is not this store's repository: %s", strings.TrimSpace(u)+" "+branch, s.Name, why)
	}
	var local []gitx.Commit
	if base != head && !s.ReadOnly {
		cs, err := s.git.LogLinear(ctx, base+".."+head)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			// The summary commit of a compaction is in the new tree already.
			if f := c.Trailers[TrailerCompactionSummary]; f != "" && s.git.OK(ctx, "cat-file", "-e", up+":"+s.repoPath(f)) {
				continue
			}
			local = append(local, c)
		}
	}
	backup := "refs/ka/backup/" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if _, err := s.git.Run(ctx, "update-ref", backup, head); err != nil {
		return nil, err
	}
	if _, err := s.git.Run(ctx, "reset", "-q", "--keep", up); err != nil {
		s.git.Run(ctx, "update-ref", "-d", backup)
		return nil, Errf(CodeGit, "commit or stash local edits to the store, then run ka sync", "cannot adopt the compacted history of %s: %s", s.Name, firstLine(err.Error()))
	}
	res.Moved, res.Compacted = true, true
	s.own(head, up)
	h.reloaded(nil) // commits and dates changed everywhere: reload it all
	if len(local) > 0 {
		if err := s.replay(ctx, h, res, local, mine); err != nil {
			return nil, s.abortReplay(ctx, h, head, backup, err)
		}
	} else if mine != "" {
		res.MineSHA = s.git.Head(ctx)
	}
	keep := backup
	if len(res.Preserved) == 0 && len(res.Unpreserved) == 0 && !res.conflicted {
		keep = ""
	}
	res.Warnings = append(res.Warnings, s.dropOldHistory(ctx, keep, head)...)
	return res, nil
}

// ---- reading summaries ------------------------------------------------------------

// HistoryEntry is one summary file.
type HistoryEntry struct {
	Store string `json:"store"`
	Name  string `json:"name" doc:"The file name in history/."`
	Path  string `json:"path" doc:"Store-relative path."`
	kaspec.HistorySummary
}

// HistoryOut lists a store's compactions, oldest first.
type HistoryOut struct {
	Summaries []HistoryEntry `json:"summaries"`
	// Root is the store's oldest commit; when it is a compaction root,
	// Compacted is true and older history lives only in the summaries.
	Root      string `json:"root,omitempty"`
	Compacted bool   `json:"compacted"`
}

// History lists the history summaries of one store ("" for every store),
// without their narratives.
func (s *Service) History(ctx context.Context, store string) (*HistoryOut, error) {
	out := &HistoryOut{Summaries: []HistoryEntry{}}
	found := false
	for _, st := range s.WS.Stores {
		if store != "" && st.Name != store {
			continue
		}
		found = true
		for _, e := range st.readSummaries() {
			e.Body = ""
			out.Summaries = append(out.Summaries, e)
		}
		if store != "" {
			if out.Root = st.rootCommit(ctx); out.Root != "" {
				if c, err := st.git.CommitInfo(ctx, out.Root); err == nil {
					out.Compacted = c.Trailers[TrailerCompactedFrom] != ""
				}
			}
		}
	}
	if !found {
		return nil, Errf(CodeNotFound, "stores: "+strings.Join(s.storeNames(), ", "), "no store %q", store)
	}
	return out, nil
}

// HistorySummary returns one summary file with its narrative.
func (s *Service) HistorySummary(store, name string) (*HistoryEntry, error) {
	name = path.Base(strings.TrimPrefix(name, kaspec.HistoryDir+"/"))
	for _, st := range s.WS.Stores {
		if store != "" && st.Name != store {
			continue
		}
		for _, e := range st.readSummaries() {
			if e.Name == name {
				return &e, nil
			}
		}
	}
	return nil, Errf(CodeNotFound, "list them with ka history", "no history summary %s", name)
}

func (s *Service) storeNames() []string {
	var names []string
	for _, st := range s.WS.Stores {
		names = append(names, st.Name)
	}
	return names
}

// readSummaries reads history/ from the working tree, oldest first.
func (s *Store) readSummaries() []HistoryEntry {
	ents, err := os.ReadDir(filepath.Join(s.Root, kaspec.HistoryDir))
	if err != nil {
		return nil
	}
	var out []HistoryEntry
	for _, e := range ents {
		if !e.Type().IsRegular() || !kaspec.HistoryFileRe.MatchString(e.Name()) {
			continue
		}
		data, err := readRegular(filepath.Join(s.Root, kaspec.HistoryDir, e.Name()))
		if err != nil {
			continue
		}
		h, _ := kaspec.ParseHistorySummary(e.Name(), data)
		if h == nil {
			continue
		}
		h.Body = strings.TrimSpace(h.Body)
		out = append(out, HistoryEntry{Store: s.Name, Name: e.Name(), Path: kaspec.HistoryDir + "/" + e.Name(), HistorySummary: *h})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
