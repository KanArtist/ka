package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const tmplBody = `# JK-1001 — First ticket

## Problem

_What's wrong, or what do we want? Keep it concrete._

## Goals

-

## Acceptance

- [ ]

## Non-goals

-

## Refs

<!-- Symbolic links resolve via ` + "`ka resolve`" + ` -->
-

# JK-1001 — First ticket

## Observed

Real content, see JK-1002.
`

// fixtureV0 builds a small v0 repo with the shapes the real corpus has.
func fixtureV0(t *testing.T) string {
	root := t.TempDir()
	write(t, root+"/projects/jk/project.yml", "# JumpKick — the build tool.\nkey: jk\nname: JumpKick\nforge: https://github.com/example/jk\nlocal_dir: jk\nid_prefix: JK\nid_start: 1001\ndefault_labels: []\n")
	write(t, root+"/projects/gy/project.yml", "key: gy\nname: Gy\nforge: https://github.com/example/gy\nlocal_dir: gy\nid_prefix: GY\n")
	write(t, root+"/projects/gy-java/project.yml", "key: gy-java\nname: Gy Java\nforge: https://github.com/example/gy-java\nlocal_dir: gy-java\nid_prefix: GYJ\n")
	fm := func(id, extra string) string {
		return "---\nid: " + id + "\nproject: jk\ntitle: Ticket " + id + "\nstatus: done\npriority: P1\nkind: go-do\nassignee: Bryan Sant\nclaimed_at: 2026-09-01\ndepends_on: []\nlabels: [cache]\nlinks: []\ncreated: 2026-09-01\nupdated: 2026-09-02\n" + extra + "---\n\n"
	}
	write(t, root+"/projects/jk/tickets/JK-1001-first.md", strings.ReplaceAll(fm("JK-1001", "branch: \"\"\nsource: JK-1002 epic\n")+tmplBody, "\n", "\r\n"))
	write(t, root+"/projects/jk/tickets/JK-1002-second.md", strings.Replace(fm("JK-1002", "status_note: \"shipped in 0.5 — see JK-1001\"\n"), "labels: [cache]", `labels: [cache, "parent:JK-1001", "--depends-on JK-1001"]`, 1)+
		"# JK-1002 — Ticket JK-1002\n\n## Problem\n\nIt broke.\n\n## Acceptance\n\n- [x] fixed\n- [ ] documented,\n  with a continuation line\n\n## Outcome (2026-09-02)\n\nFixed in abc123.\n\n```\n## not a heading\n```\n\n## Closed by audit (2026-09-03)\n\nVerified.\n")
	// duplicate legacy id, in flight, with a depends_on and a bad escape
	write(t, root+"/projects/jk/tickets/JK-1003-a.md", strings.Replace(fm("JK-1003", ""), "status: done", "status: in_progress", 1)+"## Problem\n\nA.\n")
	write(t, root+"/projects/jk/tickets/JK-1003-b.md", strings.Replace(strings.Replace(fm("JK-1003", ""), "depends_on: []", "depends_on: [JK-1002, JK-9999]", 1), "title: Ticket JK-1003", `title: "reads \usr\bin on Windows"`, 1)+"## Problem\n\nB.\n")
	// archive routed to another project
	write(t, root+"/projects/gy/archive/2026/GY-1-old.md", strings.Replace(fm("GY-1", ""), "project: jk", "project: gy", 1)+"## Problem\n\nOld java ticket.\n")
	write(t, root+"/projects/gy/tickets/GY-101-new.md", strings.Replace(fm("GY-101", ""), "kind: go-do", "kind: build|test", 1)+"## Problem\n\nBlocked on JK-1002.\n")
	return root
}

