package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/engine"
)

// ka project rename: a new key and prefix for every ticket, with references
// rewritten in every store, a commit per store, and the old IDs resolving
// only when the former prefix is kept.
func TestProjectRename(t *testing.T) {
	svc, remotes := layered(t)
	me := svc.CallerFor("bsant", "")
	ids := seedTickets(t, svc,
		engine.CreateIn{Project: "jk", Title: "Parent", Status: "ready"},
		engine.CreateIn{Project: "jk", Title: "Child", Status: "ready"})
	parent, child := ids[0], ids[1]
	if _, err := svc.Update(ctx, me, []engine.UpdateIn{{ID: child, Set: map[string]any{"parent": parent}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Comment(ctx, me, engine.CommentIn{ID: child, Body: "see " + parent + " first"}); err != nil {
		t.Fatal(err)
	}
	gyt := seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "Needs it", Status: "ready", BlockedBy: []string{child},
		Body: "Waiting on " + child + "; not JK-NOPE1 (no such ticket)."})[0]

	dry, err := svc.RenameProject(ctx, me, "jk", "jkx", "JKX", false, true)
	if err != nil || dry.Tickets != 2 || dry.IDs[parent] != "JKX"+parent[2:] {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if _, err := svc.Get(engine.GetIn{ID: parent}); err != nil {
		t.Fatal("the dry run changed something")
	}
	out, err := svc.RenameProject(ctx, me, "jk", "jkx", "JKX", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.PushError != "" || len(out.Pushed) != 2 || out.Pushed[0] != "jk" || out.Commits["gyggly"] == "" {
		t.Fatalf("rename: %+v", out)
	}
	newParent, newChild := "JKX"+parent[2:], "JKX"+child[2:]

	c, err := svc.Get(engine.GetIn{ID: newChild, Include: []string{"comments"}})
	if err != nil || c.ParentRef == nil || c.ParentRef.ID != newParent || !strings.Contains(c.Comments[0].Body, newParent) {
		t.Fatalf("child after the rename: %+v %v", c, err)
	}
	g, _ := svc.Get(engine.GetIn{ID: gyt})
	if len(g.BlockedBy) != 1 || g.BlockedBy[0].ID != newChild || !strings.Contains(g.Body, newChild) || !strings.Contains(g.Body, "JK-NOPE1") {
		t.Fatalf("cross-store references: %+v %q", g.BlockedBy, g.Body)
	}
	for _, d := range svc.Validate(svc.Now()) {
		if refCodes[d.Code] {
			t.Fatalf("ka check after the rename: %v", d)
		}
	}
	if _, err := svc.Get(engine.GetIn{ID: parent}); err == nil {
		t.Fatal("the old ID resolves without --keep-former-prefix")
	}
	// The renamed project reached its remote.
	clone := remotes["jk"].Clone()
	if _, err := os.Stat(filepath.Join(clone, "projects", "jkx", "tickets", newParent, "ticket.md")); err != nil {
		t.Fatalf("remote layout: %v", err)
	}
	pf, _ := os.ReadFile(filepath.Join(clone, "projects", "jkx", "project.yml"))
	if !strings.Contains(string(pf), "key: jkx") || !strings.Contains(string(pf), "prefix: JKX") || strings.Contains(string(pf), "former_prefixes") {
		t.Fatalf("project.yml: %s", pf)
	}

	// With --keep-former-prefix, old IDs keep working, for reads and writes.
	if _, err := svc.RenameProject(ctx, me, "jkx", "", "JKY", true, false); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Get(engine.GetIn{ID: newParent})
	if err != nil || d.ID != "JKY"+parent[2:] {
		t.Fatalf("former prefix lookup: %+v %v", d, err)
	}
	if _, err := svc.Claim(ctx, me, newParent, false); err != nil {
		t.Fatalf("claim by a former ID: %v", err)
	}
	for _, v := range svc.Validate(svc.Now()) {
		if refCodes[v.Code] {
			t.Fatalf("ka check with former_prefixes: %v", v)
		}
	}

	// Refused: an existing prefix, a bad key, nothing to do.
	for _, tc := range [][2]string{{"jkx2", "GYG"}, {"Bad_Key", ""}, {"jkx", "JKY"}} {
		if _, err := svc.RenameProject(ctx, me, "jkx", tc[0], tc[1], false, true); err == nil {
			t.Fatalf("rename to %v should fail", tc)
		}
	}
}

// refCodes are the diagnostics a rename could cause (the layered fixture
// has unrelated, deliberate config errors).
var refCodes = map[string]bool{"dangling_ref": true, "external_ref": true, "id_prefix_mismatch": true, "duplicate_prefix": true, "self_ref": true}
