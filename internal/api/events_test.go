package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/testkit"
)

type sseMsg struct {
	id    uint64
	typ   string
	ev    engine.Event
	data  map[string]any
	ping  bool
	start time.Time
}

type stream struct {
	t      *testing.T
	msgs   chan sseMsg
	cancel context.CancelFunc
}

// subscribe opens GET /api/v1/events and parses messages in the background.
func (c *client) subscribe(query string, hdr ...string) *stream {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/api/v1/events"+query, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		c.t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		c.t.Fatalf("events: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	s := &stream{t: c.t, msgs: make(chan sseMsg, 1000), cancel: cancel}
	c.t.Cleanup(cancel)
	go func() {
		defer res.Body.Close()
		defer close(s.msgs)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var m sseMsg
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if m.typ != "" || m.ping {
					m.start = time.Now()
					s.msgs <- m
				}
				m = sseMsg{}
			case line == ": ping":
				m.ping = true
			case strings.HasPrefix(line, "id: "):
				m.id, _ = strconv.ParseUint(line[4:], 10, 64)
			case strings.HasPrefix(line, "event: "):
				m.typ = line[7:]
			case strings.HasPrefix(line, "data: "):
				json.Unmarshal([]byte(line[6:]), &m.ev)
				json.Unmarshal([]byte(line[6:]), &m.data)
			}
		}
	}()
	// Let the handler register its cursor before the test acts.
	time.Sleep(50 * time.Millisecond)
	return s
}

// next waits for the next event (not ping) of one of the given types, skipping others.
func (s *stream) next(timeout time.Duration, types ...string) sseMsg {
	s.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-s.msgs:
			if !ok {
				s.t.Fatalf("stream closed waiting for %v", types)
			}
			if m.ping {
				continue
			}
			if len(types) == 0 {
				return m
			}
			for _, t := range types {
				if m.typ == t {
					return m
				}
			}
		case <-deadline:
			s.t.Fatalf("no %v event within %v", types, timeout)
		}
	}
}

// drain returns everything already received or arriving within d.
func (s *stream) drain(d time.Duration) []sseMsg {
	var out []sseMsg
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-s.msgs:
			if !ok {
				return out
			}
			if !m.ping {
				out = append(out, m)
			}
		case <-deadline:
			return out
		}
	}
}

func dataStr(m sseMsg, keys ...string) string {
	return str(m.data, append([]string{"data"}, keys...)...)
}

// An operation made through the daemon reaches an SSE client in under 250 ms,
// with a self-sufficient payload.
func TestEventsLatencyAndPayloads(t *testing.T) {
	c := start(t)
	agent := []string{"X-Ka-Actor", "agent/claude"}
	s := c.subscribe("")

	sent := time.Now()
	r := c.ok("POST", "/api/v1/tickets", map[string]any{"title": "Live ticket", "status": "ready", "acceptance": []string{"works"}}, agent...)
	id := str(r.m["created"].([]any)[0].(map[string]any), "id")
	m := s.next(2*time.Second, engine.EvTicketCreated)
	if m.ev.Ticket != id || m.ev.Actor != "agent/claude" || dataStr(m, "ticket", "title") != "Live ticket" || m.ev.Project != "jk" {
		t.Fatalf("ticket.created: %+v", m.data)
	}
	if seq, _ := strconv.ParseUint(r.hdr.Get(SeqHeader), 10, 64); seq == 0 {
		t.Fatalf("no %s header", SeqHeader)
	}

	// Measure from the moment the request is sent to the event's arrival.
	var worst time.Duration
	for i, step := range []struct {
		method, path string
		body         map[string]any
		want         string
	}{
		{"POST", "/tickets/" + id + "/claim", map[string]any{}, engine.EvClaimChanged},
		{"POST", "/tickets/" + id + "/comments", map[string]any{"body": "note " + strconv.Itoa(1)}, engine.EvCommentCreated},
		{"PATCH", "/tickets/" + id, map[string]any{"set": map[string]any{"priority": "P0"}}, engine.EvTicketUpdated},
		{"POST", "/tickets/" + id + "/acceptance", map[string]any{"items": []map[string]any{{"index": 0, "done": true}}}, engine.EvTicketUpdated},
		{"POST", "/tickets/" + id + "/transition", map[string]any{"status": "done", "summary": "Shipped."}, engine.EvTicketMoved},
	} {
		sent = time.Now()
		c.ok(step.method, "/api/v1"+step.path, step.body, agent...)
		m = s.next(2*time.Second, step.want)
		if d := m.start.Sub(sent); d > worst {
			worst = d
		}
		switch i {
		case 0:
			if dataStr(m, "to", "by") != "agent/claude" || m.ev.Actor != "agent/claude" {
				t.Fatalf("claim.changed: %+v", m.data)
			}
		case 1:
			if dataStr(m, "comment", "body") == "" || dataStr(m, "ticket", "id") != id {
				t.Fatalf("comment.created: %+v", m.data)
			}
		case 2:
			ch := m.data["data"].(map[string]any)["changes"].(map[string]any)["priority"].(map[string]any)
			if ch["from"] != "P2" && ch["from"] != nil || ch["to"] != "P0" || dataStr(m, "ticket", "priority") != "P0" {
				t.Fatalf("ticket.updated: %+v", m.data)
			}
		case 3:
			if dataStr(m, "ticket", "acceptance") != "1/1" {
				t.Fatalf("acceptance update: %+v", m.data)
			}
		case 4:
			if dataStr(m, "from") != "in_progress" || dataStr(m, "to") != "done" || dataStr(m, "category") != "completed" {
				t.Fatalf("ticket.moved: %+v", m.data)
			}
		}
	}
	t.Logf("worst operation-to-event latency: %v", worst)
	if worst > 250*time.Millisecond {
		t.Fatalf("an operation took %v to reach the SSE client (target 250 ms)", worst)
	}
	// The resolution comment arrives too, and the claim release.
	got := map[string]bool{}
	for _, m := range s.drain(300 * time.Millisecond) {
		got[m.typ] = true
	}
	if !got[engine.EvCommentCreated] || !got[engine.EvClaimChanged] {
		t.Fatalf("after close: %v", got)
	}
}

