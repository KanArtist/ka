// Package scenario holds the PRD §4.2 acceptance scenarios that milestone M1
// must pass as automated tests: S1, S4, S5 (in the multi-clone simulator),
// S6, S8 and S9.
package scenario

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/cli"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
	"github.com/KanArtist/ka/pkg/kaspec"
)

func seedTickets(t *testing.T, svc *engine.Service, ins ...engine.CreateIn) []string {
	t.Helper()
	out, err := svc.Create(ctx, svc.CallerFor("tester", ""), ins)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range out.Created {
		ids = append(ids, r.ID)
	}
	return ids
}

// S1: an agent calls ka_next and gets exactly one unblocked, highest-priority
// ticket with its context, already claimed and pushed; a second agent
// calling at the same moment gets a different ticket.
func TestS1FindAndClaim(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	seed := testkit.Service(t, r.Clone())
	ids := seedTickets(t, seed,
		engine.CreateIn{Title: "P2 work", Priority: "P2", Status: "ready"},
		engine.CreateIn{Title: "Blocker", Priority: "P1", Status: "ready", Acceptance: []string{"blocker done"}},
		engine.CreateIn{Title: "Top but backlog", Priority: "P0", Status: "backlog"},
	)
	seedTickets(t, seed, engine.CreateIn{Title: "Top but blocked", Priority: "P0", Status: "ready", BlockedBy: []string{ids[1]}})

	a := connect(t, testkit.Service(t, r.Clone()), "claude-code")
	b := connect(t, testkit.Service(t, r.Clone()), "codex")

	var wg sync.WaitGroup
	got := make([]map[string]any, 2)
	texts := make([]string, 2)
	for i, ag := range []*agentConn{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			texts[i], got[i] = ag.ok("ka_next", map[string]any{})
		}()
	}
	wg.Wait()
	idA, idB := digS(got[0], "ticket", "id"), digS(got[1], "ticket", "id")
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("agents got %q and %q", idA, idB)
	}
	if !((idA == ids[1] && idB == ids[0]) || (idA == ids[0] && idB == ids[1])) {
		t.Fatalf("expected the P1 blocker and the P2 ticket, got %s and %s", idA, idB)
	}
	for i, g := range got {
		if g["claimed"] != true || g["confirmed"] != true {
			t.Fatalf("agent %d: not claimed and confirmed: %v", i, texts[i])
		}
	}
	if !strings.Contains(texts[0]+texts[1], "blocker done") {
		t.Fatal("context (acceptance) missing from ka_next")
	}
	// The claims were pushed before the calls returned: a fresh clone sees both.
	fresh := testkit.Service(t, r.Clone())
	for _, id := range []string{idA, idB} {
		d, err := fresh.Get(engine.GetIn{ID: id})
		if err != nil || d.Claim == nil {
			t.Fatalf("claim on %s not published: %+v %v", id, d, err)
		}
	}
	if digS(got[0], "ticket", "claim", "by") != "agent/claude-code" && digS(got[1], "ticket", "claim", "by") != "agent/claude-code" {
		t.Fatal("actor is not derived from the MCP client name")
	}
}

// S4: an agent files five related defects in one ka_create call; each gets
// an ID without a network round-trip, links back to the parent, and the
// batch is pushed once.
func TestS4FileFollowUps(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	seed := testkit.Service(t, r.Clone())
	parent := seedTickets(t, seed, engine.CreateIn{Title: "The work", Status: "ready"})[0]
	a := connect(t, testkit.Service(t, r.Clone()), "claude-code")
	r.CountPushes()

	var tickets []any
	for i := range 5 {
		tickets = append(tickets, map[string]any{
			"title": fmt.Sprintf("Defect %d found while working", i), "kind": "bug", "priority": "P2",
			"parent": parent, "problem": "It breaks.", "approach": "Fix it.", "acceptance": []string{"no longer breaks"},
		})
	}
	text, sc := a.ok("ka_create", map[string]any{"tickets": tickets})
	created, _ := sc["created"].([]any)
	if len(created) != 5 {
		t.Fatalf("created %d tickets:\n%s", len(created), text)
	}
	if r.Pushes() != 1 {
		t.Fatalf("expected one push for the batch, got %d", r.Pushes())
	}
	seen := map[string]bool{}
	for _, c := range created {
		id := c.(map[string]any)["id"].(string)
		if seen[id] || !kaspec.IDRe.MatchString(id) {
			t.Fatalf("bad or duplicate ID %q", id)
		}
		seen[id] = true
	}
	fresh := testkit.Service(t, r.Clone())
	d, err := fresh.Get(engine.GetIn{ID: parent, Include: []string{"children"}})
	if err != nil || len(d.Children) != 5 {
		t.Fatalf("parent does not list 5 children: %+v %v", d, err)
	}
	child, _ := fresh.Get(engine.GetIn{ID: d.Children[0].ID})
	if child.Kind != "bug" || child.CreatedBy != "agent/claude-code" || !strings.Contains(child.Body, "## Problem") || child.Status != "backlog" {
		t.Fatalf("child: %+v", child)
	}
}

