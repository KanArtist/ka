package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/testkit"
)

// Remote mode (PRD §15): API tokens, trusted proxy headers, the Host and
// Origin rules for a public URL, and a principal clients cannot forge.
func TestRemoteMode(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, map[string]string{".ka/actors.yml": "humans:\n  alice:\n    name: Alice\n    emails:\n      - alice@example.com\n"})
	svc := testkit.Service(t, r.Clone())
	ts := NewTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	tok, _, err := ts.Create("bob", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	cidrs, _ := ParseCIDRs([]string{"10.0.0.0/8"})
	remote := &Remote{Tokens: ts, TrustedProxies: cidrs, Hosts: []string{"ka.example.com"}}
	if !remote.Configured() {
		t.Fatal("not configured")
	}
	srv := New(svc, Options{Token: testToken, Version: "test", Remote: remote})
	h := srv.Handler()

	send := func(method, path, host, remoteAddr string, hdr map[string]string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, "http://"+host+path, bytes.NewReader(b))
		req.Host = host
		req.RemoteAddr = remoteAddr
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	createdBy := func(rec *httptest.ResponseRecorder) (string, string) {
		t.Helper()
		if rec.Code != 201 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
		var out struct {
			Created []struct{ ID string } `json:"created"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		d, err := svc.Get(engineGet(out.Created[0].ID))
		if err != nil {
			t.Fatal(err)
		}
		return d.CreatedBy, out.Created[0].ID
	}
	const host = "ka.example.com"
	ticket := map[string]any{"title": "Remote ticket"}

	// An API token acts as its actor.
	by, id := createdBy(send("POST", "/api/v1/tickets", host, "203.0.113.5:4000", map[string]string{"Authorization": "Bearer " + tok}, ticket))
	if by != "bob" {
		t.Fatalf("token actor: %q", by)
	}
	// An agent with that token acts on bob's behalf.
	rec := send("POST", "/api/v1/tickets/"+id+"/claim", host, "203.0.113.5:4000", map[string]string{"Authorization": "Bearer " + tok, "X-Ka-Actor": "agent/claude"}, map[string]any{})
	if rec.Code != 200 {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	if d, _ := svc.Get(engineGet(id)); d.Claim == nil || d.Claim.By != "agent/claude" {
		t.Fatalf("claim: %+v", d.Claim)
	}
	// A client cannot forge the principal header.
	by, _ = createdBy(send("POST", "/api/v1/tickets", host, "203.0.113.5:4000", map[string]string{"Authorization": "Bearer " + tok, PrincipalHeader: "mallory"}, ticket))
	if by != "bob" {
		t.Fatalf("forged principal accepted: %q", by)
	}
	// A trusted proxy's user header, mapped through the actor registry.
	by, _ = createdBy(send("POST", "/api/v1/tickets", host, "10.1.2.3:5000", map[string]string{"X-Forwarded-User": "alice@example.com"}, ticket))
	if by != "alice" {
		t.Fatalf("proxy user: %q", by)
	}
	// The same header from anywhere else is ignored.
	if rec := send("GET", "/api/v1/workspace", host, "203.0.113.9:5000", map[string]string{"X-Forwarded-User": "alice"}, nil); rec.Code != 401 {
		t.Fatalf("untrusted proxy header: %d", rec.Code)
	}
	if rec := send("GET", "/api/v1/workspace", host, "203.0.113.9:5000", map[string]string{"Authorization": "Bearer ka_wrong"}, nil); rec.Code != 401 {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	// Host and Origin: only the public host, and same-origin browser requests.
	if rec := send("GET", "/api/v1/workspace", "evil.example:80", "203.0.113.5:4000", map[string]string{"Authorization": "Bearer " + tok}, nil); rec.Code != 403 {
		t.Fatalf("foreign Host: %d", rec.Code)
	}
	if rec := send("GET", "/api/v1/workspace", host, "203.0.113.5:4000", map[string]string{"Authorization": "Bearer " + tok, "Origin": "https://ka.example.com"}, nil); rec.Code != 200 {
		t.Fatalf("same-origin: %d %s", rec.Code, rec.Body)
	}
	if rec := send("GET", "/api/v1/workspace", host, "203.0.113.5:4000", map[string]string{"Authorization": "Bearer " + tok, "Origin": "https://evil.example"}, nil); rec.Code != 403 {
		t.Fatalf("cross-origin: %d", rec.Code)
	}
	// The browser sign-in takes an API token and sets a secure cookie behind TLS.
	rec = send("GET", "/auth?token="+tok+"&next=/board", host, "203.0.113.5:4000", map[string]string{"X-Forwarded-Proto": "https"}, nil)
	cookie := rec.Header().Get("Set-Cookie")
	if rec.Code != http.StatusSeeOther || !strings.Contains(cookie, tok) || !strings.Contains(cookie, "Secure") || !strings.Contains(cookie, "HttpOnly") {
		t.Fatalf("auth: %d %q", rec.Code, cookie)
	}
	// A removed token stops working.
	list := ts.List()
	if err := ts.Remove(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if rec := send("GET", "/api/v1/workspace", host, "203.0.113.5:4000", map[string]string{"Authorization": "Bearer " + tok}, nil); rec.Code != 401 {
		t.Fatalf("removed token still works: %d", rec.Code)
	}
}

func TestRemoteNeedsAuth(t *testing.T) {
	if (&Remote{Tokens: NewTokenStore(filepath.Join(t.TempDir(), "none.json"))}).Configured() {
		t.Fatal("a remote without tokens or proxies is not configured")
	}
}

func engineGet(id string) engine.GetIn { return engine.GetIn{ID: id} }

func TestGitHubWebhook(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	svc := testkit.Service(t, r.Clone())
	const secret = "s3cret"
	h := New(svc, Options{Token: testToken, Version: "test", WebhookSecret: secret}).Handler()
	hook := func(event string, body []byte, sig string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://127.0.0.1:7711"+GitHubHookPath, bytes.NewReader(body))
		req.Host = "127.0.0.1:0"
		req.Header.Set("X-GitHub-Event", event)
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	sign := func(b []byte) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(b)
		return "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	push := []byte(`{"ref":"refs/heads/main","repository":{"clone_url":"` + strings.ReplaceAll(r.Path, `\`, `\\`) + `/","ssh_url":"git@example.com:x/y.git"}}`)
	if rec := hook("push", push, ""); rec.Code != 401 {
		t.Fatalf("unsigned: %d", rec.Code)
	}
	if rec := hook("push", push, "sha256=00"); rec.Code != 401 {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	rec := hook("push", push, sign(push))
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"fetching":["`+svc.WS.Stores[0].Name+`"]`) {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	ping := []byte(`{"zen":"hi"}`)
	if rec := hook("ping", ping, sign(ping)); rec.Code != 202 {
		t.Fatalf("ping: %d", rec.Code)
	}
	// Without a secret configured, webhooks are refused.
	h = New(svc, Options{Token: testToken, Version: "test"}).Handler()
	if rec := hook("push", push, sign(push)); rec.Code != 403 {
		t.Fatalf("unconfigured: %d", rec.Code)
	}
}