func migrateOnce(t *testing.T, v0, out string, force bool) *report {
	t.Helper()
	archiveProject["gy"] = "gy-java"
	archivedProjects["gy-java"] = true
	splitStore["gy-java"] = "gy"
	t.Cleanup(func() {
		delete(archiveProject, "gy")
		delete(archivedProjects, "gy-java")
		delete(splitStore, "gy-java")
	})
	rep, err := run(options{V0: v0, Out: out, Layout: "split", OrgURL: "../org", Force: force, Now: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func findTicket(t *testing.T, out, legacy, base string) (string, *kaspec.Ticket) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(out, "*", "projects", "*", "tickets", "*", "ticket.md"))
	for _, m := range matches {
		data, _ := os.ReadFile(m)
		tk, _ := kaspec.ParseTicket(m, data)
		if tk != nil && tk.Fields["legacy_id"].Scalar == legacy && (base == "" || strings.Contains(tk.Body, base)) {
			return m, tk
		}
	}
	t.Fatalf("no migrated ticket for %s %s", legacy, base)
	return "", nil
}

func TestMigrateFixture(t *testing.T) {
	v0, out := fixtureV0(t), t.TempDir()
	rep := migrateOnce(t, v0, out, false)
	if rep.checkErrors != 0 || rep.checkWarns != 0 {
		t.Fatalf("migrated stores do not check clean:\n%s", rep.markdown())
	}
	if len(rep.corpus.Tickets) != 6 || len(rep.corpus.Problems) != 0 {
		t.Fatalf("tickets=%d problems=%v", len(rep.corpus.Tickets), rep.corpus.Problems)
	}

	// template residue removed, appended content kept, references rewritten
	p1, t1 := findTicket(t, out, "JK-1001", "")
	_, t2 := findTicket(t, out, "JK-1002", "")
	if strings.Contains(t1.Body, "What's wrong") || strings.Contains(t1.Body, "- [ ]") || strings.Contains(t1.Body, "# JK-1001") {
		t.Errorf("template residue left in body:\n%s", t1.Body)
	}
	if !strings.Contains(t1.Body, "Real content, see "+t2.ID+".") {
		t.Errorf("reference not rewritten:\n%s", t1.Body)
	}
	if t1.Assignee != "bsant" || t1.Kind != "feature" || t1.Fields["source"].Scalar != t2.ID+" epic" {
		t.Errorf("normalization: %+v", t1)
	}
	if t2.Parent != t1.ID || len(t2.BlockedBy) != 1 || t2.BlockedBy[0] != t1.ID || strings.Join(t2.Labels, ",") != "cache" {
		t.Errorf("relation labels not converted: parent=%q blocked_by=%v labels=%v", t2.Parent, t2.BlockedBy, t2.Labels)
	}
	if _, ok := t1.Fields["branch"]; ok {
		t.Error("empty extension fields must be dropped")
	}
	if raw, _ := os.ReadFile(p1); strings.Contains(string(raw), "\r") {
		t.Error("CRLF survived")
	}

	// acceptance, history sections and status_note become structure
	if len(t2.Acceptance) != 2 || !t2.Acceptance[0].Done || t2.Acceptance[1].Text != "documented, with a continuation line" {
		t.Errorf("acceptance = %+v", t2.Acceptance)
	}
	if strings.Contains(t2.Body, "Outcome") || strings.Contains(t2.Body, "Closed by audit") || strings.Contains(t2.Body, "Acceptance") {
		t.Errorf("history left in body:\n%s", t2.Body)
	}
	comments, _ := filepath.Glob(filepath.Join(out, "jk", "projects", "jk", "tickets", t2.ID, "comments", "*.md"))
	if len(comments) != 3 {
		t.Fatalf("want 3 comments (2 resolutions + status note), got %v", comments)
	}
	first, _ := os.ReadFile(comments[0])
	if !strings.Contains(string(first), "type: resolution") || !strings.Contains(string(first), "## not a heading") || !strings.Contains(string(first), "created: 2026-09-02T00:00:00Z") {
		t.Errorf("first comment:\n%s", first)
	}

	// duplicate legacy ids get distinct tickets; the in-flight one keeps its claim
	_, a := findTicket(t, out, "JK-1003", "A.")
	_, b := findTicket(t, out, "JK-1003", "B.")
	if a.ID == b.ID || a.Claim == nil || a.Claim.At != "2026-09-01T00:00:00Z" {
		t.Errorf("dup handling: a=%+v b=%+v", a, b)
	}
	if b.Title != `reads \usr\bin on Windows` || len(b.BlockedBy) != 1 || b.BlockedBy[0] != t2.ID {
		t.Errorf("b = %+v", b)
	}

	// archive routing and project layout
	pOld, old := findTicket(t, out, "GY-1", "")
	if !strings.HasPrefix(old.ID, "GYJ-") || !strings.Contains(pOld, filepath.Join("gy", "projects", "gy-java")) {
		t.Errorf("archive ticket landed at %s as %s", pOld, old.ID)
	}
	if py, _ := os.ReadFile(filepath.Join(out, "gy", "projects", "gy-java", "project.yml")); !strings.Contains(string(py), "archived: true") {
		t.Errorf("gy-java not archived:\n%s", py)
	}
	if sy, _ := os.ReadFile(filepath.Join(out, "jk", ".ka", "store.yml")); !strings.Contains(string(sy), "url: ../org") {
		t.Errorf("store.yml:\n%s", sy)
	}
}

func snapshot(t *testing.T, out string) map[string]string {
	m := map[string]string{}
	filepath.Walk(out, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.HasPrefix(filepath.Base(p), ".migrate") && filepath.Base(p) != "migrate-v0-report.md" {
			b, _ := os.ReadFile(p)
			m[p] = string(b)
		}
		return nil
	})
	return m
}

