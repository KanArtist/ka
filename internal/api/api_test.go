package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/testkit"
)

const testToken = "test-token"

type client struct {
	t    *testing.T
	base string
	srv  *Server
	svc  *engine.Service
}

func start(t *testing.T) *client {
	t.Helper()
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	svc := testkit.Service(t, r.Clone())
	return serve(t, svc)
}

func serve(t *testing.T, svc *engine.Service) *client {
	t.Helper()
	return serveWith(t, svc, Options{})
}

func serveWith(t *testing.T, svc *engine.Service, opts Options) *client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	opts.Token, opts.Version, opts.Shutdown = testToken, "test", cancel
	srv := New(svc, opts)
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &client{t: t, base: "http://" + ln.Addr().String(), srv: srv, svc: svc}
}

type resp struct {
	status int
	hdr    http.Header
	raw    []byte
	m      map[string]any
}

func (c *client) do(method, path string, body any, hdr ...string) resp {
	c.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		js, _ := json.Marshal(b)
		rd = bytes.NewReader(js)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil {
		if _, ok := body.([]byte); ok {
			req.Header.Set("Content-Type", "text/markdown")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, hdr: res.Header, raw: raw}
	json.Unmarshal(raw, &out.m)
	return out
}

func (c *client) ok(method, path string, body any, hdr ...string) resp {
	c.t.Helper()
	r := c.do(method, path, body, hdr...)
	if r.status >= 300 {
		c.t.Fatalf("%s %s: HTTP %d %s", method, path, r.status, r.raw)
	}
	return r
}

func str(m map[string]any, keys ...string) string {
	var v any = m
	for _, k := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = mm[k]
	}
	s, _ := v.(string)
	return s
}

// Requests without the token, or from another origin or host name, are refused.
func TestSecurity(t *testing.T) {
	c := start(t)
	cases := []struct {
		name   string
		hdr    []string
		status int
		code   string
	}{
		{"no token", []string{"Authorization", ""}, 401, "unauthorized"},
		{"wrong token", []string{"Authorization", "Bearer nope"}, 401, "unauthorized"},
		{"foreign origin", []string{"Origin", "https://evil.example"}, 403, "forbidden"},
		{"null origin", []string{"Origin", "null"}, 403, "forbidden"},
		{"other port origin", []string{"Origin", "http://127.0.0.1:1"}, 403, "forbidden"},
		{"cross-site fetch", []string{"Sec-Fetch-Site", "cross-site"}, 403, "forbidden"},
	}
	for _, tc := range cases {
		r := c.do("GET", "/api/v1/workspace", nil, tc.hdr...)
		if r.status != tc.status || str(r.m, "code") != tc.code || !strings.HasPrefix(r.hdr.Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: HTTP %d %s", tc.name, r.status, r.raw)
		}
	}
	// DNS rebinding: a foreign Host header is refused even with the token.
	req, _ := http.NewRequest("GET", c.base+"/api/v1/workspace", nil)
	req.Host = "evil.example:" + strings.Split(c.base, ":")[2]
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 403 {
		t.Fatalf("foreign Host: %v %v", res.StatusCode, err)
	}
	res.Body.Close()
	// The same origin is fine, and so is localhost.
	c.ok("GET", "/api/v1/workspace", nil, "Origin", c.base)
	c.ok("GET", "/api/v1/workspace", nil, "Origin", strings.Replace(c.base, "127.0.0.1", "localhost", 1))
	// The OpenAPI document needs the token too; /healthz does not.
	if r := c.do("GET", "/api/v1/openapi.json", nil, "Authorization", ""); r.status != 401 {
		t.Fatalf("openapi without token: %d", r.status)
	}
	if r := c.do("GET", "/healthz", nil, "Authorization", ""); r.status != 200 || str(r.m, "status") != "ok" {
		t.Fatalf("healthz: %d %s", r.status, r.raw)
	}

	// The token exchange sets an HttpOnly, SameSite=Strict cookie and
	// redirects only to local paths.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = noRedirect.Get(c.base + "/auth?token=" + testToken + "&next=//evil.example/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		t.Fatalf("auth redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, ck := range res.Cookies() {
		if ck.Name == CookieName {
			cookie = ck
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", cookie)
	}
	req, _ = http.NewRequest("GET", c.base+"/api/v1/workspace", nil)
	req.AddCookie(cookie)
	if res, err = http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Fatalf("cookie auth: %v %v", res.StatusCode, err)
	}
	res.Body.Close()
	if res, _ = noRedirect.Get(c.base + "/auth?token=wrong"); res.StatusCode != 401 {
		t.Fatalf("auth with a wrong token: %d", res.StatusCode)
	}
	res.Body.Close()
}

