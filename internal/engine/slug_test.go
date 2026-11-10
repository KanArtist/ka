package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KanArtist/ka/pkg/kaspec"
)

const guardedStore = "spec: 1\nname: test\nrequires_ka: 1.2.0\n"

func TestSlugsOffByDefault(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	c := agent("claude")

	// A given slug is checked but not stored while the project has slugs off;
	// the reference shows one derived from the title.
	if _, err := a.Create(ctx, c, []CreateIn{{Title: "x", Slug: "this-slug-is-far-too-long-to-keep"}}); !IsCode(err, CodeInvalid) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over-long slug: %v", err)
	}
	out := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Fix the compiler warnings", Slug: "warnings", Status: "ready"}}))
	id, ref := out.Created[0].ID, out.Created[0].Ref
	if ref != id+"-fix-compiler-warnings" {
		t.Fatalf("ref %q", ref)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "projects", "jk", "tickets", id, "ticket.md"))
	if strings.Contains(string(raw), "slug:") {
		t.Fatalf("slug stored with slugs off:\n%s", raw)
	}
	if subj := run(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(subj, ref+": created") {
		t.Fatalf("commit subject %q", subj)
	}
	// Setting a slug is refused; clearing is fine.
	if _, err := a.Update(ctx, c, []UpdateIn{{ID: id, Set: map[string]any{"slug": "warnings"}}}); !IsCode(err, CodeInvalid) || !strings.Contains(err.Error(), "slugs: true") {
		t.Fatalf("set slug with slugs off: %v", err)
	}
	// Any reference, even with a wrong or stale slug, resolves by ID.
	for _, r := range []string{ref, strings.ToLower(ref), id + "-old-name", strings.ToLower(id)} {
		d, err := a.Get(GetIn{ID: r})
		if err != nil || d.ID != id || d.Ref != ref {
			t.Fatalf("get %s: %v %+v", r, err, d)
		}
	}
	w := must[*WriteOut](t)(a.Comment(ctx, c, CommentIn{ID: id + "-old-name", Type: "note", Body: "via a stale ref"}))
	if w.ID != id || w.Ref != ref {
		t.Fatalf("comment via ref: %+v", w)
	}
	// Relations store bare IDs.
	out2 := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Child", Parent: ref, BlockedBy: []string{ref}}}))
	d := must[*Detail](t)(a.Get(GetIn{ID: out2.Created[0].ID}))
	if d.ParentRef == nil || d.ParentRef.ID != id || d.ParentRef.Ref != ref || len(d.BlockedBy) != 1 || d.BlockedBy[0].ID != id {
		t.Fatalf("relations: %+v", d)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "projects", "jk", "tickets", out2.Created[0].ID, "ticket.md"))
	if !strings.Contains(string(raw), "parent: "+id+"\n") {
		t.Fatalf("parent not stored as a bare ID:\n%s", raw)
	}
}

func TestSlugsOn(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/config.yml": "slugs: true\n", ".ka/store.yml": guardedStore})
	a, dir := e.clone()
	c := agent("claude")

	out := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{
		{Title: "Fix the compiler warnings", Slug: "Compiler Warnings"},
		{Title: "Café crème: the über façade"},
	}))
	given, derived := out.Created[0], out.Created[1]
	if given.Ref != given.ID+"-compiler-warnings" || derived.Ref != derived.ID+"-cafe-creme-uber" {
		t.Fatalf("refs: %+v", out.Created)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "projects", "jk", "tickets", given.ID, "ticket.md"))
	if !strings.Contains(string(raw), "title: Fix the compiler warnings\nslug: compiler-warnings\n") {
		t.Fatalf("slug not stored in canonical position:\n%s", raw)
	}
	// Renaming keeps the slug; setting it changes the reference; old ones resolve.
	must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: given.Ref, Set: map[string]any{"title": "Silence javac warnings"}}}))
	d := must[*Detail](t)(a.Get(GetIn{ID: given.ID}))
	if d.Ref != given.Ref {
		t.Fatalf("title change moved the slug: %s", d.Ref)
	}
	ws := must[[]*WriteOut](t)(a.Update(ctx, c, []UpdateIn{{ID: given.Ref, Set: map[string]any{"slug": "javac-warnings"}}}))
	if ws[0].Ref != given.ID+"-javac-warnings" {
		t.Fatalf("after slug change: %+v", ws[0])
	}
	if d := must[*Detail](t)(a.Get(GetIn{ID: given.Ref})); d.ID != given.ID {
		t.Fatalf("old ref: %+v", d)
	}
	if _, err := a.Update(ctx, c, []UpdateIn{{ID: given.ID, Set: map[string]any{"slug": "one-two-three-four-five"}}}); !IsCode(err, CodeInvalid) {
		t.Fatalf("five-word slug: %v", err)
	}
	for _, d := range a.Validate(time.Now()) {
		if d.Severity != kaspec.SevInfo {
			t.Errorf("check: %s", d)
		}
	}
}

