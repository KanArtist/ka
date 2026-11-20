package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/testkit"
)

// fakeDaemon answers initialize with a session; handle answers the rest.
func fakeDaemon(t *testing.T, handle func(w http.ResponseWriter, sid string, msg map[string]any)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		var msg map[string]any
		json.NewDecoder(r.Body).Decode(&msg)
		if msg["method"] == "initialize" {
			mu.Lock()
			n++
			sid := fmt.Sprintf("s%d", n)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":"2025-06-18"}}`, msg["id"])
			return
		}
		handle(w, r.Header.Get("Mcp-Session-Id"), msg)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runProxy sends lines to a proxy and returns what it writes back once it
// has written want messages.
func runProxy(t *testing.T, endpoint string, want int, lines ...string) []map[string]any {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &Proxy{Endpoint: endpoint, Token: "tok"}
	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background(), inR, outW); outW.Close() }()
	go func() {
		for _, l := range lines {
			io.WriteString(inW, l+"\n")
		}
	}()
	var out []map[string]any
	got := make(chan struct{})
	go func() {
		defer close(got)
		sc := bufio.NewScanner(outR)
		for len(out) < want && sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			out = append(out, m)
		}
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatalf("got %d of %d messages: %v", len(out), want, out)
	}
	inW.Close()
	go io.Copy(io.Discard, outR)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return out
}

const initLine = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`

// A response stream that ends without the request's answer (the daemon
// stopped mid-call) gives the client an error instead of leaving it waiting.
func TestProxyStreamEndsWithoutAnswer(t *testing.T) {
	srv := fakeDaemon(t, func(w http.ResponseWriter, sid string, msg map[string]any) {
		if msg["id"] == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// A server request that happens to reuse the ID is not the answer.
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"roots/list\"}\n\n")
	})
	out := runProxy(t, srv.URL, 3, initLine, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"ka_ask"}}`)
	last := out[2]
	if last["id"] != float64(7) || last["error"] == nil || !strings.Contains(fmt.Sprint(last["error"]), "without an answer") {
		t.Fatalf("messages: %v", out)
	}
}

// When the daemon has expired the session, the proxy starts a new one with
// the client's initialize and sends the request again.
func TestProxyRenewsExpiredSession(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := fakeDaemon(t, func(w http.ResponseWriter, sid string, msg map[string]any) {
		mu.Lock()
		seen = append(seen, sid+" "+fmt.Sprint(msg["method"]))
		mu.Unlock()
		switch {
		case sid == "s1" && msg["method"] == "tools/call":
			http.Error(w, "session not found", http.StatusNotFound)
		case msg["id"] == nil:
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%v,"result":{"session":%q}}`, msg["id"], sid)
		}
	})
	out := runProxy(t, srv.URL, 2, initLine, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
	if res, _ := out[1]["result"].(map[string]any); res["session"] != "s2" {
		t.Fatalf("messages: %v (daemon saw %v)", out, seen)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"s1 tools/call", "s2 notifications/initialized", "s2 tools/call"}; strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("daemon saw %v, want %v", seen, want)
	}
}

func TestSameID(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{`1`, `1`, true}, {`1`, `1.0`, true}, {`"a"`, `"a"`, true}, {`1`, `"1"`, false}, {`{}`, `{}`, true}, {`2`, `1`, false}} {
		if got := sameID(json.RawMessage(c.a), json.RawMessage(c.b)); got != c.want {
			t.Errorf("sameID(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// The daemon checks Host itself (allowing --public-url), so the SDK must not
// refuse a reverse proxy that connects over loopback with the public Host.
func TestHTTPHandlerBehindReverseProxy(t *testing.T) {
	testkit.GitEnv(t)
	svc := testkit.Service(t, testkit.NewRemote(t, nil).Clone())
	srv := httptest.NewServer(New(svc, Options{}).HTTPHandler())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	req.Host = "ka.example.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Mcp-Session-Id") == "" {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("initialize with a public Host: %d %s", res.StatusCode, b)
	}
}
