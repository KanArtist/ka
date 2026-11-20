package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// B5: two sessions of the same agent actor (one agent run in two checkouts)
// race for tickets like two agents do: the session whose push loses finds
// the other's claim when its claim is replayed, and moves on. An agent that
// restarted (a new session) can still claim its own ticket again.
func TestClaimRaceSameActorTwoSessions(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	ids := must[*CreateOut](t)(a.Create(ctx, agent("lead"), []CreateIn{
		{Title: "one", Priority: "P0", Status: "ready"},
		{Title: "two", Priority: "P1", Status: "ready"},
	}))
	one, two := ids.Created[0].ID, ids.Created[1].ID
	b, _ := e.clone()
	sess := func(s string) Caller { return Caller{Actor: "agent/claude-code", OnBehalfOf: "tester", Session: s} }

	na := must[*NextOut](t)(a.Next(ctx, sess("S1"), NextIn{}))
	nb := must[*NextOut](t)(b.Next(ctx, sess("S2"), NextIn{}))
	if na.Ticket.ID != one || !na.Confirmed || nb.Ticket.ID != two || !nb.Confirmed {
		t.Fatalf("a got %s, b got %s; want %s and %s", na.Ticket.ID, nb.Ticket.ID, one, two)
	}
	// A restarted agent (new session) takes its own ticket over.
	co, err := a.Claim(ctx, sess("S3"), one, false)
	if err != nil || !co.Confirmed {
		t.Fatalf("claim from a new session: %+v %v", co, err)
	}
	// Another actor still cannot.
	if _, err := b.Claim(ctx, agent("other"), one, false); !IsCode(err, CodeClaimLost) {
		t.Fatalf("claim of a held ticket by another actor: %v", err)
	}

	// Simultaneous claims of one ticket from both checkouts: exactly one
	// wins, whichever way the remote rejects the other push (a plain
	// rejection, or git 2.53's "incorrect old value provided").
	var rest []string
	for _, c := range must[*CreateOut](t)(a.Create(ctx, agent("lead"), []CreateIn{{Title: "r1"}, {Title: "r2"}, {Title: "r3"}, {Title: "r4"}})).Created {
		rest = append(rest, c.ID)
	}
	b.Sync(ctx, true)
	for _, id := range rest {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, svc := range []*Service{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				co, err := svc.Claim(ctx, sess("S"+string(rune('A'+i))), id, false)
				if err == nil && !co.Confirmed {
					err = errors.New("unconfirmed: " + co.PushError)
				}
				errs[i] = err
			}()
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case !IsCode(err, CodeClaimLost):
				t.Fatalf("%s: %v", id, err)
			}
		}
		if won != 1 {
			t.Fatalf("%s: %d sessions won the claim (%v)", id, won, errs)
		}
	}
}

// claimedAgainst: the session rule, case by case.
func TestClaimedAgainst(t *testing.T) {
	held := &kaspec.Claim{By: "agent/x", Session: "S1"}
	for _, tc := range []struct {
		actor, session, takeover string
		replayed, want           bool
	}{
		{"agent/y", "S1", "", false, true},
		{"agent/x", "S1", "", true, false},
		{"agent/x", "S2", "", false, false}, // a restart: same actor, new session
		{"agent/x", "S2", "", true, true},   // lost the race to another session
		{"agent/x", "", "", true, false},
		{"agent/x", "S2", "S1", true, false}, // took S1's claim over knowingly
	} {
		o := &Op{Actor: tc.actor, Session: tc.session, Takeover: tc.takeover, replayed: tc.replayed}
		if got := claimedAgainst(held, o); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
	if claimedAgainst(&kaspec.Claim{By: "agent/x"}, &Op{Actor: "agent/x", Session: "S2", replayed: true}) {
		t.Error("a claim without a session keeps the actor-only rule")
	}
	o, err := DecodeOp((&Op{Op: "claim", ID: "JK-AAAAA"}).Encode())
	if err != nil || !o.replayed {
		t.Fatalf("an op decoded from a commit is a replay: %+v %v", o, err)
	}
}

// B19: release cannot close a ticket (closing needs a resolution summary),
// neither through the service nor as a replayed operation.
func TestReleaseCannotClose(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	c := agent("a")
	id := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "x", Status: "ready"}})).Created[0].ID
	must[*ClaimOut](t)(a.Claim(ctx, c, id, false))
	if _, err := a.Release(ctx, c, id, "done", false); !IsCode(err, CodeResolutionRequired) {
		t.Fatalf("release to done: %v", err)
	}
	o := stamp(&Op{Op: "release", ID: id, Status: "done"}, c, a.now())
	if _, err := a.do(ctx, a.WS.Stores[0], o, false); !IsCode(err, CodeResolutionRequired) {
		t.Fatalf("release op to done: %v", err)
	}
	if d := must[*Detail](t)(a.Get(GetIn{ID: id})); d.Status != "in_progress" || d.Claim == nil {
		t.Fatalf("ticket changed: %+v", d.Summary)
	}
	if w := must[*WriteOut](t)(a.Release(ctx, c, id, "", false)); w.Status != "ready" {
		t.Fatalf("plain release: %+v", w)
	}
}

