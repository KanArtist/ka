package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

var ctx = context.Background()

func TestCreateClaimCloseFlow(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	c := agent("claude")

	out := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{
		{Title: "Blocker", Priority: "P1", Kind: "bug", Status: "ready", Acceptance: []string{"it works", "tests pass"}},
	}))
	if len(out.Created) != 1 || !out.Pushed {
		t.Fatalf("create: %+v", out)
	}
	blocker := out.Created[0].ID
	out2 := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{
		{Title: "Blocked work", Priority: "P0", Status: "ready", BlockedBy: []string{blocker}},
	}))
	blocked := out2.Created[0].ID

	// The P0 ticket is blocked, so next picks the P1 blocker.
	n := must[*NextOut](t)(a.Next(ctx, c, NextIn{}))
	if n.Ticket.ID != blocker || !n.Claimed || !n.Confirmed {
		t.Fatalf("next: %+v", n)
	}
	if n.Ticket.Status != "in_progress" || n.Ticket.Claim == nil || n.Ticket.Claim.By != "agent/claude" {
		t.Fatalf("claimed ticket: %+v", n.Ticket.Summary)
	}

	w, prog := must2(t)(a.CheckItems(ctx, c, CheckIn{ID: blocker, Items: []CheckItem{{Text: "it works", Done: true}}}))
	if prog != "1/2" || w.ID != blocker {
		t.Fatalf("check: %v %+v", prog, w)
	}

	if _, err := a.Transition(ctx, c, TransitionIn{ID: blocker, Status: "done"}); !IsCode(err, CodeResolutionRequired) {
		t.Fatalf("closing without summary: %v", err)
	}
	tr := must[*WriteOut](t)(a.Transition(ctx, c, TransitionIn{ID: blocker, Status: "done", Summary: "Fixed it."}))
	if len(tr.Unblocked) != 1 || tr.Unblocked[0] != blocked {
		t.Fatalf("unblocked: %+v", tr)
	}
	d := must[*Detail](t)(a.Get(GetIn{ID: blocker, Include: []string{"comments"}}))
	if d.Claim != nil || len(d.Comments) != 1 || d.Comments[0].Type != "resolution" || d.Comments[0].Resolution != "done" {
		t.Fatalf("closed ticket: %+v", d)
	}
	// Dependents now show up in next.
	n = must[*NextOut](t)(a.Next(ctx, c, NextIn{Peek: true}))
	if n.Ticket.ID != blocked {
		t.Fatalf("next after close: %+v", n.Ticket.ID)
	}

	// The commit carries replayable trailers.
	log := run(t, dir, "log", "-1", "--format=%B", "HEAD~1")
	if !strings.Contains(log, "Ka-Op: check ") || !strings.Contains(log, "Ka-Actor: agent/claude") {
		t.Fatalf("trailers:\n%s", log)
	}
	// And everything validates.
	for _, d := range a.Validate(time.Now()) {
		if d.Severity != kaspec.SevInfo {
			t.Errorf("check: %s", d)
		}
	}
}

func must2(t *testing.T) func(*WriteOut, string, error) (*WriteOut, string) {
	return func(w *WriteOut, s string, err error) (*WriteOut, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return w, s
	}
}

func TestConcurrentClaimAcrossClones(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	ids := must[*CreateOut](t)(a.Create(ctx, agent("lead"), []CreateIn{
		{Title: "one", Priority: "P0", Status: "ready"},
		{Title: "two", Priority: "P1", Status: "ready"},
	}))
	b, _ := e.clone()
	c, _ := e.clone()

	// Both clones see the same top ticket; whoever pushes second must
	// replay, lose the claim and move on to the next ticket.
	nb := must[*NextOut](t)(b.Next(ctx, agent("b"), NextIn{}))
	nc := must[*NextOut](t)(c.Next(ctx, agent("c"), NextIn{}))
	if nb.Ticket.ID == nc.Ticket.ID {
		t.Fatalf("both agents got %s", nb.Ticket.ID)
	}
	if nb.Ticket.ID != ids.Created[0].ID || nc.Ticket.ID != ids.Created[1].ID {
		t.Fatalf("unexpected order: b=%s c=%s", nb.Ticket.ID, nc.Ticket.ID)
	}
	// An explicit claim of a held ticket reports the holder.
	_, err := c.Claim(ctx, agent("c"), ids.Created[0].ID, false)
	if !IsCode(err, CodeClaimLost) || AsError(err).Data["holder"] != "agent/b" {
		t.Fatalf("claim of held ticket: %v", err)
	}
}