func TestEventsQuestions(t *testing.T) {
	c := start(t)
	id := str(c.ok("POST", "/api/v1/tickets", map[string]any{"title": "Q", "owner": "tester"}).m["created"].([]any)[0].(map[string]any), "id")
	s := c.subscribe("?types=question.*")
	c.ok("POST", "/api/v1/tickets/"+id+"/questions", map[string]any{"question": "Which?", "options": []string{"a", "b"}}, "X-Ka-Actor", "agent/claude")
	m := s.next(2*time.Second, engine.EvQuestionOpened)
	if dataStr(m, "comment", "type") != "question" || m.ev.Actor != "agent/claude" {
		t.Fatalf("question.opened: %+v", m.data)
	}
	c.ok("POST", "/api/v1/tickets/"+id+"/answers", map[string]any{"choice": 1})
	m = s.next(2 * time.Second)
	if m.typ != engine.EvQuestionAnswered || m.ev.Actor != "tester" {
		t.Fatalf("question.answered: %s %+v", m.typ, m.data)
	}
	// The filter kept everything else out.
	for _, m := range s.drain(200 * time.Millisecond) {
		t.Fatalf("filtered stream got %s", m.typ)
	}
}

// Reconnecting with Last-Event-ID replays what was missed; an ID older than
// the ring buffer (or from another daemon) yields reset.
func TestEventsReplayAndReset(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	svc := testkit.Service(t, r.Clone())
	hub := engine.NewEventHub(8)
	svc.SetEvents(hub)
	c := serveWith(t, svc, Options{Events: hub, Heartbeat: 50 * time.Millisecond})

	first := c.subscribe("")
	c.ok("POST", "/api/v1/tickets", map[string]any{"title": "One"})
	m1 := first.next(2*time.Second, engine.EvTicketCreated)
	first.cancel()
	c.ok("POST", "/api/v1/tickets", map[string]any{"title": "Two"})
	c.ok("POST", "/api/v1/tickets", map[string]any{"title": "Three"})

	again := c.subscribe("", "Last-Event-ID", strconv.FormatUint(m1.id, 10))
	a, b := again.next(2*time.Second, engine.EvTicketCreated), again.next(2*time.Second, engine.EvTicketCreated)
	if dataStr(a, "ticket", "title") != "Two" || dataStr(b, "ticket", "title") != "Three" || a.id <= m1.id {
		t.Fatalf("replay: %+v %+v", a.data, b.data)
	}
	// Heartbeats keep the stream open.
	for pinged, deadline := false, time.After(time.Second); !pinged; {
		select {
		case m := <-again.msgs:
			pinged = m.ping
		case <-deadline:
			t.Fatal("no heartbeat")
		}
	}

	// Overflow the 8-event buffer; the first ID is now too old.
	for i := 0; i < 10; i++ {
		c.ok("POST", "/api/v1/tickets", map[string]any{"title": "Filler " + strconv.Itoa(i)})
	}
	old := c.subscribe("", "Last-Event-ID", strconv.FormatUint(m1.id, 10))
	if m := old.next(2 * time.Second); m.typ != engine.EvReset || m.id != hub.Seq() {
		t.Fatalf("expected reset at %d, got %s %d", hub.Seq(), m.typ, m.id)
	}
	// An ID from an earlier daemon is too old as well; garbage is treated the same.
	for _, id := range []string{"5", "not-a-number"} {
		if m := c.subscribe("?last_event_id=" + id).next(2 * time.Second); m.typ != engine.EvReset {
			t.Fatalf("Last-Event-ID %s: got %s", id, m.typ)
		}
	}
	// After the reset, live events flow.
	c.ok("POST", "/api/v1/tickets", map[string]any{"title": "After reset"})
	if m := old.next(2*time.Second, engine.EvTicketCreated); dataStr(m, "ticket", "title") != "After reset" {
		t.Fatalf("after reset: %+v", m.data)
	}
}