// The OpenAPI document validates against the official OpenAPI 3.1 schema
// (the schema-base variant, which also checks every Schema object).
func TestOpenAPIValidates(t *testing.T) {
	c := start(t)
	r := c.ok("GET", "/api/v1/openapi.json", nil)
	if str(r.m, "openapi") != "3.1.0" {
		t.Fatalf("openapi version: %q", str(r.m, "openapi"))
	}
	comp := jsonschema.NewCompiler()
	for _, f := range []string{"oas-3.1-schema.json", "oas-3.1-schema-base.json", "oas-3.1-dialect-base.json", "oas-3.1-meta-base.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		if err := comp.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
	}
	sch, err := comp.Compile("https://spec.openapis.org/oas/3.1/schema-base/2022-10-07")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(r.raw))
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("openapi.json is not valid OpenAPI 3.1: %v", err)
	}
	// Every operation documents the problem+json error shape.
	paths := r.m["paths"].(map[string]any)
	for p, item := range paths {
		for method, op := range item.(map[string]any) {
			resps, _ := op.(map[string]any)["responses"].(map[string]any)
			if resps["default"] == nil {
				t.Errorf("%s %s: no default (error) response", method, p)
			}
		}
	}
}

// Every exported engine.Service method is reachable over REST (or is
// plumbing with no meaning over HTTP).
func TestEveryServiceMethodHasAnEndpoint(t *testing.T) {
	plumbing := map[string]string{
		"Index": "internal", "Human": "GET /workspace (you)", "CallerFor": "X-Ka-Actor", "Reload": "internal",
		"TicketPath": "GET /tickets/{id}/source", "BoardFilter": "GET /boards/{name}", "Projects": "GET /workspace",
		"ETag": "ETag headers", "SetEvents": "GET /events", "WatchLocal": "daemon", "FetchLoop": "daemon", "Flush": "daemon",
	}
	endpoints := map[string]string{
		"Search": "listTickets", "Get": "getTicket", "Create": "createTickets", "Update": "updateTickets", "Claim": "claimTicket",
		"Release": "releaseTicket", "Transition": "transitionTicket", "Comment": "createComment", "CheckItems": "checkAcceptance",
		"Ask": "askQuestion", "Answer": "answerQuestion", "Inbox": "getInbox", "Next": "next", "Schema": "getSchema",
		"Sync": "sync", "ResolveLink": "resolveLink", "Validate": "check", "ReplaceTicket": "replaceTicketSource", "Fix": "fix",
		"WorkspaceInfo": "getWorkspace", "Stores": "listStores", "Boards": "listBoards", "Actors": "listActors", "Kick": "gitHook",
	}
	ops := map[string]bool{}
	for _, item := range New(nil, Options{}).OpenAPI().Paths {
		for _, o := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if o != nil {
				ops[o.OperationID] = true
			}
		}
	}
	typ := reflect.TypeOf(&engine.Service{})
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i).Name
		if _, ok := plumbing[m]; ok {
			continue
		}
		op, ok := endpoints[m]
		if !ok {
			t.Errorf("engine.Service.%s has no REST endpoint: add one (and list it here)", m)
			continue
		}
		if !ops[op] {
			t.Errorf("engine.Service.%s: operation %s is not registered", m, op)
		}
	}
}