func TestReplayRenamesCollidingID(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, _ := e.clone()
	ca, cb := agent("a"), agent("b")

	// Force both clones to create the same ID.
	mk := func(s *Service, c Caller, title string) *Op {
		now := s.now()
		return stamp(&Op{Op: "create", ID: "JK-ZZZZ9", New: &kaspec.Ticket{Title: title, Status: "ready", Created: now, Updated: now}}, c, now)
	}
	sa := a.WS.Stores[0]
	if _, err := a.do(ctx, sa, mk(a, ca, "from a"), true); err != nil {
		t.Fatal(err)
	}
	sb := b.WS.Stores[0]
	res, err := b.do(ctx, sb, mk(b, cb, "from b"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || res.Renamed["JK-ZZZZ9"] == "" {
		t.Fatalf("expected a rename: %+v", res)
	}
	b.Reload()
	if d, _ := b.Get(GetIn{ID: "JK-ZZZZ9"}); d.Title != "from a" {
		t.Fatalf("upstream ticket changed: %+v", d)
	}
	if d, _ := b.Get(GetIn{ID: res.Renamed["JK-ZZZZ9"]}); d == nil || d.Title != "from b" {
		t.Fatalf("renamed ticket missing")
	}
}

func TestConcurrentFieldEditsBothApply(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "shared", Status: "ready", Labels: []string{"x"}}})).Created[0].ID
	b, _ := e.clone()
	// a pushes a label and priority; b (stale) adds another label and changes kind.
	must[[]*WriteOut](t)(a.Update(ctx, agent("a"), []UpdateIn{{ID: id, Set: map[string]any{"priority": "P1"}, Add: map[string][]string{"labels": {"a"}}}}))
	must[[]*WriteOut](t)(b.Update(ctx, agent("b"), []UpdateIn{{ID: id, Set: map[string]any{"kind": "bug"}, Add: map[string][]string{"labels": {"b"}}}}))
	c, _ := e.clone()
	d := must[*Detail](t)(c.Get(GetIn{ID: id}))
	if d.Priority != "P1" || d.Kind != "bug" || strings.Join(d.Labels, ",") != "a,b,x" {
		t.Fatalf("lost update: %+v", d.Summary)
	}
}

func TestHandEditConflictIsPreserved(t *testing.T) {
	e := newEnv(t, nil)
	MergeDriverCommand = ""
	a, _ := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "original", Status: "ready"}})).Created[0].ID
	b, bdir := e.clone()
	must[[]*WriteOut](t)(a.Update(ctx, agent("a"), []UpdateIn{{ID: id, Set: map[string]any{"title": "upstream title"}}}))

	// A hand edit in clone b changes the same line, committed without ka.
	p := filepath.Join(bdir, "projects", "jk", "tickets", id, "ticket.md")
	data, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(data), "title: original", "title: hand title", 1)), 0o644)
	run(t, bdir, "commit", "-qam", "hand edit")

	out := b.Sync(ctx, true)
	if len(out.Errors) > 0 {
		t.Fatalf("sync: %+v", out.Errors[0])
	}
	if len(out.Preserved) != 1 {
		t.Fatalf("expected one preserved conflict, got %+v", out)
	}
	d := must[*Detail](t)(b.Get(GetIn{ID: id, Include: []string{"comments"}}))
	if d.Title != "upstream title" || len(d.Comments) != 1 || !strings.Contains(d.Comments[0].Body, "hand title") {
		t.Fatalf("preserved: %+v", d)
	}
	if st := b.WS.Stores[0].State(ctx); st.State != "synced" {
		t.Fatalf("state: %+v", st)
	}
}

func TestMergeTickets(t *testing.T) {
	base := "---\nid: JK-7QK3M\ntitle: t\nstatus: ready\nlabels:\n  - a\nacceptance:\n  - text: one\n    done: false\n  - text: two\n    done: false\ncreated: 2026-11-07T12:00:00Z\nupdated: 2026-11-07T12:00:00Z\n---\n\nline 1\n\nline 2\n"
	up := strings.NewReplacer("status: ready", "status: review", "  - a\n", "  - a\n  - up\n", "updated: 2026-11-07T12:00:00Z", "updated: 2026-11-07T13:00:00Z", "line 1", "line 1 up").Replace(base)
	local := strings.NewReplacer("status: ready", "status: blocked", "  - a\n", "  - local\n", "text: two\n    done: false", "text: two\n    done: true", "updated: 2026-11-07T12:00:00Z", "updated: 2026-11-07T14:00:00Z", "line 2", "line 2 local").Replace(base)
	merged, pres, ok := MergeTickets(ctx, []byte(base), []byte(up), []byte(local))
	if !ok {
		t.Fatal("not merged")
	}
	m, _ := kaspec.ParseTicket("m", merged)
	if m.Status != "review" || strings.Join(m.Labels, ",") != "local,up" || !m.Acceptance[1].Done || m.Updated != "2026-11-07T14:00:00Z" {
		t.Fatalf("merged:\n%s", merged)
	}
	if !strings.Contains(m.Body, "line 1 up") || !strings.Contains(m.Body, "line 2 local") {
		t.Fatalf("body:\n%s", m.Body)
	}
	if len(pres) != 1 || pres[0].Field != "status" || pres[0].Lost != "blocked" {
		t.Fatalf("preserved: %+v", pres)
	}
}