// Commits by other processes on the clone, and upstream changes fetched by
// sync, produce the same events.
func TestEventsFromOtherProcessesAndUpstream(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	clone := r.Clone()
	c := serve(t, testkit.Service(t, clone))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.svc.WatchLocal(ctx, 50*time.Millisecond)
	s := c.subscribe("")

	// Another process on the same clone (the CLI, another ka mcp).
	other := testkit.Service(t, clone)
	out, err := other.Create(context.Background(), other.CallerFor("agent/codex", ""), []engine.CreateIn{{Title: "From the CLI"}})
	if err != nil {
		t.Fatal(err)
	}
	m := s.next(2*time.Second, engine.EvTicketCreated)
	if m.ev.Ticket != out.Created[0].ID || m.ev.Actor != "agent/codex" {
		t.Fatalf("local commit event: %+v", m.ev)
	}

	// Someone else pushes; the daemon syncs.
	remote := testkit.Service(t, r.Clone())
	up, err := remote.Create(context.Background(), remote.CallerFor("alice", ""), []engine.CreateIn{{Title: "From upstream"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Comment(context.Background(), remote.CallerFor("alice", ""), engine.CommentIn{ID: out.Created[0].ID, Body: "seen it"}); err != nil {
		t.Fatal(err)
	}
	c.ok("POST", "/api/v1/sync", map[string]any{"force": true})
	got := map[string]engine.Event{}
	for _, m := range s.drain(500 * time.Millisecond) {
		got[m.typ+" "+m.ev.Ticket] = m.ev
	}
	if _, ok := got[engine.EvTicketCreated+" "+up.Created[0].ID]; !ok {
		t.Fatalf("upstream ticket.created missing: %v", keys(got))
	}
	if e, ok := got[engine.EvCommentCreated+" "+out.Created[0].ID]; !ok || e.Actor != "alice" {
		t.Fatalf("upstream comment.created missing: %v", keys(got))
	}
	if _, ok := got[engine.EvSyncState+" "]; !ok {
		t.Fatalf("sync.state missing: %v", keys(got))
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEventFilter(t *testing.T) {
	f := engine.ParseEventFilter("ticket.*,comment.created", "jk", "")
	for _, tc := range []struct {
		e    engine.Event
		want bool
	}{
		{engine.Event{Type: "ticket.updated", Project: "jk"}, true},
		{engine.Event{Type: "comment.created", Project: "jk"}, true},
		{engine.Event{Type: "claim.changed", Project: "jk"}, false},
		{engine.Event{Type: "ticket.updated", Project: "other"}, false},
		{engine.Event{Type: "reset"}, true},
	} {
		if got := f.Match(tc.e); got != tc.want {
			t.Errorf("%+v: %v", tc.e, got)
		}
	}
	if !engine.ParseEventFilter("", "", "jk-1").Match(engine.Event{Type: "ticket.updated", Ticket: "JK-1"}) {
		t.Error("ticket filter is case-insensitive")
	}
}

// POST /hooks/git makes the daemon fetch now; upstream changes arrive as
// events without waiting for the fetch interval.
func TestGitHookFetchesNow(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	c := serve(t, testkit.Service(t, r.Clone()))
	c.svc.WS.Stores[0].FetchInterval = time.Hour
	c.svc.Background = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.svc.FetchLoop(ctx)
	s := c.subscribe("?types=ticket.created")

	other := testkit.Service(t, r.Clone())
	out, err := other.Create(context.Background(), other.CallerFor("alice", ""), []engine.CreateIn{{Title: "Pushed elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := c.do("POST", "/api/v1/hooks/git", map[string]any{"ref": "refs/heads/main"}); r.status != 202 {
		t.Fatalf("hook: %d %s", r.status, r.raw)
	}
	sent := time.Now()
	m := s.next(5*time.Second, engine.EvTicketCreated)
	if m.ev.Ticket != out.Created[0].ID || m.ev.Actor != "alice" {
		t.Fatalf("event: %+v", m.ev)
	}
	t.Logf("webhook to event: %v", m.start.Sub(sent))
	if d := m.start.Sub(sent); d > 2*time.Second {
		t.Fatalf("webhook to event took %v (target 2 s)", d)
	}
	st := c.ok("GET", "/api/v1/stores", nil).m["stores"].([]any)[0].(map[string]any)
	if str(st, "push_mode") != "immediate" || str(st, "fetch_interval") != "1h0m0s" {
		t.Fatalf("store: %+v", st)
	}
}

// GET /activity turns git history into the same events as the live stream.
func TestActivityHistory(t *testing.T) {
	c := start(t)
	agent := []string{"X-Ka-Actor", "agent/claude"}
	id := str(c.ok("POST", "/api/v1/tickets", map[string]any{"title": "History", "status": "ready", "acceptance": []string{"a"}}, agent...).m["created"].([]any)[0].(map[string]any), "id")
	c.ok("POST", "/api/v1/tickets/"+id+"/claim", map[string]any{}, agent...)
	c.ok("POST", "/api/v1/tickets/"+id+"/comments", map[string]any{"type": "progress", "body": "Halfway there."}, agent...)
	c.ok("PATCH", "/api/v1/tickets/"+id, map[string]any{"set": map[string]any{"priority": "P0"}, "add": map[string]any{"labels": []string{"x"}}})
	c.ok("POST", "/api/v1/tickets/"+id+"/transition", map[string]any{"status": "done", "summary": "Finished."}, agent...)

	r := c.ok("GET", "/api/v1/activity?limit=50", nil)
	evs := r.m["events"].([]any)
	types := map[string]map[string]any{}
	for _, e := range evs {
		m := e.(map[string]any)
		if str(m, "ticket") == id {
			if _, seen := types[str(m, "type")]; !seen {
				types[str(m, "type")] = m
			}
		}
	}
	for _, want := range []string{"ticket.created", "claim.changed", "comment.created", "ticket.updated", "ticket.moved"} {
		if types[want] == nil {
			t.Fatalf("activity lacks %s: %s", want, r.raw)
		}
	}
	if m := types["ticket.moved"]; str(m, "data", "from") != "in_progress" || str(m, "data", "to") != "done" || str(m, "actor") != "agent/claude" || str(m, "commit") == "" {
		t.Fatalf("moved: %+v", m)
	}
	if m := types["comment.created"]; str(m, "data", "comment", "body") == "" {
		t.Fatalf("comment: %+v", m)
	}
	if m := types["ticket.updated"]; str(m, "actor") != "tester" || m["data"].(map[string]any)["changes"].(map[string]any)["priority"] == nil {
		t.Fatalf("updated: %+v", m)
	}
	// One ticket's timeline, and the diff behind an entry.
	r2 := c.ok("GET", "/api/v1/activity?ticket="+id, nil)
	for _, e := range r2.m["events"].([]any) {
		if str(e.(map[string]any), "ticket") != id {
			t.Fatalf("ticket timeline has %s", r2.raw)
		}
	}
	if n := len(r2.m["events"].([]any)); n < 6 {
		t.Fatalf("timeline: %d events", n)
	}
	sha := str(types["ticket.updated"], "commit")
	d := c.ok("GET", "/api/v1/commits/"+sha, nil)
	if !strings.Contains(str(d.m, "diff"), "+priority: P0") || str(d.m, "store") == "" {
		t.Fatalf("commit diff: %s", d.raw)
	}
	if r := c.do("GET", "/api/v1/commits/zzzz", nil); r.status != 422 {
		t.Fatalf("bad sha: %d", r.status)
	}
	if r := c.do("GET", "/api/v1/commits/abcdef0", nil); r.status != 404 {
		t.Fatalf("unknown sha: %d", r.status)
	}
	// Paging back from the oldest entry ends.
	last := evs[len(evs)-1].(map[string]any)
	r = c.ok("GET", "/api/v1/activity?limit=50&before="+str(last, "at"), nil)
	if r.m["more"] != false {
		t.Fatalf("paging: %s", r.raw)
	}
}
