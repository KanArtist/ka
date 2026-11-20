package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/testkit"
)

// Compacting rewrites shared history: over REST it needs confirm, and in
// remote mode only the local token may do it (a dry run is open to anyone).
func TestCompactEndpoint(t *testing.T) {
	testkit.GitEnv(t)
	svc := testkit.Service(t, testkit.NewRemote(t, nil).Clone())
	ts := NewTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	tok, _, err := ts.Create("bob", "")
	if err != nil {
		t.Fatal(err)
	}
	h := New(svc, Options{Token: testToken, Version: "test", Remote: &Remote{Tokens: ts, Hosts: []string{"ka.example.com"}}}).Handler()
	call := func(method, path, token string, body map[string]any) (int, map[string]any) {
		var req = httptest.NewRequest(method, "http://ka.example.com/api/v1"+path, nil)
		if body != nil {
			js, _ := json.Marshal(body)
			req = httptest.NewRequest(method, "http://ka.example.com/api/v1"+path, bytes.NewReader(js))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	if code, m := call("POST", "/stores/test/compact", tok, map[string]any{"dry_run": true}); code != 200 || m["dry_run"] != true {
		t.Fatalf("dry run with an API token: %d %v", code, m)
	}
	if code, _ := call("POST", "/stores/test/compact", tok, map[string]any{"confirm": "test"}); code != 403 {
		t.Fatalf("API token: %d", code)
	}
	if code, m := call("POST", "/stores/test/compact", testToken, map[string]any{}); code != 422 {
		t.Fatalf("no confirm: %d %v", code, m)
	}
	code, m := call("POST", "/stores/test/compact", testToken, map[string]any{"confirm": "test", "summary": "All of it."})
	if code != 200 || m["root"] == nil {
		t.Fatalf("local token: %d %v", code, m)
	}
	code, m = call("GET", "/stores/test/history", tok, nil)
	if code != 200 || m["compacted"] != true {
		t.Fatalf("history: %d %v", code, m)
	}
	sums, _ := m["summaries"].([]any)
	if len(sums) != 1 {
		t.Fatalf("summaries: %v", m)
	}
	name, _ := sums[0].(map[string]any)["name"].(string)
	code, m = call("GET", "/stores/test/history/"+name, tok, nil)
	if code != 200 || !strings.Contains(m["body"].(string), "All of it.") {
		t.Fatalf("summary: %d %v", code, m)
	}
}