func TestTicketLifecycle(t *testing.T) {
	c := start(t)
	agent := []string{"X-Ka-Actor", "agent/claude", "X-Ka-Session", "s1"}

	r := c.do("POST", "/api/v1/tickets", map[string]any{"title": "Fix the flaky cache test", "kind": "bug", "priority": "P1", "status": "ready", "acceptance": []string{"passes 100 runs"}})
	if r.status != 201 {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	id := str(r.m["created"].([]any)[0].(map[string]any), "id")
	r = c.ok("POST", "/api/v1/tickets/batch", map[string]any{"tickets": []map[string]any{{"title": "Follow-up A", "parent": id}, {"title": "Follow-up B", "parent": id}}}, agent...)
	if n := len(r.m["created"].([]any)); n != 2 {
		t.Fatalf("batch create: %s", r.raw)
	}

	// Validation errors use the engine's codes, with a fix.
	r = c.do("POST", "/api/v1/tickets", map[string]any{"title": "x", "kind": "bugg"})
	if r.status != 422 || str(r.m, "code") != "unknown_kind" || str(r.m, "fix") == "" {
		t.Fatalf("unknown kind: %d %s", r.status, r.raw)
	}
	r = c.do("POST", "/api/v1/tickets", map[string]any{"kind": "bug"})
	if r.status != 422 || str(r.m, "code") != "invalid_argument" {
		t.Fatalf("missing title: %d %s", r.status, r.raw)
	}
	if r = c.do("GET", "/api/v1/tickets/JK-ZZZZZ", nil); r.status != 404 || str(r.m, "code") != "not_found" {
		t.Fatalf("missing ticket: %d %s", r.status, r.raw)
	}

	r = c.ok("GET", "/api/v1/tickets?q=kind:bug", nil)
	if r.m["total"].(float64) != 1 {
		t.Fatalf("search: %s", r.raw)
	}
	r = c.ok("GET", "/api/v1/search?q=follow-up&limit=-1", nil)
	if r.m["total"].(float64) != 2 || len(r.m["tickets"].([]any)) != 2 {
		t.Fatalf("search all: %s", r.raw)
	}

	// Field patches need no precondition.
	r = c.ok("PATCH", "/api/v1/tickets/"+id, map[string]any{"set": map[string]any{"priority": "P0"}, "add": map[string]any{"labels": []string{"cache"}}})
	if r.m["commit"] == nil {
		t.Fatalf("patch: %s", r.raw)
	}
	r = c.ok("GET", "/api/v1/tickets/"+id, nil)
	tag := r.hdr.Get("ETag")
	if str(r.m, "priority") != "P0" || tag == "" || len(r.m["children"].([]any)) != 2 {
		t.Fatalf("get: %s %q", r.raw, tag)
	}
	// Body edits need If-Match, and a stale one is refused.
	newBody := "## Problem\n\nThe cache test flakes.\n"
	if r = c.do("PATCH", "/api/v1/tickets/"+id, map[string]any{"body": newBody}); r.status != 428 || str(r.m, "code") != "if_match_required" {
		t.Fatalf("body without If-Match: %d %s", r.status, r.raw)
	}
	if r = c.do("PATCH", "/api/v1/tickets/"+id, map[string]any{"body": newBody}, "If-Match", `"0000"`); r.status != 412 || str(r.m, "code") != "edit_conflict" {
		t.Fatalf("stale If-Match: %d %s", r.status, r.raw)
	}
	c.ok("PATCH", "/api/v1/tickets/"+id, map[string]any{"body": newBody}, "If-Match", tag)
	if r = c.do("PATCH", "/api/v1/tickets/"+id, map[string]any{"body": "again\n"}, "If-Match", tag); r.status != 412 {
		t.Fatalf("reused If-Match after a change: %d %s", r.status, r.raw)
	}

	// Raw source round trip.
	r = c.ok("GET", "/api/v1/tickets/"+id+"/source", nil)
	if !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/markdown") || !bytes.Contains(r.raw, []byte("The cache test flakes.")) {
		t.Fatalf("source: %q %s", r.hdr.Get("Content-Type"), r.raw)
	}
	edited := bytes.Replace(r.raw, []byte("The cache test flakes."), []byte("The cache test flakes under load."), 1)
	if r2 := c.do("PUT", "/api/v1/tickets/"+id+"/source", edited); r2.status != 428 && r2.status != 422 {
		t.Fatalf("source PUT without If-Match: %d %s", r2.status, r2.raw)
	}
	c.ok("PUT", "/api/v1/tickets/"+id+"/source", edited, "If-Match", r.hdr.Get("ETag"))
	if d, _ := c.svc.Get(engine.GetIn{ID: id}); !strings.Contains(d.Body, "under load") {
		t.Fatalf("source edit not applied: %q", d.Body)
	}

	// The agent works the ticket.
	r = c.ok("POST", "/api/v1/tickets/"+id+"/claim", map[string]any{}, agent...)
	if r.m["confirmed"] != true {
		t.Fatalf("claim: %s", r.raw)
	}
	if r = c.do("POST", "/api/v1/tickets/"+id+"/claim", map[string]any{}, "X-Ka-Actor", "agent/codex"); r.status != 409 || str(r.m, "code") != "claim_lost" {
		t.Fatalf("second claim: %d %s", r.status, r.raw)
	}
	c.ok("POST", "/api/v1/tickets/"+id+"/comments", map[string]any{"type": "progress", "body": "Reproduced the flake."}, agent...)
	if r = c.do("POST", "/api/v1/tickets/"+id+"/comments", map[string]any{"type": "progress", "body": "Again."}, agent...); r.status != 429 || str(r.m, "code") != "progress_throttled" {
		t.Fatalf("throttle: %d %s", r.status, r.raw)
	}
	r = c.ok("POST", "/api/v1/tickets/"+id+"/acceptance", map[string]any{"items": []map[string]any{{"index": 0, "done": true}}}, agent...)
	if str(r.m, "acceptance") != "1/1" {
		t.Fatalf("check: %s", r.raw)
	}
	if r = c.do("POST", "/api/v1/tickets/"+id+"/transition", map[string]any{"status": "done"}, agent...); r.status != 422 || str(r.m, "code") != "resolution_required" {
		t.Fatalf("close without summary: %d %s", r.status, r.raw)
	}
	if r = c.do("POST", "/api/v1/tickets/"+id+"/transition", map[string]any{"status": "doing"}, agent...); r.status != 422 || !strings.Contains(str(r.m, "fix"), "in_progress") {
		t.Fatalf("typo status: %d %s", r.status, r.raw)
	}
	c.ok("POST", "/api/v1/tickets/"+id+"/transition", map[string]any{"status": "done", "summary": "Fixed the eviction race."}, agent...)
	r = c.ok("GET", "/api/v1/tickets/"+id+"/comments", nil)
	cs := r.m["comments"].([]any)
	types := map[string]bool{}
	for _, cm := range cs {
		types[str(cm.(map[string]any), "type")] = true
	}
	if len(cs) != 2 || !types["resolution"] || !types["progress"] {
		t.Fatalf("comments: %s", r.raw)
	}
	// Commits record the agent, the human it works for and the session.
	log := testkit.Git(t, c.svc.WS.Stores[0].Root, "log", "-1", "--format=%B")
	for _, want := range []string{"Ka-Actor: agent/claude", "Ka-On-Behalf-Of: tester", "Ka-Session: s1"} {
		if !strings.Contains(log, want) {
			t.Fatalf("trailers missing %q:\n%s", want, log)
		}
	}

	// Batch patch, next, release.
	kids := c.ok("GET", "/api/v1/tickets?q=has:parent", nil).m["tickets"].([]any)
	var ups []map[string]any
	for _, k := range kids {
		kid := str(k.(map[string]any), "id")
		ups = append(ups, map[string]any{"id": kid, "set": map[string]any{"priority": "P2"}})
		c.ok("POST", "/api/v1/tickets/"+kid+"/transition", map[string]any{"status": "ready"})
	}
	if r = c.ok("PATCH", "/api/v1/tickets", map[string]any{"updates": ups}); len(r.m["results"].([]any)) != 2 {
		t.Fatalf("batch patch: %s", r.raw)
	}
	r = c.ok("POST", "/api/v1/next", map[string]any{}, agent...)
	next := str(r.m, "ticket", "id")
	if r.m["claimed"] != true || next == "" {
		t.Fatalf("next: %s", r.raw)
	}
	c.ok("POST", "/api/v1/tickets/"+next+"/release", map[string]any{"status": "ready"}, agent...)
	if r = c.do("POST", "/api/v1/next", map[string]any{"query": "label:nothing-has-this"}, agent...); r.status != 404 || str(r.m, "code") != "no_candidates" {
		t.Fatalf("next without candidates: %d %s", r.status, r.raw)
	}
}