func TestMigrateIsRepeatable(t *testing.T) {
	v0, out := fixtureV0(t), t.TempDir()
	migrateOnce(t, v0, out, false)
	before := snapshot(t, out)
	_, t1 := findTicket(t, out, "JK-1001", "")

	// 1. re-running changes nothing
	rep := migrateOnce(t, v0, out, false)
	if rep.created+rep.updated+rep.deleted != 0 || rep.newIDs != 0 {
		t.Fatalf("re-run changed files: %+v", rep)
	}
	after := snapshot(t, out)
	if len(before) != len(after) {
		t.Fatal("file set changed")
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("%s changed on re-run", k)
		}
	}

	// 2. a ticket changes in v0 and a new one appears: update + create, ids stable
	f := filepath.Join(v0, "projects/jk/tickets/JK-1003-a.md")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, []byte(strings.Replace(string(b), "status: in_progress", "status: review", 1)), 0o644)
	write(t, v0+"/projects/jk/tickets/JK-1004-new.md", "---\nid: JK-1004\nproject: jk\ntitle: New\nstatus: ready\ncreated: 2026-11-08\nupdated: 2026-11-08\ndepends_on: [JK-1001]\n---\n\nFollows JK-1003.\n")
	rep = migrateOnce(t, v0, out, false)
	if rep.updated != 1 || rep.created != 1 || rep.newIDs != 1 || len(rep.conflicts) != 0 {
		t.Fatalf("incremental run: %+v %v", rep, rep.conflicts)
	}
	if _, again := findTicket(t, out, "JK-1001", ""); again.ID != t1.ID {
		t.Fatal("ids must be stable across runs")
	}
	pNew, nt := findTicket(t, out, "JK-1004", "")
	if nt.BlockedBy[0] != t1.ID || !strings.Contains(nt.Body, "JK-1003") {
		t.Errorf("new ticket: %+v (an ambiguous legacy id stays as written)", nt)
	}

	// 3. a v1 file edited by hand is never overwritten without --force
	edited := strings.Replace(string(mustRead(t, pNew)), "status: ready", "status: blocked", 1)
	os.WriteFile(pNew, []byte(edited), 0o644)
	nf := v0 + "/projects/jk/tickets/JK-1004-new.md"
	nb, _ := os.ReadFile(nf)
	os.WriteFile(nf, []byte(strings.Replace(string(nb), "title: New", "title: Renamed", 1)), 0o644)
	rep = migrateOnce(t, v0, out, false)
	if len(rep.conflicts) != 1 || string(mustRead(t, pNew)) != edited {
		t.Fatalf("conflict not protected: %v", rep.conflicts)
	}
	rep = migrateOnce(t, v0, out, false)
	if len(rep.conflicts) != 1 {
		t.Fatal("conflict must persist until resolved")
	}
	rep = migrateOnce(t, v0, out, true)
	if len(rep.conflicts) != 0 || !strings.Contains(string(mustRead(t, pNew)), "title: Renamed") {
		t.Fatal("--force must overwrite")
	}

	// 4. a ticket deleted from v0 is removed, comments and directory included
	_, t2 := findTicket(t, out, "JK-1002", "")
	os.Remove(filepath.Join(v0, "projects/jk/tickets/JK-1002-second.md"))
	rep = migrateOnce(t, v0, out, false)
	if _, err := os.Stat(filepath.Join(out, "jk", "projects", "jk", "tickets", t2.ID)); !os.IsNotExist(err) {
		t.Fatalf("deleted ticket directory still present (rep %+v)", rep)
	}
	if rep.checkErrors != 0 {
		t.Fatalf("store invalid after deletion:\n%s", rep.markdown())
	}

	// 5. files the migrator never wrote are never touched
	native := filepath.Join(out, "jk", "projects", "jk", "tickets", "JK-ZZZZ9", "ticket.md")
	write(t, native, "---\nid: JK-ZZZZ9\ntitle: Native v1 ticket\nstatus: ready\ncreated: 2026-11-08T00:00:00Z\nupdated: 2026-11-08T00:00:00Z\n---\n")
	migrateOnce(t, v0, out, true)
	if _, err := os.Stat(native); err != nil {
		t.Fatal("a native v1 ticket was removed")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMigrateRealCorpus runs against a real v0 checkout when KA_V0_CORPUS
// points at one (it is not part of this repository).
func TestMigrateRealCorpus(t *testing.T) {
	v0 := os.Getenv("KA_V0_CORPUS")
	if v0 == "" {
		t.Skip("set KA_V0_CORPUS to a KanArtist v0 checkout to run")
	}
	out := t.TempDir()
	rep, err := run(options{V0: v0, Out: out, Layout: "split", OrgURL: "../org", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.checkErrors != 0 || rep.checkWarns != 0 || len(rep.corpus.Problems) != 0 {
		t.Fatalf("real corpus: %d errors, %d warnings, problems %v", rep.checkErrors, rep.checkWarns, rep.corpus.Problems)
	}
	rep2, err := run(options{V0: v0, Out: out, Layout: "split", OrgURL: "../org", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.created+rep2.updated+rep2.deleted != 0 {
		t.Fatalf("re-run on the real corpus changed files: %+v", rep2)
	}
	t.Logf("migrated %d tickets, %d files", len(rep.corpus.Tickets), rep.created)
}
