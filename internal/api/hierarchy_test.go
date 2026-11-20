package api

import (
	"testing"

	"github.com/KanArtist/ka/internal/testkit"
)

func TestTreeAndFromLabel(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, map[string]string{".ka/store.yml": "spec: 1\nname: test\nrequires_ka: 1.3.0\n"})
	c := serve(t, testkit.Service(t, r.Clone()))

	c.ok("POST", "/api/v1/tickets/batch", map[string]any{"tickets": []map[string]any{
		{"title": "Port the installer", "labels": []string{"windows"}, "status": "ready"},
		{"title": "Sign the binaries", "labels": []string{"windows"}, "status": "done"},
	}})
	if res := c.do("POST", "/api/v1/initiatives/from-label", map[string]any{"label": "windows", "dry_run": true}); res.status != 200 || res.m["created"] != true || len(res.m["tickets"].([]any)) != 2 {
		t.Fatalf("dry run: %d %s", res.status, res.raw)
	}
	res := c.ok("POST", "/api/v1/initiatives/from-label", map[string]any{"label": "windows", "title": "Windows support"})
	id := str(res.m, "initiative", "id")
	if id == "" || res.m["created"] != true {
		t.Fatalf("from-label: %s", res.raw)
	}
	res = c.ok("GET", "/api/v1/tree?id="+id, nil)
	root := res.m["nodes"].([]any)[0].(map[string]any)
	ru := root["rollup"].(map[string]any)
	if root["kind"] != "initiative" || ru["total"].(float64) != 2 || ru["done"].(float64) != 1 || ru["ready"].(float64) != 1 || len(root["children"].([]any)) != 2 {
		t.Fatalf("tree: %s", res.raw)
	}
	res = c.ok("GET", "/api/v1/tree?hide_closed=true", nil)
	if n := res.m["nodes"].([]any); len(n) != 1 || len(n[0].(map[string]any)["children"].([]any)) != 1 {
		t.Fatalf("project tree: %s", res.raw)
	}
	if res = c.do("POST", "/api/v1/tickets", map[string]any{"title": "Nested", "kind": "initiative", "parent": id}); res.status != 422 || str(res.m, "code") != "hierarchy_invalid" {
		t.Fatalf("nested initiative: %d %s", res.status, res.raw)
	}
	res = c.ok("POST", "/api/v1/next", map[string]any{"within": id, "peek": true})
	if str(res.m, "ticket", "title") != "Port the installer" {
		t.Fatalf("next within: %s", res.raw)
	}
}