func TestQuestionsAndWorkspace(t *testing.T) {
	c := start(t)
	agent := []string{"X-Ka-Actor", "agent/claude"}
	id := str(c.ok("POST", "/api/v1/tickets", map[string]any{"title": "Drop the legacy table", "status": "ready", "owner": "tester"}).m["created"].([]any)[0].(map[string]any), "id")

	r := c.ok("POST", "/api/v1/tickets/"+id+"/questions", map[string]any{"question": "Drop users_old?", "kind": "approval", "to": "tester"}, agent...)
	if r.m["answered"] != false {
		t.Fatalf("ask: %s", r.raw)
	}
	r = c.ok("GET", "/api/v1/inbox", nil)
	if len(r.m["questions"].([]any)) != 1 {
		t.Fatalf("human inbox: %s", r.raw)
	}
	c.ok("POST", "/api/v1/tickets/"+id+"/answers", map[string]any{"approved": true, "text": "go ahead"})
	r = c.ok("GET", "/api/v1/inbox?since=1h", nil, agent...)
	ans := r.m["answers"].([]any)
	if len(ans) != 1 || ans[0].(map[string]any)["comment"].(map[string]any)["approved"] != true {
		t.Fatalf("agent inbox: %s", r.raw)
	}
	if r = c.do("POST", "/api/v1/tickets/"+id+"/answers", map[string]any{"text": "again"}); r.status != 404 || str(r.m, "code") != "no_open_question" {
		t.Fatalf("answer with nothing open: %d %s", r.status, r.raw)
	}

	r = c.ok("GET", "/api/v1/workspace", nil)
	if str(r.m, "you") != "tester" || len(r.m["stores"].([]any)) != 1 {
		t.Fatalf("workspace: %s", r.raw)
	}
	r = c.ok("GET", "/api/v1/projects/jk", nil)
	if str(r.m, "prefix") != "JK" || str(r.m["statuses"].([]any)[0].(map[string]any), "name") == "" {
		t.Fatalf("project: %s", r.raw)
	}
	if r = c.do("GET", "/api/v1/projects/nope", nil); r.status != 404 {
		t.Fatalf("unknown project: %d %s", r.status, r.raw)
	}
	c.ok("GET", "/api/v1/projects", nil)
	c.ok("GET", "/api/v1/schema?project=jk", nil)
	r = c.ok("GET", "/api/v1/stores", nil)
	if str(r.m["stores"].([]any)[0].(map[string]any), "state") != "synced" {
		t.Fatalf("stores: %s", r.raw)
	}
	c.ok("GET", "/api/v1/sync", nil)
	r = c.ok("POST", "/api/v1/sync", map[string]any{"force": true})
	if len(r.m["stores"].([]any)) != 1 {
		t.Fatalf("sync: %s", r.raw)
	}
	r = c.ok("GET", "/api/v1/check", nil)
	if r.m["errors"].(float64) != 0 {
		t.Fatalf("check: %s", r.raw)
	}
	c.ok("POST", "/api/v1/fix", map[string]any{"dry_run": true})
	c.ok("GET", "/api/v1/actors", nil)
	c.ok("GET", "/api/v1/boards", nil)
	if r = c.do("GET", "/api/v1/boards/nope", nil); r.status != 404 {
		t.Fatalf("unknown board: %d", r.status)
	}
	if r = c.do("GET", "/api/v1/links/resolve", nil); r.status != 422 {
		t.Fatalf("resolve without link: %d %s", r.status, r.raw)
	}
	r = c.ok("GET", "/api/v1/daemon", nil)
	if str(r.m, "version") != "test" {
		t.Fatalf("daemon: %s", r.raw)
	}
}

