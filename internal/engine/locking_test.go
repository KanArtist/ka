package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// gate makes a bare remote slow: while the gate is closed, the first push
// to arrive waits in its pre-receive hook (later pushes go through).
type gate struct {
	t                    *testing.T
	closed, entered, dir string
}

func slowRemote(t *testing.T, remote string) *gate {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks")
	}
	d := t.TempDir()
	g := &gate{t: t, closed: filepath.Join(d, "closed"), entered: filepath.Join(d, "entered"), dir: filepath.Join(d, "once")}
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"if [ -e '" + g.closed + "' ] && mkdir '" + g.dir + "' 2>/dev/null; then\n" +
		"  touch '" + g.entered + "'\n" +
		"  while [ -e '" + g.closed + "' ]; do sleep 0.05; done\n" +
		"fi\n"
	p := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.closed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() { os.Remove(g.closed) }

// waitEntered waits until a push is held at the gate.
func (g *gate) waitEntered() {
	g.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(g.entered); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.t.Fatal("no push reached the remote")
}

func within[T any](t *testing.T, d time.Duration, what string, f func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- f() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s", what, d)
	}
	panic("unreachable")
}

type writeRes struct {
	w   *WriteOut
	err error
}

// A second write to a store commits and publishes while the first write's
// push is still waiting on a slow remote; both end up on the remote, in
// order. (With one write lock held across the push, the second write waited
// for the first push.)
func TestWriteWhilePushInFlight(t *testing.T) {
	e := newEnv(t, nil)
	a, adir := e.clone()
	ids := must[*CreateOut](t)(a.Create(ctx, agent("lead"), []CreateIn{{Title: "one", Status: "ready"}, {Title: "two", Status: "ready"}}))
	one, two := ids.Created[0].ID, ids.Created[1].ID
	g := slowRemote(t, e.remote)

	first := make(chan writeRes, 1)
	go func() {
		w, err := a.Comment(ctx, agent("one"), CommentIn{ID: one, Body: "first"})
		first <- writeRes{w, err}
	}()
	g.waitEntered()
	second := within(t, 20*time.Second, "the second write", func() writeRes {
		w, err := a.Comment(ctx, agent("two"), CommentIn{ID: two, Body: "second"})
		return writeRes{w, err}
	})
	if second.err != nil || !second.w.Pushed {
		t.Fatalf("second write: %+v %v", second.w, second.err)
	}
	select {
	case r := <-first:
		t.Fatalf("the first write finished while its push was held: %+v %v", r.w, r.err)
	default:
	}
	g.open()
	r := <-first
	if r.err != nil || !r.w.Pushed {
		t.Fatalf("first write: %+v %v", r.w, r.err)
	}

	subjects := strings.Split(run(t, e.root, "--git-dir", e.remote, "log", "--format=%s", "main"), "\n")
	i1 := slices.IndexFunc(subjects, func(s string) bool { return strings.HasPrefix(s, one) && strings.Contains(s, ": note") })
	i2 := slices.IndexFunc(subjects, func(s string) bool { return strings.HasPrefix(s, two) && strings.Contains(s, ": note") })
	if i1 < 0 || i2 < 0 || i2 > i1 { // newest first: the second comment is above the first
		t.Fatalf("remote history:\n%s", strings.Join(subjects, "\n"))
	}
	if remote, local := run(t, e.root, "--git-dir", e.remote, "rev-parse", "main"), run(t, adir, "rev-parse", "HEAD"); remote != local {
		t.Fatalf("remote %s, local %s", remote, local)
	}
	// The tracking ref names exactly what the remote has, and the
	// in-flight marks are gone.
	if tr := run(t, adir, "rev-parse", "origin/main"); tr != run(t, adir, "rev-parse", "HEAD") {
		t.Fatalf("tracking ref %s", tr)
	}
	if ents, _ := os.ReadDir(filepath.Join(adir, ".git", "ka-inflight")); len(ents) != 0 {
		t.Fatalf("in-flight marks left: %v", ents)
	}
}

