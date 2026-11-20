package cli

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/KanArtist/ka/internal/api"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/mcpserver"
	"github.com/KanArtist/ka/internal/testkit"
)

// With a daemon running for the workspace, `ka mcp` proxies to it: writes
// go through the daemon's service (its event hub sees them at once).
func TestMCPProxiesToDaemon(t *testing.T) {
	testkit.GitEnv(t)
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	r := testkit.NewRemote(t, nil)
	dir := r.Clone()
	svc := testkit.Service(t, dir)
	hub := engine.NewEventHub(100)
	svc.SetEvents(hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := api.New(svc, api.Options{Token: "tok", Version: "test", Events: hub, MCP: mcpserver.New(svc, mcpserver.Options{}).HTTPHandler()})
	ln, err := api.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ctx, ln)
	rt := &api.Runtime{PID: os.Getpid(), Addr: ln.Addr().String(), URL: "http://" + ln.Addr().String(), Workspace: svc.WS.Source,
		WorkspaceHash: api.WorkspaceHash(svc.WS.Source), Token: "tok"}
	if err := api.WriteRuntime(rt); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []struct {
		name   string
		args   []string
		viaHub bool
	}{{"proxy", []string{"mcp"}, true}, {"in-process", []string{"mcp", "--no-daemon"}, false}} {
		t.Run(mode.name, func(t *testing.T) {
			c1, c2 := net.Pipe()
			done := make(chan int)
			go func() {
				done <- Run(ctx, &IO{In: c2, Out: c2, Err: os.Stderr, Dir: filepath.Join(dir)}, mode.args)
				c2.Close()
			}()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: c1, Writer: c1}, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := hub.Seq()
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ka_create", Arguments: map[string]any{"tickets": []any{map[string]any{"title": "Via " + mode.name}}}})
			if err != nil || res.IsError {
				t.Fatalf("ka_create: %v %+v", err, res)
			}
			evs, _, _ := hub.Since(before)
			sawCreate := false
			for _, e := range evs {
				if e.Type == engine.EvTicketCreated && e.Actor == "agent/claude-code" {
					sawCreate = true
				}
			}
			if sawCreate != mode.viaHub {
				t.Fatalf("daemon saw the write: %v, want %v (%+v)", sawCreate, mode.viaHub, evs)
			}
			cs.Close()
			c1.Close()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("ka mcp exited %d", code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("ka mcp did not exit after the client closed")
			}
		})
	}
}

// ka open --print, with a daemon running, prints the token exchange URL
// for the ticket.
func TestOpenPrintsAuthURL(t *testing.T) {
	testkit.GitEnv(t)
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	r := testkit.NewRemote(t, nil)
	dir := r.Clone()
	svc := testkit.Service(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := api.New(svc, api.Options{Token: "tok", Version: "test"})
	ln, err := api.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ctx, ln)
	rt := &api.Runtime{PID: os.Getpid(), Addr: ln.Addr().String(), URL: "http://" + ln.Addr().String(), Workspace: svc.WS.Source,
		WorkspaceHash: api.WorkspaceHash(svc.WS.Source), Token: "tok"}
	if err := api.WriteRuntime(rt); err != nil {
		t.Fatal(err)
	}
	// The URL carries a one-time code, never the local token.
	res := ka(t, dir, "open", "jk-12345", "--print")
	u, err := url.Parse(strings.TrimSpace(res.out))
	if res.code != 0 || err != nil || u.Path != "/auth" || u.Query().Get("next") != "/ticket/JK-12345" || len(u.Query().Get("code")) != 64 ||
		u.Query().Has("token") || strings.Contains(res.out, rt.Token) {
		t.Fatalf("open: %+v", res)
	}
	if u2 := strings.TrimSpace(ka(t, dir, "open", "--print").out); u2 == u.String() {
		t.Fatal("ka open reused a code")
	}
	// The code works once.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i, want := range []int{http.StatusSeeOther, http.StatusUnauthorized} {
		resp, err := noRedirect.Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("use %d of the code: %d, want %d", i+1, resp.StatusCode, want)
		}
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ka watch QUERY refreshes the matching tickets when a ticket changes, and
// finds a restarted daemon (new address and token) on its own.
func TestWatchRefreshesAndRediscovers(t *testing.T) {
	testkit.GitEnv(t)
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	r := testkit.NewRemote(t, nil)
	dir := r.Clone()
	svc := testkit.Service(t, dir)
	serve := func(tok string) (*engine.EventHub, context.CancelFunc) {
		hub := engine.NewEventHub(100)
		svc.SetEvents(hub)
		ctx, cancel := context.WithCancel(context.Background())
		srv := api.New(svc, api.Options{Token: tok, Version: "test", Events: hub})
		ln, err := api.Listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ctx, ln)
		rt := &api.Runtime{PID: os.Getpid(), Addr: ln.Addr().String(), URL: "http://" + ln.Addr().String(), Workspace: svc.WS.Source,
			WorkspaceHash: api.WorkspaceHash(svc.WS.Source), Token: tok}
		if err := api.WriteRuntime(rt); err != nil {
			t.Fatal(err)
		}
		return hub, cancel
	}
	until := func(d time.Duration, publish func(), cond func() bool) bool {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			publish()
			if cond() {
				return true
			}
		}
		return false
	}

	hubA, stopA := serve("a")
	defer stopA()
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan int)
	go func() {
		done <- Run(ctx, &IO{In: strings.NewReader(""), Out: out, Err: io.Discard, Dir: dir}, []string{"watch", "--history", "0", "--json", "Later"})
	}()

	created, err := svc.Create(ctx, svc.CallerFor("", ""), []engine.CreateIn{{Title: "Later work"}})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Created[0].ID
	// Well inside the old 3 s poll: the create event triggers the refresh.
	if !until(2500*time.Millisecond, func() { hubA.Publish(engine.Event{Type: "ticket.updated", Ticket: id}) },
		func() bool { return strings.Contains(out.String(), `"type":"ticket.updated"`) }) {
		t.Fatalf("no event for the newly matching ticket:\n%s", out)
	}

	stopA()
	hubB, stopB := serve("b")
	defer stopB()
	if !until(10*time.Second, func() { hubB.Publish(engine.Event{Type: "ticket.moved", Ticket: id}) },
		func() bool { return strings.Contains(out.String(), `"type":"ticket.moved"`) }) {
		t.Fatalf("did not reconnect to the new daemon:\n%s", out)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ka watch did not exit")
	}
}
