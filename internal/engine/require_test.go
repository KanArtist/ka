package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireKa(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": "spec: 1\nname: test\n# who may use it\nextends:\n  - url: ../org\n"})
	a, dir := e.clone()
	c := agent("claude")
	for _, v := range []string{"1.3", "latest", ""} {
		if _, err := a.RequireKa(ctx, c, "", v); !IsCode(err, CodeInvalid) {
			t.Errorf("version %q: %v", v, err)
		}
	}
	withKaVersion(t, "1.2.0")
	if _, err := a.RequireKa(ctx, c, "", "1.3.0"); !IsCode(err, CodeInvalid) || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("newer than this ka: %v", err)
	}
	withKaVersion(t, "1.3.0")
	out := must[*RequireOut](t)(a.RequireKa(ctx, c, "test", "1.2.0"))
	if !out.Changed || out.From != "" || !out.Pushed {
		t.Fatalf("raise: %+v", out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".ka", "store.yml"))
	if string(raw) != "spec: 1\nname: test\n# who may use it\nrequires_ka: 1.2.0\nextends:\n  - url: ../org\n" {
		t.Fatalf("store.yml:\n%s", raw)
	}
	out = must[*RequireOut](t)(a.RequireKa(ctx, c, "", "1.3.0"))
	if !out.Changed || out.From != "1.2.0" || out.To != "1.3.0" {
		t.Fatalf("raise again: %+v", out)
	}
	if _, err := a.RequireKa(ctx, c, "", "1.2.0"); !IsCode(err, CodeInvalid) || !strings.Contains(err.Error(), "never lowered") {
		t.Fatalf("lower: %v", err)
	}
	if out := must[*RequireOut](t)(a.RequireKa(ctx, c, "", "1.3.0")); out.Changed {
		t.Fatalf("same version: %+v", out)
	}
	// The index sees the new requirement: containers are allowed now.
	must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "GDPR", Kind: "initiative"}}))
	if log := run(t, dir, "log", "-1", "--format=%s%n%b", "--", ".ka/store.yml"); !strings.Contains(log, "store: require ka 1.3.0") || !strings.Contains(log, "Ka-Actor: agent/claude") {
		t.Fatalf("commit:\n%s", log)
	}
}

// A hierarchy rule broken by a concurrent change makes the replayed
// operation fail like any other: it is preserved, and the rest of the
// replay goes through.
func TestHierarchyViolationOnReplay(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/store.yml": containersStore})
	a, _ := e.clone()
	b, _ := e.clone()
	c := agent("claude")
	ids := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "GDPR", Kind: "initiative"}, {Title: "Consent", Kind: "epic"}, {Title: "Other"}})).Created
	ini, epic, other := ids[0].ID, ids[1].ID, ids[2].ID
	b.Sync(ctx, true)

	// Bob turns the initiative into a feature and pushes first.
	must[[]*WriteOut](t)(b.Update(ctx, c, []UpdateIn{{ID: ini, Set: map[string]any{"kind": "feature"}}}))
	// Alice, offline, puts the epic under it and edits another ticket.
	a.NoPush = true
	must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: epic, Set: map[string]any{"parent": ini}}, {ID: other, Set: map[string]any{"priority": "P1"}}}))
	a.NoPush = false
	so := a.Sync(ctx, true)
	if len(so.Errors) > 0 || len(so.Preserved) != 1 {
		t.Fatalf("sync: %+v", so)
	}
	if d := must[*Detail](t)(a.Get(GetIn{ID: epic})); d.ParentRef != nil {
		t.Fatalf("epic put under a feature: %+v", d.ParentRef)
	}
	if d := must[*Detail](t)(a.Get(GetIn{ID: other})); d.Priority != "P1" {
		t.Fatalf("the rest of the replay was lost: %+v", d.Summary)
	}
	if !strings.Contains(must[*Detail](t)(a.Get(GetIn{ID: epic, Include: []string{"comments"}})).Comments[0].Body, "hierarchy_invalid") {
		t.Fatal("preserved comment does not say why")
	}
}