func TestQuery(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{
		{Title: "Flaky cache test", Kind: "bug", Labels: []string{"cache"}, Priority: "P1", Status: "ready"},
		{Title: "Infra thing", Kind: "chore", Labels: []string{"infra"}, Status: "backlog"},
		{Title: "Done thing", Status: "ready"},
	}))
	for q, want := range map[string]int{
		"":                           3,
		"kind:bug":                   1,
		"-label:infra":               2,
		"flaky":                      1,
		"label:cache,infra":          2,
		"priority:none":              2,
		"category:unstarted":         2,
		"is:ready":                   2,
		"created_by:agent/*":         3,
		"\"cache test\" -kind:chore": 1,
	} {
		out, err := a.Search(SearchIn{Query: q})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if out.Total != want {
			t.Errorf("%q: got %d, want %d", q, out.Total, want)
		}
	}
	if _, err := a.Search(SearchIn{Query: "lable:x"}); !IsCode(err, CodeInvalid) || !strings.Contains(AsError(err).Fix, "label") {
		t.Errorf("typo key: %v", err)
	}
}

func TestValidationErrorsAreActionable(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	_, err := a.Create(ctx, agent("a"), []CreateIn{{Title: "x", Status: "doing"}})
	if !IsCode(err, CodeUnknownStatus) || !strings.Contains(AsError(err).Fix, "in_progress") {
		t.Fatalf("status: %v", err)
	}
	_, err = a.Create(ctx, agent("a"), []CreateIn{{Title: "x", Kind: "bgu"}})
	if !IsCode(err, CodeUnknownKind) || !strings.Contains(AsError(err).Fix, "did you mean bug") {
		t.Fatalf("kind: %v", err)
	}
	_, err = a.Create(ctx, agent("a"), []CreateIn{{Title: "x", BlockedBy: []string{"JK-ZZZZZ"}}})
	if !IsCode(err, CodeDanglingRef) {
		t.Fatalf("ref: %v", err)
	}
	_, err = a.Create(ctx, agent("a"), []CreateIn{{Title: "x", Fields: map[string]any{"estimate": "M"}}})
	if !IsCode(err, CodeUnknownField) {
		t.Fatalf("field: %v", err)
	}
}

func TestProgressThrottle(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "x"}})).Created[0].ID
	must[*WriteOut](t)(a.Comment(ctx, agent("a"), CommentIn{ID: id, Type: "progress", Body: "step 1"}))
	now = now.Add(30 * time.Second)
	_, err := a.Comment(ctx, agent("a"), CommentIn{ID: id, Type: "progress", Body: "step 2"})
	if !IsCode(err, CodeThrottled) || AsError(err).Data["retry_after_s"] != 90 {
		t.Fatalf("throttle: %v", err)
	}
	// Other agents and other types are not throttled.
	must[*WriteOut](t)(a.Comment(ctx, agent("b"), CommentIn{ID: id, Type: "progress", Body: "b's step"}))
	must[*WriteOut](t)(a.Comment(ctx, agent("a"), CommentIn{ID: id, Type: "note", Body: "a note"}))
	now = now.Add(2 * time.Minute)
	must[*WriteOut](t)(a.Comment(ctx, agent("a"), CommentIn{ID: id, Type: "progress", Body: "step 2"}))
}