// S6: an agent without ka writes a ticket by hand and pushes it. ka picks it
// up, ka check reports its defects, and ka fix corrects the formatting.
func TestS6HandWrittenTicket(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	svc := testkit.Service(t, r.Clone())

	// The hand-writer follows the AGENTS.md protocol, imperfectly: wrong key
	// order, CRLF line endings and an unsorted label list.
	hand := r.Clone()
	id := "JK-" + kaspec.RandomBase32(5)
	dir := filepath.Join(hand, "projects", "jk", "tickets", id)
	os.MkdirAll(dir, 0o755)
	content := "---\r\ntitle: \"Written by hand\"\r\nid: " + id + "\r\nstatus: ready\r\nlabels: [zeta, alpha]\r\npriority: P1\r\ncreated: 2026-11-07T10:00:00Z\r\nupdated: 2026-11-07T10:00:00Z\r\n---\r\n\r\n## Problem\r\n\r\nSomething.\r\n"
	os.WriteFile(filepath.Join(dir, "ticket.md"), []byte(content), 0o644)
	testkit.Git(t, hand, "add", "-A")
	testkit.Git(t, hand, "commit", "-qm", "file a ticket by hand")
	testkit.Git(t, hand, "push", "-q")

	// ka picks it up on sync.
	svc.Sync(ctx, true)
	d, err := svc.Get(engine.GetIn{ID: id})
	if err != nil || d.Title != "Written by hand" {
		t.Fatalf("hand-written ticket not indexed: %v", err)
	}
	// ka check reports the defects.
	var codes []string
	for _, dg := range svc.Validate(time.Now()) {
		codes = append(codes, dg.Code)
	}
	if !contains(codes, "crlf") && !contains(codes, "non_canonical") {
		t.Fatalf("defects not reported: %v", codes)
	}
	// ka fix corrects them, in a commit of its own, and pushes.
	out, err := svc.Fix(ctx, svc.CallerFor("", ""), false)
	if err != nil || len(out.Categories) == 0 {
		t.Fatalf("fix: %+v %v", out, err)
	}
	for _, dg := range svc.Validate(time.Now()) {
		if dg.Severity != kaspec.SevInfo {
			t.Errorf("after fix: %s", dg)
		}
	}
	fresh := r.Clone()
	data, _ := os.ReadFile(filepath.Join(fresh, "projects", "jk", "tickets", id, "ticket.md"))
	if bytes.Contains(data, []byte("\r")) || !bytes.HasPrefix(data, []byte("---\nid: "+id+"\ntitle: Written by hand\n")) {
		t.Fatalf("fix not published:\n%s", data)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// S8: an agent finishes and calls ka_transition(done, resolution); the
// resolution is recorded as a comment, every watcher sees the move, and
// dependents become unblocked and show up in ka_next.
func TestS8CloseTheLoop(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	seed := testkit.Service(t, r.Clone())
	ids := seedTickets(t, seed, engine.CreateIn{Title: "First", Priority: "P2", Status: "ready"})
	dep := seedTickets(t, seed, engine.CreateIn{Title: "Depends on first", Priority: "P0", Status: "ready", BlockedBy: ids})[0]

	a := connect(t, testkit.Service(t, r.Clone()), "claude-code")
	_, sc := a.ok("ka_next", map[string]any{})
	if digS(sc, "ticket", "id") != ids[0] {
		t.Fatalf("next should skip the blocked P0: %v", sc)
	}
	text, sc := a.ok("ka_transition", map[string]any{"id": ids[0], "status": "done", "summary": "Shipped the first part."})
	if un, _ := sc["unblocked"].([]any); len(un) != 1 || un[0] != dep {
		t.Fatalf("unblocked: %s", text)
	}
	_, sc = a.ok("ka_get", map[string]any{"id": ids[0], "include": []string{"comments"}})
	cs, _ := sc["comments"].([]any)
	if len(cs) != 1 || dig(cs[0].(map[string]any), "resolution") != "done" || !strings.Contains(digS(cs[0].(map[string]any), "body"), "Shipped") {
		t.Fatalf("resolution comment: %v", sc["comments"])
	}
	// Another watcher sees the move after a sync.
	watcher := testkit.Service(t, r.Clone())
	if d, _ := watcher.Get(engine.GetIn{ID: ids[0]}); d.Status != "done" || d.Claim != nil {
		t.Fatalf("watcher sees %+v", d.Summary)
	}
	_, sc = a.ok("ka_next", map[string]any{"claim": false})
	if digS(sc, "ticket", "id") != dep {
		t.Fatalf("dependent not offered: %v", sc)
	}
}

// S9: an agent asks for approval and tells the human to run an exact
// command; `ka approve <ID>` works as written and the agent sees the result.
func TestS9PointAHumanAtAnAction(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	seed := testkit.Service(t, r.Clone())
	id := seedTickets(t, seed, engine.CreateIn{Title: "Drop the legacy table", Status: "ready"})[0]
	a := connect(t, testkit.Service(t, r.Clone()), "claude-code")
	text, _ := a.ok("ka_ask", map[string]any{"id": id, "question": "About to drop table users_old. Approve?", "kind": "approval"})
	if !strings.Contains(text, id) {
		t.Fatalf("ask: %s", text)
	}

	// The human runs exactly what the agent said, in their own checkout.
	human := r.Clone()
	var out, errb bytes.Buffer
	code := cli.Run(ctx, &cli.IO{Out: &out, Err: &errb, Dir: human}, []string{"approve", id, "-m", "go ahead"})
	if code != 0 {
		t.Fatalf("ka approve exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "approved") || !strings.Contains(out.String(), "pushed") {
		t.Fatalf("ka approve output: %s", out.String())
	}
	// The agent sees the approval after syncing.
	a.ok("ka_sync", map[string]any{"force": true})
	text, sc := a.ok("ka_inbox", map[string]any{})
	ans, _ := sc["answers"].([]any)
	if len(ans) != 1 || dig(ans[0].(map[string]any), "comment", "approved") != true {
		t.Fatalf("inbox: %s", text)
	}
	// A second approve has nothing left to answer, and says so.
	out.Reset()
	errb.Reset()
	if code := cli.Run(ctx, &cli.IO{Out: &out, Err: &errb, Dir: human}, []string{"approve", id}); code == 0 || !strings.Contains(errb.String(), "no_open_question") {
		t.Fatalf("second approve: %d %s", code, errb.String())
	}
}
