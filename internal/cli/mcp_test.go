package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	res := ka(t, dir, "open", "jk-12345", "--print")
	want := rt.URL + "/auth?next=%2Fticket%2FJK-12345&token=tok"
	if res.code != 0 || strings.TrimSpace(res.out) != want {
		t.Fatalf("open: %+v, want %s", res, want)
	}
}