// Agents racing through ka_next, two in one process (a daemon) and one in
// another clone, never both get a confirmed claim on one ticket: each
// confirmed claim is the one the remote holds, every other agent moves on or
// finds nothing, and no lost claim is preserved as a conflict comment (the
// claim that lost in another writer's replay is reported to its own caller).
func TestNextRaceOneConfirmedClaim(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, _ := e.clone()
	if runtime.GOOS != "windows" {
		// A remote that takes a little while, so pushes overlap.
		os.WriteFile(filepath.Join(e.remote, "hooks", "pre-receive"),
			[]byte("#!/bin/sh\ncat >/dev/null\nsleep 0.0$(( $$ % 6 ))\n"), 0o755)
	}
	rounds := 12
	if testing.Short() {
		rounds = 4
	}
	for round := range rounds {
		label := fmt.Sprintf("round-%d", round)
		n := 1 + round%2 // one ticket, or two
		var ins []CreateIn
		for i := range n {
			ins = append(ins, CreateIn{Title: fmt.Sprintf("%s %d", label, i), Status: "ready", Labels: []string{label}})
		}
		must[*CreateOut](t)(a.Create(ctx, agent("lead"), ins))
		b.Sync(ctx, true)

		type res struct {
			who string
			out *NextOut
			err error
		}
		results := make(chan res, 3)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, p := range []struct {
			svc  *Service
			name string
		}{{a, "a1"}, {a, "a2"}, {b, "b1"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out, err := p.svc.Next(ctx, agent(p.name), NextIn{Query: "label:" + label})
				results <- res{p.name, out, err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		winners := map[string]string{} // ticket -> agent
		for r := range results {
			switch {
			case r.err != nil:
				if !IsCode(r.err, CodeNoCandidates) {
					t.Fatalf("round %d: %s: %v", round, r.who, r.err)
				}
			case !r.out.Claimed || !r.out.Confirmed:
				t.Fatalf("round %d: %s: claimed %v, confirmed %v (%s)", round, r.who, r.out.Claimed, r.out.Confirmed, r.out.PushError)
			default:
				id := r.out.Ticket.ID
				if w, dup := winners[id]; dup {
					t.Fatalf("round %d: %s confirmed for both %s and %s", round, id, w, r.who)
				}
				winners[id] = r.who
			}
		}
		if len(winners) != n {
			t.Fatalf("round %d: %d tickets, confirmed claims %v", round, n, winners)
		}
		// What the remote holds.
		c, _ := e.clone()
		for id, who := range winners {
			d := must[*Detail](t)(c.Get(GetIn{ID: id, Include: []string{"comments"}}))
			if d.Claim == nil || d.Claim.By != "agent/"+who {
				t.Fatalf("round %d: %s confirmed to %s, remote claim %+v", round, id, who, d.Claim)
			}
			for _, cm := range d.Comments {
				if cm.Type == "system" {
					t.Fatalf("round %d: lost claim preserved on %s: %s", round, id, cm.Body)
				}
			}
		}
	}
}

// When another process compacts the clone this process serves (ka compact
// from a terminal against the daemon's clone), the index reload reports
// store.compacted, once.
func TestCatchUpReportsForeignCompaction(t *testing.T) {
	e := newEnv(t, nil)
	a, adir := e.clone()
	must[*CreateOut](t)(a.Create(ctx, agent("lead"), []CreateIn{{Title: "kept", Status: "ready"}}))
	hub := NewEventHub(100)
	a.SetEvents(hub)
	seq := hub.Seq()

	other := e.open(adir) // a second process on the same clone
	out := must[*CompactOut](t)(other.Compact(ctx, agent("lead"), CompactIn{Store: "test", Confirm: "test"}))

	a.ix.catchUp()
	evs, _, _ := hub.Since(seq)
	var got []CompactedEvent
	for _, ev := range evs {
		if ev.Type == EvStoreCompacted {
			got = append(got, ev.Data.(CompactedEvent))
		}
	}
	if len(got) != 1 || got[0].Root != out.Root || got[0].From != out.From || got[0].Adopted {
		t.Fatalf("compacted events %+v (root %s from %s), all: %+v", got, out.Root, out.From, evs)
	}
	seq = hub.Seq()
	a.ix.catchUp()
	if evs, _, _ := hub.Since(seq); len(evs) != 0 {
		t.Fatalf("reported again: %+v", evs)
	}
	if res := must[*SearchOut](t)(a.Search(SearchIn{Query: "kept"})); res.Total != 1 {
		t.Fatalf("index after the compaction: %+v", res)
	}
}

// A replay by another writer that meets a commit whose writer is still
// pushing it records the outcome for that writer instead of preserving it
// as a comment, and the writer reports it: here a claim that lost.
func TestReplayOfInFlightClaimReportsToItsWriter(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	b, _ := e.clone()
	id := must[*CreateOut](t)(a.Create(ctx, agent("lead"), []CreateIn{{Title: "contested", Status: "ready"}})).Created[0].ID
	b.Sync(ctx, true)
	// b's claim wins upstream.
	must[*ClaimOut](t)(b.Claim(ctx, agent("b"), id, false))

	// a's claim is committed and marked in flight, as by a writer that has
	// released the locks and not pushed yet.
	sa := a.WS.Stores[0]
	a.ix.catchUp()
	o := stamp(&Op{Op: "claim", ID: id}, agent("a"), a.now())
	res, err := sa.commitLocal(ctx, a.hooksBy(sa, o.Actor), o, true)
	if err != nil {
		t.Fatal(err)
	}
	mine := res.Commit
	// Another writer in a's clone syncs first: its replay drops the claim.
	if _, err := sa.Sync(ctx, a.hooks(sa)); err != nil {
		t.Fatal(err)
	}
	d := must[*Detail](t)(a.Get(GetIn{ID: id, Include: []string{"comments"}}))
	if d.Claim == nil || d.Claim.By != "agent/b" || len(d.Comments) != 0 {
		t.Fatalf("after the other writer's sync: claim %+v, comments %+v", d.Claim, d.Comments)
	}
	// The claim's writer now pushes, is rejected, and learns it lost.
	err = sa.publish(ctx, a.hooks(sa), res, mine)
	if !IsCode(err, CodeClaimLost) || res.Pushed || res.Commit != "" {
		t.Fatalf("writer: %+v %v", res, err)
	}
	if _, err := os.Stat(sa.inflightPath(mine)); !os.IsNotExist(err) {
		t.Fatal("in-flight mark left behind")
	}
}
