package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/api"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
)

// writeFiles writes store-relative files under dir.
func writeFiles(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func ticketMD(id, status, priority, updated, extra string) string {
	s := "---\nid: " + id + "\ntitle: Ticket " + id + "\nstatus: " + status + "\n"
	if priority != "" {
		s += "priority: " + priority + "\n"
	}
	s += "created: 2026-01-01T00:00:00Z\nupdated: " + updated + "\n" + extra + "---\n\nBody.\n"
	return s
}

// completionWorkspace builds two stores: org (config with extra statuses,
// projects or and mv) and jk (extends ../org; projects ap, jk, and a mv
// tombstone with a leftover ticket), with open, closed, slugged, invalid,
// duplicated and lower-case-named tickets. It returns jk's directory.
func completionWorkspace(t testing.TB) string {
	root := t.TempDir()
	ctx := context.Background()
	org := filepath.Join(root, "org")
	newRepo(t, org)
	if _, err := engine.InitStore(ctx, engine.InitOptions{Dir: org, Name: "org", Project: "or", Prefix: "OR"}); err != nil {
		t.Fatal(err)
	}
	statuses := "workflow:\n  statuses:\n" +
		"    - {name: backlog, category: backlog}\n    - {name: ready, category: unstarted}\n" +
		"    - {name: in_progress, category: started}\n    - {name: shipped, category: completed}\n" +
		"    - {name: done, category: completed}\n    - {name: canceled, category: canceled}\n  initial: backlog\n"
	writeFiles(t, org, map[string]string{
		".ka/config.yml":                         statuses,
		"projects/mv/project.yml":                "key: mv\nprefix: MV\n",
		"projects/or/tickets/OR-0PEN1/ticket.md": ticketMD("OR-0PEN1", "ready", "P1", "2026-02-01T00:00:00Z", ""),
		"projects/or/tickets/OR-SH1P1/ticket.md": ticketMD("OR-SH1P1", "shipped", "", "2026-02-01T00:00:00Z", ""),
		"projects/mv/tickets/MV-0PEN1/ticket.md": ticketMD("MV-0PEN1", "in_progress", "", "2026-02-02T00:00:00Z", ""),
		"projects/mv/tickets/MV-D0NE1/ticket.md": ticketMD("MV-D0NE1", "done", "", "2026-02-02T00:00:00Z", ""),
	})
	testkit.Git(t, org, "add", "-A")
	testkit.Git(t, org, "commit", "-qm", "org")

	jk := filepath.Join(root, "jk")
	newRepo(t, jk)
	if _, err := engine.InitStore(ctx, engine.InitOptions{Dir: jk, Name: "jk", Project: "jk", Prefix: "JK", Extends: "../org"}); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		// The project adds a closed status on top of org's workflow.
		"projects/ap/project.yml": "key: ap\nprefix: AP\nconfig:\n  workflow:\n    statuses:\n" +
			"      - {name: ready, category: unstarted}\n      - {name: wontfix, category: canceled}\n      - {name: done, category: completed}\n",
		"projects/ap/tickets/AP-0PEN1/ticket.md": ticketMD("AP-0PEN1", "ready", "P0", "2026-03-01T00:00:00Z", ""),
		"projects/ap/tickets/AP-W0NT1/ticket.md": ticketMD("AP-W0NT1", "wontfix", "", "2026-03-01T00:00:00Z", ""),
		// shipped is only a status through org: in ap it is unknown, so open.
		"projects/ap/tickets/AP-SH1P1/ticket.md": ticketMD("AP-SH1P1", "shipped", "", "2026-03-01T00:00:00Z", ""),
		// The same ID in ap (open) and jk (closed): jk, loaded later, wins.
		"projects/ap/tickets/JK-DUP01/ticket.md":                         ticketMD("JK-DUP01", "ready", "", "2026-03-01T00:00:00Z", ""),
		"projects/jk/tickets/JK-DUP01/ticket.md":                         ticketMD("JK-DUP01", "done", "", "2026-03-01T00:00:00Z", ""),
		"projects/jk/tickets/JK-0PEN1/ticket.md":                         ticketMD("JK-0PEN1", "ready", "P2", "2026-03-05T00:00:00Z", ""),
		"projects/jk/tickets/JK-0PEN2/ticket.md":                         ticketMD("JK-0PEN2", "in_progress", "", "2026-03-04T00:00:00Z", ""),
		"projects/jk/tickets/JK-SLUG1/ticket.md":                         ticketMD("JK-SLUG1", "backlog", "P3", "2026-03-03T00:00:00Z", "slug: a-slug\n"),
		"projects/jk/tickets/JK-SH1P1/ticket.md":                         ticketMD("JK-SH1P1", "shipped", "", "2026-03-03T00:00:00Z", ""),
		"projects/jk/tickets/JK-CANC1/ticket.md":                         ticketMD("JK-CANC1", "canceled", "", "2026-03-03T00:00:00Z", ""),
		"projects/jk/tickets/JK-0DDS1/ticket.md":                         ticketMD("JK-0DDS1", "nonsense", "", "2026-03-03T00:00:00Z", ""),
		"projects/jk/tickets/JK-BAD01/ticket.md":                         "not a ticket at all\n",
		"projects/jk/tickets/JK-N0MD1/notes.txt":                         "no ticket.md here\n",
		"projects/jk/tickets/jk-l0wer/ticket.md":                         ticketMD("JK-L0WER", "ready", "", "2026-03-03T00:00:00Z", ""),
		"projects/jk/tickets/JK-C0MM1/ticket.md":                         ticketMD("JK-C0MM1", "ready", "", "2026-03-03T00:00:00Z", ""),
		"projects/jk/tickets/JK-C0MM1/comments/20260303T000000Z-AAAA.md": "---\ntype: note\nauthor: agent/x\ncreated: 2026-03-03T00:00:00Z\n---\n\nhi\n",
		// mv moved to org; its leftover ticket here is ignored.
		"projects/mv/project.yml":                "key: mv\nprefix: MV\nmoved_to_store: org\n",
		"projects/mv/tickets/MV-STAL1/ticket.md": ticketMD("MV-STAL1", "ready", "", "2026-03-03T00:00:00Z", ""),
	}
	writeFiles(t, jk, files)
	testkit.Git(t, jk, "add", "-A")
	testkit.Git(t, jk, "commit", "-qm", "jk")
	return jk
}