func TestFixSlugs(t *testing.T) {
	e := newEnv(t, nil)
	a, dir := e.clone()
	c := agent("claude")
	out := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Speed up the build"}, {Title: "Flaky cache test"}}))

	if _, err := a.FixWith(ctx, c, FixOptions{Slugs: true}); !IsCode(err, CodeInvalid) || !strings.Contains(err.Error(), "no project stores slugs") {
		t.Fatalf("fix --slugs with slugs off: %v", err)
	}
	os.WriteFile(filepath.Join(dir, ".ka", "config.yml"), []byte("slugs: true\n"), 0o644)
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-qm", "slugs on")
	a = e.open(dir)
	if _, err := a.FixWith(ctx, c, FixOptions{Slugs: true}); !IsCode(err, CodeInvalid) || !strings.Contains(err.Error(), "requires_ka") {
		t.Fatalf("fix --slugs without requires_ka: %v", err)
	}
	if diags := a.Validate(time.Now()); !hasCode(diags, "slugs_unguarded") {
		t.Fatalf("slugs: true without requires_ka: no slugs_unguarded in %v", diags)
	}
	os.WriteFile(filepath.Join(dir, ".ka", "store.yml"), []byte(guardedStore), 0o644)
	run(t, dir, "commit", "-qam", "require 1.2.0")
	a = e.open(dir)
	fo := must[*FixOut](t)(a.FixWith(ctx, c, FixOptions{Slugs: true}))
	if len(fo.Categories) != 1 || fo.Categories[0].Name != "slugs" || len(fo.Categories[0].Files) != 2 {
		t.Fatalf("fix: %+v", fo)
	}
	d := must[*Detail](t)(a.Get(GetIn{ID: out.Created[0].ID}))
	if d.Ref != out.Created[0].ID+"-speed-up-build" {
		t.Fatalf("after backfill: %s", d.Ref)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "projects", "jk", "tickets", out.Created[1].ID, "ticket.md"))
	if !strings.Contains(string(raw), "slug: flaky-cache-test\n") {
		t.Fatalf("backfilled ticket:\n%s", raw)
	}
	for _, d := range a.Validate(time.Now()) {
		if d.Severity != kaspec.SevInfo {
			t.Errorf("check: %s", d)
		}
	}
}

func hasCode(ds []kaspec.Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func withKaVersion(t *testing.T, v string) {
	old := kaspec.KaVersion
	kaspec.KaVersion = v
	t.Cleanup(func() { kaspec.KaVersion = old })
}

func TestRequiresKa(t *testing.T) {
	withKaVersion(t, "1.2.0")
	e := newEnv(t, map[string]string{".ka/store.yml": guardedStore})
	a, dir := e.clone()
	b, bdir := e.clone()
	c := agent("claude")
	out := must[*CreateOut](t)(a.Create(ctx, c, []CreateIn{{Title: "Works on 1.2.0"}}))
	id := out.Created[0].ID

	// Another client raises the requirement past this ka and pushes.
	run(t, bdir, "pull", "-q", "--rebase")
	os.WriteFile(filepath.Join(bdir, ".ka", "store.yml"), []byte("spec: 1\nname: test\nrequires_ka: 1.3.0\n"), 0o644)
	run(t, bdir, "commit", "-qam", "require 1.3.0")
	run(t, bdir, "push", "-q")
	_ = b

	// Sync refuses to bring it in; the local store is unchanged and a local
	// write stays unpushed.
	before := run(t, dir, "rev-parse", "HEAD")
	if _, err := a.WS.Stores[0].Sync(ctx, a.hooks(a.WS.Stores[0])); !IsCode(err, CodeKaOutdated) || !strings.Contains(err.Error(), "requires ka 1.3.0") {
		t.Fatalf("sync: %v", err)
	}
	if after := run(t, dir, "rev-parse", "HEAD"); after != before {
		t.Fatalf("local store moved onto a store this ka cannot read")
	}
	w := must[*WriteOut](t)(a.Comment(ctx, c, CommentIn{ID: id, Type: "note", Body: "local"}))
	if w.Pushed || !strings.Contains(w.PushError, "requires ka 1.3.0") {
		t.Fatalf("write after the requirement moved: %+v", w)
	}

	// Once the newer store.yml is checked out, writes and new sessions fail
	// with the upgrade hint.
	run(t, dir, "fetch", "-q")
	run(t, dir, "reset", "-q", "--hard", "origin/main")
	if _, err := a.Comment(ctx, c, CommentIn{ID: id, Type: "note", Body: "x"}); !IsCode(err, CodeKaOutdated) {
		t.Fatalf("write on an outdated ka: %v", err)
	}
	ws, err := Open(ctx, OpenOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(ctx, ws)
	if !IsCode(err, CodeKaOutdated) || !strings.Contains(AsError(err).Fix, "upgrade ka") {
		t.Fatalf("new service on an outdated ka: %v", err)
	}
	// A development build is never refused.
	withKaVersion(t, "1.2.1-dev")
	if _, err := New(ctx, ws); err != nil {
		t.Fatalf("dev build: %v", err)
	}
}