// B21: Answer takes an open question by exact name or a unique prefix, never
// an answered one or another kind of comment, and a choice on a question
// without options is an error, not a panic.
func TestAnswerPicksOpenQuestion(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	c, human := agent("a"), Caller{Actor: "tester"}
	id := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "x"}})).Created[0].ID
	note := must[*WriteOut](t)(a.Comment(ctx, c, CommentIn{ID: id, Body: "a note"})).Comment
	q1 := must[*AskOut](t)(a.Ask(ctx, c, AskIn{ID: id, Question: "free text?"})).Question
	q2 := must[*AskOut](t)(a.Ask(ctx, c, AskIn{ID: id, Question: "pick", Options: []string{"a", "b"}})).Question

	zero := 0
	if _, err := a.Answer(ctx, human, AnswerIn{ID: id, Question: note, Text: "hi"}); !IsCode(err, CodeNoQuestion) {
		t.Fatalf("answering a note: %v", err)
	}
	if _, err := a.Answer(ctx, human, AnswerIn{ID: id, Question: "2", Text: "hi"}); !IsCode(err, CodeNoQuestion) {
		t.Fatalf("an ambiguous prefix: %v", err)
	}
	if _, err := a.Answer(ctx, human, AnswerIn{ID: id, Question: q1, Choice: &zero}); !IsCode(err, CodeInvalid) {
		t.Fatalf("a choice on a question without options: %v", err)
	}
	must[*WriteOut](t)(a.Answer(ctx, human, AnswerIn{ID: id, Question: q2[:len(q2)-4], Choice: &zero}))
	if _, err := a.Answer(ctx, human, AnswerIn{ID: id, Question: q2, Choice: &zero}); !IsCode(err, CodeNoQuestion) {
		t.Fatalf("answering an answered question: %v", err)
	}
	must[*WriteOut](t)(a.Answer(ctx, human, AnswerIn{ID: id, Question: q1, Text: "sure"}))
}

// S1: an out-of-range id_suffix_length is clamped, and a project whose IDs
// can never be valid fails instead of spinning.
func TestNewIDIsBounded(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	pi := a.ix.projects["jk"]
	pi.Cfg.IDSuffixLength = 9
	id, err := a.newID(pi)
	if _, sfx, ok := kaspec.ParseID(id); err != nil || !ok || len(sfx) != 8 {
		t.Fatalf("newID with length 9: %q %v", id, err)
	}
	pi.Prefix = "jk" // never a valid ID
	if _, err := a.newID(pi); err == nil {
		t.Fatal("newID with an invalid prefix succeeded")
	}
}

// B20: per-store batch results; a no-change write still knows the store has
// a remote; batch errors count from 1.
func TestWriteResults(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	c := agent("a")
	id := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "same"}})).Created[0].ID
	ws := must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: id, Set: map[string]any{"title": "same"}}}))
	if !ws[0].NoChange || ws[0].Local {
		t.Fatalf("no-change update: %+v", ws[0])
	}
	out := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "ok"}, {Title: ""}}))
	if out.Error == nil || !strings.HasPrefix(out.Error.Message, "ticket 2: ") {
		t.Fatalf("batch error: %+v", out.Error)
	}
}

// B20, D5: an editor save handles the slug like ka_update does.
func TestReplaceTicketSlug(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	c := agent("a")
	id := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Speed up the build"}})).Created[0].ID
	p := filepath.Join(dir, "projects", "jk", "tickets", id, "ticket.md")
	data, _ := os.ReadFile(p)
	edited := strings.Replace(string(data), "title: Speed up the build\n", "title: Speed up the build\nslug: faster\n", 1)
	if _, err := a.ReplaceTicket(ctx, c, id, []byte(edited), ""); err == nil || !strings.Contains(err.Error(), "slugs") {
		t.Fatalf("a slug edit with slugs off: %v", err)
	}
	os.WriteFile(filepath.Join(dir, ".ka", "config.yml"), []byte("slugs: true\n"), 0o644)
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "slugs on")
	a = e.open(dir)
	must[*WriteOut](t)(a.ReplaceTicket(ctx, c, id, []byte(edited), ""))
	if d := must[*Detail](t)(a.Get(GetIn{ID: id})); d.Ref != id+"-faster" {
		t.Fatalf("ref after the edit: %s", d.Ref)
	}
}