var completionPrefixes = []string{"", "j", "jk", "JK-", "jk-0", "jk-d", "JK-SH", "jk-l", "a", "ap-", "o", "or-s", "m", "mv-", "x", "JK-BAD", "jk-n0"}

func openWS(t testing.TB, dir string) *engine.Workspace {
	t.Helper()
	ws, err := engine.Open(context.Background(), engine.OpenOptions{Dir: dir, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// The file scan offers exactly what the index offers.
func TestCompletionScanMatchesIndex(t *testing.T) {
	testkit.GitEnv(t)
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	dir := completionWorkspace(t)
	ws := openWS(t, dir)
	if len(ws.Stores) != 2 {
		t.Fatalf("stores: %d", len(ws.Stores))
	}
	ctx := context.Background()
	if got := sorted(indexTicketIDs(ctx, ws, "")); !slices.Equal(got, []string{"AP-0PEN1", "AP-SH1P1", "JK-0DDS1", "JK-0PEN1", "JK-0PEN2", "JK-BAD01", "JK-C0MM1", "JK-SLUG1", "MV-0PEN1", "OR-0PEN1", "jk-l0wer"}) {
		t.Fatalf("the fixture no longer covers what it should: %v", got)
	}
	for _, p := range completionPrefixes {
		want := sorted(indexTicketIDs(ctx, ws, p))
		got, ok := scanTicketIDs(ws, p)
		if !ok || !slices.Equal(sorted(got), want) {
			t.Errorf("prefix %q: scan %v (ok %v), index %v", p, sorted(got), ok, want)
		}
	}
	for _, p := range []string{"", "s", "in", "w", "d", "c", "zz"} {
		want := sorted(indexStatuses(ctx, ws, p))
		got, ok := scanStatuses(ws, p)
		if !ok || !slices.Equal(sorted(got), want) {
			t.Errorf("statuses %q: scan %v, index %v", p, sorted(got), want)
		}
	}
	// End to end through ka __complete (no daemon: the scan path).
	if r := ka(t, dir, "__complete", "show", "jk-0"); r.code != 0 || r.out != "JK-0DDS1\nJK-0PEN1\nJK-0PEN2\n" {
		t.Fatalf("__complete show: %+v", r)
	}
	if r := ka(t, dir, "__complete", "move", "JK-0PEN1", "s"); r.code != 0 || r.out != "shipped\n" {
		t.Fatalf("__complete move: %+v", r)
	}
}

// newRepo makes dir its own git repository, so that committing there can
// never reach a repository that encloses it (TMPDIR inside a checkout).
func newRepo(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Belt and braces: git never looks above the scratch parent.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	testkit.Git(t, dir, "init", "-q", "-b", "main") // fails the test on error
	if top := strings.TrimSpace(testkit.Git(t, dir, "rev-parse", "--show-toplevel")); !sameDir(top, dir) {
		t.Fatalf("%s is not its own repository (toplevel %s)", dir, top)
	}
}

// genStore writes n tickets (with comments) to a fresh store at dir.
func genStore(t testing.TB, dir string, n, comments int) {
	newRepo(t, dir)
	if _, err := engine.InitStore(context.Background(), engine.InitOptions{Dir: dir, Name: "big", Project: "jk", Prefix: "JK"}); err != nil {
		t.Fatal(err)
	}
	const b32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	statuses := []string{"backlog", "ready", "in_progress", "done", "done", "canceled"}
	files := map[string]string{}
	for i := range n {
		id := "JK-" + string([]byte{b32[i/32768%32], b32[i/1024%32], b32[i/32%32], b32[i%32], 'Q'})
		pri := []string{"", "P0", "P1", "P2", "P3"}[i%5]
		upd := fmt.Sprintf("2026-%02d-%02dT00:00:00Z", 1+i%12, 1+i%28)
		d := "projects/jk/tickets/" + id + "/"
		files[d+"ticket.md"] = ticketMD(id, statuses[i%len(statuses)], pri, upd, "") + strings.Repeat("Some body text.\n", 20)
		for c := range comments {
			files[fmt.Sprintf("%scomments/20260303T0000%02dZ-AAAA.md", d, c)] = "---\ntype: note\nauthor: agent/x\ncreated: 2026-03-03T00:00:00Z\n---\n\nA comment.\n"
		}
	}
	writeFiles(t, dir, files)
	testkit.Git(t, dir, "add", "-A")
	testkit.Git(t, dir, "commit", "-qm", "big")
}

// With more matches than the cap, the scan keeps the same first 300 in
// the index's order (priority, then most recently updated).
func TestCompletionScanCap(t *testing.T) {
	testkit.GitEnv(t)
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	dir := filepath.Join(t.TempDir(), "big")
	genStore(t, dir, 1500, 0)
	ws := openWS(t, dir)
	ctx := context.Background()
	for _, p := range []string{"", "jk", "JK-0", "JK-1", "JK-1A"} {
		want := sorted(indexTicketIDs(ctx, ws, p))
		got, _ := scanTicketIDs(ws, p)
		if !slices.Equal(sorted(got), want) {
			t.Errorf("prefix %q: scan %d %v…, index %d %v…", p, len(got), got[:min(5, len(got))], len(want), want[:min(5, len(want))])
		}
		if p == "" && len(want) != completeCap {
			t.Errorf("empty prefix gave %d, want the cap", len(want))
		}
	}
}

// startCompletionDaemon serves svc's workspace like `ka daemon` and writes its
// runtime file.
func startCompletionDaemon(t *testing.T, svc *engine.Service) *api.Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := api.New(svc, api.Options{Token: "tok", Version: "test"})
	ln, err := api.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ctx, ln)
	rt := &api.Runtime{PID: os.Getpid(), Addr: ln.Addr().String(), URL: "http://" + ln.Addr().String(), Workspace: svc.WS.Source,
		WorkspaceHash: api.WorkspaceHash(svc.WS.Source), Token: "tok"}
	if err := api.WriteRuntime(rt); err != nil {
		t.Fatal(err)
	}
	return rt
}

// With a daemon running, completion asks it, and gets what the index gives.
func TestCompletionDaemonMatchesIndex(t *testing.T) {
	testkit.GitEnv(t)
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	dir := completionWorkspace(t)
	ws := openWS(t, dir)
	ctx := context.Background()
	if _, ok := daemonTicketIDs(ctx, ws.Source, ""); ok {
		t.Fatal("answered without a daemon")
	}
	startCompletionDaemon(t, testkit.Service(t, dir))
	for _, p := range append(completionPrefixes, "jk*", "a b") {
		want := sorted(indexTicketIDs(ctx, ws, p))
		got, ok := daemonTicketIDs(ctx, ws.Source, p)
		if strings.ContainsAny(p, "* ") {
			if ok {
				t.Errorf("prefix %q went to the daemon", p)
			}
			continue
		}
		if !ok || !slices.Equal(sorted(got), want) {
			t.Errorf("prefix %q: daemon %v (ok %v), index %v", p, sorted(got), ok, want)
		}
	}
	for _, p := range []string{"", "s", "in", "w"} {
		want := sorted(indexStatuses(ctx, ws, p))
		got, ok := daemonStatuses(ctx, ws.Source, p)
		if !ok || !slices.Equal(sorted(got), want) {
			t.Errorf("statuses %q: daemon %v, index %v", p, sorted(got), want)
		}
	}
	if r := ka(t, dir, "__complete", "show", "jk-0"); r.code != 0 || r.out != "JK-0DDS1\nJK-0PEN1\nJK-0PEN2\n" {
		t.Fatalf("__complete show: %+v", r)
	}
}

// A daemon that has gone away (stale runtime file) is skipped quickly.
func TestCompletionStaleDaemon(t *testing.T) {
	testkit.GitEnv(t)
	t.Setenv("KA_RUNTIME_DIR", t.TempDir())
	dir := completionWorkspace(t)
	ws := openWS(t, dir)
	ln, err := api.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	api.WriteRuntime(&api.Runtime{PID: os.Getpid(), Addr: addr, URL: "http://" + addr, Workspace: ws.Source, WorkspaceHash: api.WorkspaceHash(ws.Source), Token: "tok"})
	start := time.Now()
	if _, ok := daemonTicketIDs(context.Background(), ws.Source, "jk"); ok {
		t.Fatal("a dead daemon answered")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	if r := ka(t, dir, "__complete", "show", "jk-0"); r.code != 0 || r.out != "JK-0DDS1\nJK-0PEN1\nJK-0PEN2\n" {
		t.Fatalf("__complete show: %+v", r)
	}
}

// BenchmarkCompletion compares the index (old) with the file scan and the
// daemon on a store of 3000 tickets with 3 comments each:
//
//	TMPDIR=/var/tmp go test ./internal/cli -run '^$' -bench Completion -benchtime 5x
func BenchmarkCompletion(b *testing.B) {
	testkit.GitEnv(b)
	b.Setenv("KA_RUNTIME_DIR", b.TempDir())
	dir := filepath.Join(b.TempDir(), "big")
	genStore(b, dir, 3000, 3)
	ctx := context.Background()
	x := &IO{Dir: dir}
	for _, p := range []string{"", "JK-012"} {
		b.Run("index/"+p, func(b *testing.B) {
			for range b.N {
				if ws := openWorkspaceQuiet(ctx, x); ws == nil || len(indexTicketIDs(ctx, ws, p)) == 0 {
					b.Fatal("no ids")
				}
			}
		})
		b.Run("scan/"+p, func(b *testing.B) {
			for range b.N {
				if len(ticketIDs(ctx, x, p)) == 0 {
					b.Fatal("no ids")
				}
			}
		})
	}
	b.Run("open-only", func(b *testing.B) {
		for range b.N {
			openWorkspaceQuiet(ctx, x)
		}
	})
	svc := testkit.Service(b, dir)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := api.New(svc, api.Options{Token: "tok", Version: "test"})
	ln, err := api.Listen("127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	go srv.Serve(ctx2, ln)
	api.WriteRuntime(&api.Runtime{PID: os.Getpid(), Addr: ln.Addr().String(), URL: "http://" + ln.Addr().String(), Workspace: svc.WS.Source,
		WorkspaceHash: api.WorkspaceHash(svc.WS.Source), Token: "tok"})
	for _, p := range []string{"", "JK-012"} {
		b.Run("daemon/"+p, func(b *testing.B) {
			for range b.N {
				ws := openWorkspaceQuiet(ctx, x)
				if out, ok := daemonTicketIDs(ctx, ws.Source, p); !ok || len(out) == 0 {
					b.Fatal("daemon did not answer")
				}
			}
		})
	}
}

// sameDir reports whether two paths name the same directory.
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && filepath.Clean(ra) == filepath.Clean(rb)
}