func TestBoards(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, map[string]string{"boards/bugs.yml": "name: Bugs\nfilter: kind:bug\n"})
	c := serve(t, testkit.Service(t, r.Clone()))
	c.ok("POST", "/api/v1/tickets/batch", map[string]any{"tickets": []map[string]any{{"title": "A bug", "kind": "bug"}, {"title": "A feature", "kind": "feature"}}})
	res := c.ok("GET", "/api/v1/boards", nil)
	if bs := res.m["boards"].([]any); len(bs) != 1 || str(bs[0].(map[string]any), "filter") != "kind:bug" {
		t.Fatalf("boards: %s", res.raw)
	}
	res = c.ok("GET", "/api/v1/boards/bugs", nil)
	if res.m["tickets"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("board tickets: %s", res.raw)
	}
}

func TestRuntimeFileAndShutdown(t *testing.T) {
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	c := start(t)
	addr := strings.TrimPrefix(c.base, "http://")
	rt := &Runtime{PID: os.Getpid(), Addr: addr, URL: c.base, Workspace: c.svc.WS.Source, WorkspaceHash: WorkspaceHash(c.svc.WS.Source), Token: testToken}
	if err := WriteRuntime(rt); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(RuntimePath(rt.WorkspaceHash)); err != nil || (fi.Mode().Perm()&0o077 != 0 && !isWindows()) {
		t.Fatalf("runtime file: %v %v", fi.Mode(), err)
	}
	got := Discover(context.Background(), c.svc.WS.Source)
	if got == nil || got.Token != testToken {
		t.Fatalf("discover: %+v", got)
	}
	// A runtime file pointing at a dead address is ignored.
	stale := *rt
	stale.Addr = "127.0.0.1:1"
	if Probe(context.Background(), &stale) {
		t.Fatal("probe of a dead address succeeded")
	}
	if r := c.do("POST", "/api/v1/daemon/shutdown", nil); r.status != 202 {
		t.Fatalf("shutdown: %d %s", r.status, r.raw)
	}
	RemoveRuntime(rt)
	if _, err := os.Stat(RuntimePath(rt.WorkspaceHash)); !os.IsNotExist(err) {
		t.Fatalf("runtime file not removed: %v", err)
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	if _, err := Listen("0.0.0.0:0"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Listen(0.0.0.0): %v", err)
	}
}

func isWindows() bool { return runtime.GOOS == "windows" }
