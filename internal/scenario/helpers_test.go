package scenario

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/KanArtist/ka/internal/api"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/mcpserver"
)

var ctx = context.Background()

// agentConn is an MCP client connected in memory to a ka server over one clone.
type agentConn struct {
	t   testing.TB
	svc *engine.Service
	cs  *mcp.ClientSession
}

// viaDaemon makes connect go through a daemon: the client speaks stdio to
// the ka mcp proxy, which forwards to the daemon's /mcp over HTTP.
var viaDaemon bool

// daemons holds one daemon per service (clone) while viaDaemon is set.
var daemons = map[*engine.Service]string{}

func daemonFor(t testing.TB, svc *engine.Service) string {
	if url, ok := daemons[svc]; ok {
		return url
	}
	dctx, cancel := context.WithCancel(ctx)
	srv := api.New(svc, api.Options{Token: "scenario", Version: "test", MCP: mcpserver.New(svc, mcpserver.Options{}).HTTPHandler()})
	ln, err := api.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { srv.Serve(dctx, ln); close(done) }()
	url := "http://" + ln.Addr().String()
	daemons[svc] = url
	t.Cleanup(func() { cancel(); <-done; delete(daemons, svc) })
	return url
}

func connect(t testing.TB, svc *engine.Service, clientName string) *agentConn {
	t.Helper()
	var ct mcp.Transport
	if viaDaemon {
		url := daemonFor(t, svc)
		c1, c2 := net.Pipe()
		p := &mcpserver.Proxy{Endpoint: url + "/mcp", Token: "scenario"}
		done := make(chan struct{})
		go func() { p.Run(ctx, c2, c2); c2.Close(); close(done) }()
		t.Cleanup(func() { c1.Close(); <-done })
		ct = &mcp.IOTransport{Reader: c1, Writer: c1}
	} else {
		srv := mcpserver.New(svc, mcpserver.Options{})
		var st *mcp.InMemoryTransport
		ct, st = mcp.NewInMemoryTransports()
		if _, err := srv.MCP.Connect(ctx, st, nil); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &agentConn{t: t, svc: svc, cs: cs}
}

// call invokes a tool and returns its text and structured content.
func (a *agentConn) call(name string, args map[string]any) (string, map[string]any, bool) {
	a.t.Helper()
	res, err := a.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		a.t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	var sc map[string]any
	if res.StructuredContent != nil {
		// The MCP spec requires an object; clients such as Claude Code
		// reject the whole result otherwise.
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &sc); err != nil {
			a.t.Fatalf("%s: structuredContent is not a JSON object: %s", name, b)
		}
	}
	return text.String(), sc, res.IsError
}

// ok calls a tool and fails the test on a tool error.
func (a *agentConn) ok(name string, args map[string]any) (string, map[string]any) {
	a.t.Helper()
	text, sc, isErr := a.call(name, args)
	if isErr {
		a.t.Fatalf("%s(%v) failed: %s", name, args, text)
	}
	return text, sc
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func digS(m map[string]any, path ...string) string {
	s, _ := dig(m, path...).(string)
	return s
}
