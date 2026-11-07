package scenario

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/mcpserver"
)

var ctx = context.Background()

// agentConn is an MCP client connected in memory to a ka server over one clone.
type agentConn struct {
	t   testing.TB
	svc *engine.Service
	cs  *mcp.ClientSession
}

func connect(t testing.TB, svc *engine.Service, clientName string) *agentConn {
	t.Helper()
	srv := mcpserver.New(svc, mcpserver.Options{})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.MCP.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
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
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &sc)
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
