package scenario

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
)

// The M2 exit scenarios, end to end through a daemon: an agent on the ka mcp
// bridge, and a human using the same REST and SSE endpoints as the web UI.

type rest struct {
	t    testing.TB
	base string
}

func (r rest) do(method, path string, body any) map[string]any {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.base+path, rd)
	req.Header.Set("Authorization", "Bearer scenario")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	json.NewDecoder(res.Body).Decode(&m)
	if res.StatusCode >= 300 {
		r.t.Fatalf("%s %s: HTTP %d %v", method, path, res.StatusCode, m)
	}
	return m
}

// watch subscribes to the event stream like the UI and reports each event's
// type and arrival time.
func (r rest) watch(t testing.TB) <-chan struct {
	ev engine.Event
	at time.Time
} {
	c, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(c, "GET", r.base+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer scenario")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan struct {
		ev engine.Event
		at time.Time
	}, 100)
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e engine.Event
				json.Unmarshal([]byte(d), &e)
				out <- struct {
					ev engine.Event
					at time.Time
				}{e, time.Now()}
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	return out
}

// S2: the agent posts progress and checks acceptance items; a human
// watching sees each change within about a second.
func TestS2ProgressVisibleLive(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	seed := testkit.Service(t, r.Clone())
	id := seedTickets(t, seed, engine.CreateIn{Title: "Make it live", Status: "ready", Acceptance: []string{"first", "second"}})[0]

	viaDaemon = true
	defer func() { viaDaemon = false }()
	svc := testkit.Service(t, r.Clone())
	agent := connect(t, svc, "claude-code")
	human := rest{t, daemonFor(t, svc)}
	events := human.watch(t)

	agent.ok("ka_claim", map[string]any{"id": id})
	steps := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"ka_comment", map[string]any{"id": id, "type": "progress", "body": "Wired the SSE client."}, engine.EvCommentCreated},
		{"ka_check", map[string]any{"id": id, "items": []any{map[string]any{"index": 0, "done": true}}}, engine.EvTicketUpdated},
		{"ka_check", map[string]any{"id": id, "items": []any{map[string]any{"index": 1, "done": true}}}, engine.EvTicketUpdated},
	}
	for _, s := range steps {
		sent := time.Now()
		agent.ok(s.tool, s.args)
		deadline := time.After(time.Second)
	wait:
		for {
			select {
			case m := <-events:
				if m.ev.Type == s.want && m.ev.Ticket == id && m.ev.Actor == "agent/claude-code" {
					t.Logf("%s reached the watcher in %v", s.tool, m.at.Sub(sent))
					break wait
				}
			case <-deadline:
				t.Fatalf("%s: no %s event within a second", s.tool, s.want)
			}
		}
	}
	d := human.do("GET", "/api/v1/tickets/"+id, nil)
	items := d["acceptance_items"].([]any)
	if items[0].(map[string]any)["done"] != true || items[1].(map[string]any)["done"] != true {
		t.Fatalf("acceptance: %v", items)
	}
}

// S3: the agent asks with options and waits; the question is in the
// human's inbox; the human picks an option as the UI does; the agent's
// ka_ask returns with the answer.
func TestS3AskAHuman(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	seed := testkit.Service(t, r.Clone())
	id := seedTickets(t, seed, engine.CreateIn{Title: "Pick a storage format", Status: "ready"})[0]

	viaDaemon = true
	defer func() { viaDaemon = false }()
	svc := testkit.Service(t, r.Clone())
	svc.PollStep = 50 * time.Millisecond
	agent := connect(t, svc, "claude-code")
	human := rest{t, daemonFor(t, svc)}

	type result struct {
		text string
		sc   map[string]any
	}
	done := make(chan result, 1)
	go func() {
		text, sc := agent.ok("ka_ask", map[string]any{"id": id, "question": "JSON lines or SQLite?", "options": []any{"JSON lines", "SQLite"}, "wait": "1m"})
		done <- result{text, sc}
	}()

	// The question reaches the human's inbox (the badge counts these).
	var q map[string]any
	for deadline := time.Now().Add(5 * time.Second); q == nil; {
		in := human.do("GET", "/api/v1/inbox", nil)
		if qs, _ := in["questions"].([]any); len(qs) == 1 {
			q = qs[0].(map[string]any)
		} else if time.Now().After(deadline) {
			t.Fatalf("inbox: %v", in)
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	name := q["comment"].(map[string]any)["name"].(string)
	human.do("POST", "/api/v1/tickets/"+id+"/answers", map[string]any{"question": name, "choice": 1})

	select {
	case res := <-done:
		if res.sc["answered"] != true || !strings.Contains(res.text, "SQLite") {
			t.Fatalf("ka_ask returned: %q %v", res.text, res.sc)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's ka_ask did not return after the answer")
	}
	if qs := human.do("GET", "/api/v1/inbox", nil)["questions"].([]any); len(qs) != 0 {
		t.Fatalf("question still open: %v", qs)
	}
}