func TestAskAndAnswerAcrossClones(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, _ := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("a"), []CreateIn{{Title: "fork in the road"}})).Created[0].ID
	b.Sync(ctx, true)

	done := make(chan *AskOut)
	go func() {
		out, err := a.Ask(ctx, agent("a"), AskIn{ID: id, Question: "Left or right?", Options: []string{"left", "right"}, Wait: 20 * time.Second})
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	// The human, in another clone, sees it in their inbox and answers.
	human := Caller{Actor: "tester"}
	var q []InboxItem
	for i := 0; i < 200 && len(q) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		b.Sync(ctx, true)
		q = b.Inbox(human, "").Questions
	}
	if len(q) != 1 || q[0].Comment.To != "tester" {
		t.Fatalf("inbox: %+v", q)
	}
	one := 1
	must[*WriteOut](t)(b.Answer(ctx, human, AnswerIn{ID: id, Choice: &one}))
	out := <-done
	if out == nil || !out.Answered || out.Answer.Choice == nil || *out.Answer.Choice != 1 {
		t.Fatalf("ask: %+v", out)
	}
	if in := a.Inbox(agent("a"), ""); len(in.Answers) != 1 {
		t.Fatalf("agent inbox: %+v", in)
	}
}

func TestSaveAndDeleteBoard(t *testing.T) {
	e := newEnv(t, nil)
	svc, dir := e.clone()
	me := svc.CallerFor("tester", "")
	b, err := svc.SaveBoard(ctx, me, BoardIn{Store: svc.WS.Stores[0].Name, Key: "agents-wip", Title: "Agents: work in progress",
		Filter: "is:started assignee:agent/*", Swimlanes: "assignee", Columns: []string{"in_progress", "review"}, CardFields: []string{"priority"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "test/agents-wip" || b.Title != "Agents: work in progress" || len(b.Columns) != 2 {
		t.Fatalf("saved: %+v", b)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "boards", "agents-wip.yml"))
	want := "name: \"Agents: work in progress\"\nfilter: is:started assignee:agent/*\nswimlanes: assignee\ncolumns:\n  - in_progress\n  - review\ncard_fields:\n  - priority\n"
	if string(data) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", data, want)
	}
	if f, ok := svc.BoardFilter("test/agents-wip"); !ok || f != "is:started assignee:agent/*" {
		t.Fatalf("BoardFilter: %q %v", f, ok)
	}
	// Pushed: a fresh clone has it.
	other, _ := e.clone()
	if len(other.Boards()) != 1 {
		t.Fatalf("fresh clone boards: %+v", other.Boards())
	}
	if _, err := svc.SaveBoard(ctx, me, BoardIn{Store: "test", Key: "Bad Key", Title: "x"}); err == nil {
		t.Fatal("invalid key accepted")
	}
	if _, err := svc.SaveBoard(ctx, me, BoardIn{Store: "test", Key: "q", Title: "x", Filter: "nosuchkey:1"}); err == nil {
		t.Fatal("invalid filter accepted")
	}
	if err := svc.DeleteBoard(ctx, me, "agents-wip"); err != nil {
		t.Fatal(err)
	}
	if len(svc.Boards()) != 0 {
		t.Fatal("board still listed after delete")
	}
	log := run(t, dir, "log", "--format=%s%n%b", "-2")
	if !strings.Contains(log, "ka: remove board agents-wip") || !strings.Contains(log, "Ka-Actor: tester") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestDocsAndBacklinks(t *testing.T) {
	e := newEnv(t, map[string]string{
		"projects/jk/docs/design/cache.md": "Intro line\n\n# Cache design\n\nBody.\n",
		"projects/jk/docs/notes.txt":       "plain\n",
	})
	svc, _ := e.clone()
	me := svc.CallerFor("tester", "")
	out, err := svc.Create(ctx, me, []CreateIn{
		{Title: "Implement the cache", Body: "See projects/jk/docs/design/cache.md for the design."},
		{Title: "Unrelated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	other := out.Created[1].ID
	if _, err := svc.Comment(ctx, me, CommentIn{ID: other, Body: "Also relevant: projects/jk/docs/design/cache.md"}); err != nil {
		t.Fatal(err)
	}
	list, err := svc.Docs("")
	if err != nil || len(list.Docs) != 2 || list.Docs[0].Path != "design/cache.md" || list.Docs[0].Title != "Cache design" || list.Docs[1].Title != "notes.txt" {
		t.Fatalf("docs: %+v %v", list, err)
	}
	d, err := svc.Doc("jk", "design/cache.md")
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Cache design" || !d.Text || d.StorePath != "projects/jk/docs/design/cache.md" || len(d.Backlinks) != 2 {
		t.Fatalf("doc: %+v", d)
	}
	for _, bad := range []string{"../project.yml", "../../.ka/store.yml", "", "design"} {
		if _, err := svc.Doc("jk", bad); err == nil {
			t.Fatalf("Doc(%q) should fail", bad)
		}
	}
}