// B22: a ticket in a read-only store is shown by a peek but never offered
// for claiming.
func TestNextSkipsReadOnlyStores(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "x", Status: "ready"}})).Created[0].ID
	a.WS.Stores[0].ReadOnly = true
	if _, err := a.Next(ctx, agent("a"), NextIn{}); !IsCode(err, CodeNoCandidates) {
		t.Fatalf("next in a read-only store: %v", err)
	}
	if n := must[*NextOut](t)(a.Next(ctx, agent("a"), NextIn{Peek: true})); n.Ticket == nil || n.Ticket.ID != id || n.Claimed {
		t.Fatalf("peek: %+v", n)
	}
}

// B22: relations written with a former project prefix are stored with the
// current one.
func TestFormerPrefixInRelations(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	c := agent("a")
	ids := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "parent"}, {Title: "child"}}))
	parent, child := ids.Created[0].ID, ids.Created[1].ID
	if _, err := a.RenameProject(ctx, c, "jk", "", "JKX", true, false); err != nil {
		t.Fatal(err)
	}
	newParent, newChild := "JKX"+parent[2:], "JKX"+child[2:]
	must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: child, Set: map[string]any{"parent": parent}, Add: map[string][]string{"related": {parent}}}}))
	d := must[*Detail](t)(a.Get(GetIn{ID: newChild}))
	if d.ParentRef == nil || d.ParentRef.ID != newParent || len(d.Related) != 1 || d.Related[0] != newParent {
		t.Fatalf("relations: %+v %v", d.ParentRef, d.Related)
	}
	cr := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "blocked", BlockedBy: []string{child}, Parent: parent}}))
	d = must[*Detail](t)(a.Get(GetIn{ID: cr.Created[0].ID}))
	if len(d.BlockedBy) != 1 || d.BlockedBy[0].ID != newChild || d.ParentRef.ID != newParent {
		t.Fatalf("created relations: %+v %+v", d.BlockedBy, d.ParentRef)
	}
}

func TestRewriteProjectYAML(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"key: jk\nprefix: JK\nformer_prefixes: [OLD]\n", "key: jk\nprefix: JKX\nformer_prefixes: [OLD, JK]\n"},
		{"key: jk\nprefix: JK\nformer_prefixes:\n  - OLD\n", "key: jk\nprefix: JKX\nformer_prefixes:\n  - OLD\n  - JK\n"},
		{"key: jk\nprefix: JK\n", "key: jk\nprefix: JKX\nformer_prefixes:\n  - JK\n"},
	} {
		got := rewriteProjectYAML([]byte(tc.in), "jk", "JKX", "JK", true)
		if string(got) != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
		if again := rewriteProjectYAML(got, "jk", "JKX", "JK", true); string(again) != string(got) {
			t.Errorf("not idempotent: %q", again)
		}
	}
}

// B22: commit subjects are one line and never cut inside a character.
func TestSubjectPart(t *testing.T) {
	if got := subjectPart("a\nb\r\nc", 120); got != "a b c" {
		t.Fatalf("newlines: %q", got)
	}
	long := strings.Repeat("é", 100)
	if got := subjectPart(long, 120); !utf8.ValidString(got) || len(got) > 120 || !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated: %q", got)
	}
}

// B22: KA_HUMAN must be a valid handle to count.
func TestKAHumanIsValidated(t *testing.T) {
	e := newEnv(t, nil)
	_, dir := e.clone()
	t.Setenv("KA_HUMAN", "Not A Handle")
	if a := e.open(dir); a.Human() == "Not A Handle" {
		t.Fatal("an invalid KA_HUMAN was used")
	}
	t.Setenv("KA_HUMAN", "someone")
	if a := e.open(dir); a.Human() != "someone" {
		t.Fatalf("KA_HUMAN: %q", a.Human())
	}
}

// B22: claim events name the actor.
func TestClaimEventsCarryActor(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "x", Status: "ready"}})).Created[0].ID
	hub := NewEventHub(100)
	a.SetEvents(hub)
	seq := hub.Seq()
	must[*ClaimOut](t)(a.Claim(ctx, agent("b"), id, false))
	evs, _, _ := hub.Since(seq)
	found := false
	for _, ev := range evs {
		if ev.Type == EvClaimChanged {
			found = true
			if ev.Actor != "agent/b" {
				t.Fatalf("claim event actor %q", ev.Actor)
			}
		}
	}
	if !found {
		t.Fatalf("no claim event in %+v", evs)
	}
}

