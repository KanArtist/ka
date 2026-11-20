package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/mcpserver"
	"github.com/KanArtist/ka/internal/presence"
	"github.com/KanArtist/ka/internal/testkit"
)

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// MCP sessions on the daemon show up in GET /presence and as
// presence.changed events, with their last tool call, and disappear when
// they close.
func TestPresence(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	svc := testkit.Service(t, r.Clone())
	hub := engine.NewEventHub(1000)
	svc.SetEvents(hub)
	pres := presence.New(hub)
	c := serveWith(t, svc, Options{Events: hub, Presence: pres, MCP: mcpserver.New(svc, mcpserver.Options{Presence: pres}).HTTPHandler()})
	id := str(c.ok("POST", "/api/v1/tickets", map[string]any{"title": "Ready work", "status": "ready"}).m["created"].([]any)[0].(map[string]any), "id")
	s := c.subscribe("?types=presence.changed")

	// Without the token, /mcp is refused like everything else.
	if r := c.do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}, "Authorization", ""); r.status != 401 {
		t.Fatalf("/mcp without token: %d", r.status)
	}

	ctx := context.Background()
	tr := &mcp.StreamableClientTransport{Endpoint: c.base + "/mcp", HTTPClient: &http.Client{Transport: bearer{testToken}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "Claude Code", Version: "2.1"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ka_next", Arguments: map[string]any{}}); err != nil || res.IsError {
		t.Fatalf("ka_next: %v %+v", err, res)
	}
	m := s.next(2*time.Second, engine.EvPresenceChanged)
	if dataStr(m, "state") != "connected" {
		t.Fatalf("first presence event: %+v", m.data)
	}
	var last sseMsg
	for last = m; dataStr(last, "session", "last_tool") != "ka_next"; {
		last = s.next(2*time.Second, engine.EvPresenceChanged)
	}
	if dataStr(last, "session", "last_ticket") != id || last.ev.Actor != "agent/claude-code" {
		t.Fatalf("presence after ka_next: %+v", last.data)
	}
	// A failing call records its error code.
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "ka_get", Arguments: map[string]any{"id": "JK-ZZZZZ"}})

	res := c.ok("GET", "/api/v1/presence", nil)
	ss := res.m["sessions"].([]any)
	if len(ss) != 1 {
		t.Fatalf("presence: %s", res.raw)
	}
	sess := ss[0].(map[string]any)
	// The session shows under a derived ID, not the Mcp-Session-Id, and the
	// claim (and its Ka-Session trailer) carries the same one.
	if pid := str(sess, "id"); pid != presence.PublicID(cs.ID()) || pid == cs.ID() || len(pid) != 12 || strings.Contains(string(res.raw), cs.ID()) {
		t.Fatalf("session id %q for %q", pid, cs.ID())
	}
	if src := string(c.ok("GET", "/api/v1/tickets/"+id+"/source", nil).raw); !strings.Contains(src, presence.PublicID(cs.ID())) || strings.Contains(src, cs.ID()) {
		t.Fatalf("claim session in the ticket file:\n%s", src)
	}
	if dataStr(last, "session", "id") != presence.PublicID(cs.ID()) {
		t.Fatalf("event session id: %+v", last.data)
	}
	if str(sess, "actor") != "agent/claude-code" || str(sess, "client") != "Claude Code 2.1" || str(sess, "last_tool") != "ka_get" ||
		str(sess, "last_ticket") != "JK-ZZZZZ" || str(sess, "last_error") != "not_found" || str(sess, "on_behalf_of") != "tester" {
		t.Fatalf("session: %s", res.raw)
	}

	cs.Close()
	for {
		m := s.next(3*time.Second, engine.EvPresenceChanged)
		if dataStr(m, "state") == "disconnected" {
			break
		}
	}
	if n := len(c.ok("GET", "/api/v1/presence", nil).m["sessions"].([]any)); n != 0 {
		t.Fatalf("%d sessions after close", n)
	}
}

func TestPresenceSweep(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tr := presence.New(nil)
	tr.Now = func() time.Time { return now }
	if !tr.Seen("a", presence.Session{Actor: "agent/x"}, presence.Call{Method: "tools/call", Tool: "ka_next"}) {
		t.Fatal("first call is new")
	}
	if tr.Seen("a", presence.Session{Actor: "agent/x"}, presence.Call{Method: "tools/list"}) {
		t.Fatal("second call is not new")
	}
	if l := tr.List(); len(l) != 1 || l[0].Calls != 2 || l[0].LastTool != "ka_next" || l[0].LastMethod != "tools/list" {
		t.Fatalf("list: %+v", l)
	}
	now = now.Add(3 * time.Hour)
	tr.Sweep(2 * time.Hour)
	if len(tr.List()) != 0 {
		t.Fatal("idle session not swept")
	}
}

// In remote mode an MCP session belongs to the principal that created it:
// another token holder (or the local token) cannot use or end it.
func TestMCPSessionBoundToPrincipal(t *testing.T) {
	testkit.GitEnv(t)
	svc := testkit.Service(t, testkit.NewRemote(t, nil).Clone())
	ts := NewTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	bob, _, _ := ts.Create("bob", "")
	alice, _, _ := ts.Create("alice", "")
	c := serveWith(t, svc, Options{Remote: &Remote{Tokens: ts}, MCP: mcpserver.New(svc, mcpserver.Options{}).HTTPHandler()})

	ctx := context.Background()
	tr := &mcp.StreamableClientTransport{Endpoint: c.base + "/mcp", HTTPClient: &http.Client{Transport: bearer{bob}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "bob-agent", Version: "1"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list := map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/list"}
	for _, tok := range []string{alice, testToken} {
		for _, method := range []string{"POST", "GET", "DELETE"} {
			var body any
			if method == "POST" {
				body = list
			}
			r := c.do(method, "/mcp", body, "Authorization", "Bearer "+tok, "Mcp-Session-Id", cs.ID(),
				"Accept", "application/json, text/event-stream", "Mcp-Protocol-Version", "2025-06-18")
			if r.status != 403 {
				t.Fatalf("%s /mcp with another principal's session: %d %s", method, r.status, r.raw)
			}
		}
	}
	// The owner's session is untouched.
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatalf("owner after refused requests: %v", err)
	}
}