// P1b: waiting for an answer never pushes a push: manual store.
func TestWaitAnswerDoesNotPushManualStore(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	st := a.WS.Stores[0]
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "x"}})).Created[0].ID
	remoteHead := run(t, e.root, "--git-dir", e.remote, "rev-parse", "main")
	st.PushMode = "manual"
	out := must[*AskOut](t)(a.Ask(ctx, agent("a"), AskIn{ID: id, Question: "?", Wait: 150 * time.Millisecond}))
	if out.Answered || out.Pushed {
		t.Fatalf("ask: %+v", out)
	}
	if h := run(t, e.root, "--git-dir", e.remote, "rev-parse", "main"); h != remoteHead {
		t.Fatal("the manual store was pushed while waiting")
	}
}

// B4: a board commit takes only the board; nothing is written mid-merge.
func TestBoardCommitTakesOnlyItsFile(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	writeFile(t, filepath.Join(dir, "staged.txt"), "mine\n")
	run(t, dir, "add", "staged.txt")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "x\n")
	me := a.CallerFor("tester", "")
	if _, err := a.SaveBoard(ctx, me, BoardIn{Store: "test", Key: "b", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	if files := commitFiles(t, dir, "HEAD"); strings.Join(files, " ") != "boards/b.yml" {
		t.Fatalf("board commit took %v", files)
	}
	if st := run(t, dir, "status", "--porcelain"); st != "A  staged.txt\n?? untracked.txt" {
		t.Fatalf("status: %q", st)
	}
	writeFile(t, filepath.Join(dir, ".git", "MERGE_HEAD"), run(t, dir, "rev-parse", "HEAD")+"\n")
	if _, err := a.SaveBoard(ctx, me, BoardIn{Store: "test", Key: "c", Title: "C"}); !IsCode(err, CodeGitBusy) {
		t.Fatalf("board mid-merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "boards", "c.yml")); err == nil {
		t.Fatal("the board was written mid-merge")
	}
}

// B3: ka fix leaves a ticket that does not parse cleanly exactly as it is,
// maps an alias that is not a valid actor, fills timestamps from history,
// and commits only what it fixed.
func TestFixNeverDropsData(t *testing.T) {
	e := newEnv(t, map[string]string{
		".ka/actors.yml":                             "humans:\n  tester:\n    name: Test User\n    aliases: [Test User]\n",
		"projects/jk/tickets/JK-BAD01/ticket.md":     "---\nid: JK-BAD01\ntitle: Bad\nstatus: ready\npriority: high\nassignee: Test User\nestimate: 3\ncreated: 2026-01-01T00:00:00Z\nupdated: 2026-01-01T00:00:00Z\n---\n",
		"projects/jk/tickets/JK-AKA01/ticket.md":     "---\nid: JK-AKA01\ntitle: Alias\nstatus: ready\nassignee: Test User\ncreated: 2026-01-01T00:00:00Z\nupdated: 2026-01-01T00:00:00Z\n---\n",
		"projects/jk/tickets/JK-TSA01/ticket.md":     "---\nid: JK-TSA01\ntitle: No times\nstatus: ready\n---\n",
		"projects/jk/tickets/JK-CRR01/ticket.md":     "---\r\ntitle: CRLF\r\nid: JK-CRR01\r\nstatus: ready\r\ncreated: 2026-01-01T00:00:00Z\r\nupdated: 2026-01-01T00:00:00Z\r\n---\r\n",
		"projects/jk/tickets/JK-TSA01/comments/x.md": "not a comment",
	})
	a, dir := e.clone()
	bad := filepath.Join(dir, "projects", "jk", "tickets", "JK-BAD01", "ticket.md")
	before, _ := os.ReadFile(bad)
	writeFile(t, filepath.Join(dir, "staged.txt"), "mine\n")
	run(t, dir, "add", "staged.txt")

	out := must[*FixOut](t)(a.Fix(ctx, a.CallerFor("tester", ""), false))
	if after, _ := os.ReadFile(bad); string(after) != string(before) {
		t.Fatalf("a ticket with errors was rewritten:\n%s", after)
	}
	got := map[string]string{}
	for _, c := range out.Categories {
		got[c.Name] = strings.Join(c.Files, " ")
	}
	if !strings.Contains(got["actors"], "JK-AKA01") || !strings.Contains(got["timestamps"], "JK-TSA01") || !strings.Contains(got["format"], "JK-CRR01") || strings.Contains(got["actors"]+got["format"], "JK-BAD01") {
		t.Fatalf("categories: %v", got)
	}
	ali, _ := os.ReadFile(filepath.Join(dir, "projects", "jk", "tickets", "JK-AKA01", "ticket.md"))
	if !strings.Contains(string(ali), "assignee: tester\n") {
		t.Fatalf("alias not mapped:\n%s", ali)
	}
	tsa, _ := os.ReadFile(filepath.Join(dir, "projects", "jk", "tickets", "JK-TSA01", "ticket.md"))
	if !strings.Contains(string(tsa), "created: ") || !strings.Contains(string(tsa), "updated: ") {
		t.Fatalf("timestamps not filled:\n%s", tsa)
	}
	if st := run(t, dir, "status", "--porcelain"); st != "A  staged.txt" {
		t.Fatalf("status after fix: %q", st)
	}
	if log := run(t, dir, "log", "--format=%B", "-1"); !strings.Contains(log, "Ka-Actor: tester") {
		t.Fatalf("trailers: %s", log)
	}
}

// S2: links are never followed: not listed or read as docs, not rewritten
// by ka fix, and a project that contains one is not moved or renamed.
func TestLinksAreNotFollowed(t *testing.T) {
	e := newEnv(t, map[string]string{"projects/jk/docs/real.md": "# Real\n"})
	a, dir := e.clone()
	outside := filepath.Join(e.root, "secret.md")
	writeFile(t, outside, "---\nid: JK-SEC01\ntitle: secret\nstatus: ready\ncreated: 2026-01-01T00:00:00Z\nupdated: 2026-01-01T00:00:00Z\n---\n")
	if err := os.Symlink(outside, filepath.Join(dir, "projects", "jk", "docs", "link.md")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	link := filepath.Join(dir, "projects", "jk", "tickets", "JK-SEC01", "ticket.md")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	a.Reload()
	list := must[*DocsOut](t)(a.Docs("jk"))
	if len(list.Docs) != 1 || list.Docs[0].Path != "real.md" {
		t.Fatalf("docs: %+v", list.Docs)
	}
	if _, err := a.Doc("jk", "link.md"); !IsCode(err, CodeNotFound) {
		t.Fatalf("doc through a link: %v", err)
	}
	if _, err := a.Fix(ctx, a.CallerFor("tester", ""), false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(outside); !strings.HasPrefix(string(data), "---\nid: JK-SEC01\ntitle: secret\nstatus: ready\ncreated") {
		t.Fatalf("the link target changed:\n%s", data)
	}
	if _, err := a.RenameProject(ctx, a.CallerFor("tester", ""), "jk", "jkx", "", false, false); !IsCode(err, CodeInvalid) {
		t.Fatalf("rename of a project with a link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "projects", "jk", "project.yml")); err != nil {
		t.Fatal("the failed rename left the project moved")
	}
}

// Doc truncates a large text doc at a character boundary, so it stays text.
func TestDocTruncatesAtRune(t *testing.T) {
	e := newEnv(t, map[string]string{"projects/jk/docs/big.md": "x" + strings.Repeat("é", maxDoc/2+10)})
	a, _ := e.clone()
	d := must[*DocOut](t)(a.Doc("jk", "big.md"))
	if !d.Truncated || !d.Text || len(d.Body) > maxDoc {
		t.Fatalf("doc: truncated=%v text=%v len=%d", d.Truncated, d.Text, len(d.Body))
	}
}

// Service.Sync reports what could not be preserved, in its result and its
// event.
func TestSyncReportsUnpreserved(t *testing.T) {
	e := newEnv(t, nil)
	a, adir := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "doomed", Status: "ready"}})).Created[0].ID
	b, _ := e.clone()
	b.NoPush = true
	must[[]*WriteOut](t)(b.Update(ctx, agent("b"), []UpdateIn{{ID: id, Set: map[string]any{"title": "edited"}}}))
	run(t, adir, "rm", "-rq", "projects/jk/tickets/"+id)
	run(t, adir, "commit", "-qm", "delete by hand")
	run(t, adir, "push", "-q")

	hub := NewEventHub(100)
	b.SetEvents(hub)
	seq := hub.Seq()
	out := b.Sync(ctx, true)
	if len(out.Unpreserved) != 1 || !strings.Contains(out.Unpreserved[0], id) {
		t.Fatalf("sync: %+v", out)
	}
	evs, _, _ := hub.Since(seq)
	for _, ev := range evs {
		if m, ok := ev.Data.(map[string]any); ok && ev.Type == EvConflictPreserved && m["unpreserved"] != nil {
			return
		}
	}
	t.Fatalf("no unpreserved event in %+v", evs)
}
