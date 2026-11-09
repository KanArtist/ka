package scenario

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/testkit"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// ledger records every operation the simulator saw succeed, so the final
// state can be checked against it: nothing accepted may go missing.
type ledger struct {
	mu       sync.Mutex
	created  map[string]string   // id -> title
	labels   map[string][]string // id -> labels added
	comments map[string][]string // id -> comment filenames
	closed   map[string]string   // id -> resolution comment filename
	errors   map[string]int      // expected error codes seen
	renamed  map[string]string
	ops      int
}

func (l *ledger) add(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops++
	f()
}

// expected errors in a race: someone else got there first.
var expected = map[string]bool{
	engine.CodeClaimLost: true, engine.CodeNoCandidates: true, engine.CodeNotClaimed: true,
	engine.CodeNotYourClaim: true, "ticket_closed": true,
}

// S5: many agents on many clones update the same store at once. No
// duplicate IDs, no lost updates, no human needed, and anything that could
// not be applied is preserved visibly.
func TestS5ConcurrentChaos(t *testing.T) {
	machines, perMachine, agents, steps := 6, 2, 20, 12
	if testing.Short() {
		machines, perMachine, agents, steps = 3, 2, 8, 6
		if runtime.GOOS == "windows" {
			// Starting git processes is slow on Windows; keep the
			// contention (2 processes per clone) with fewer operations.
			machines, agents, steps = 2, 6, 5
		}
	}
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, map[string]string{
		".ka/config.yml": "comments:\n  progress_min_interval: 0s\nclaims:\n  stale_after: 24h\n",
	})
	seed := testkit.Service(t, r.Clone())
	var ins []engine.CreateIn
	for i := range 30 {
		ins = append(ins, engine.CreateIn{Title: fmt.Sprintf("seed %02d", i), Status: "ready", Priority: kaspec.Priorities[i%4],
			Acceptance: []string{"part one", "part two"}})
	}
	seedIDs := seedTickets(t, seed, ins...)

	// Each machine is a clone; each clone hosts perMachine independent
	// processes (services), which share the clone through the file lock.
	var svcs []*engine.Service
	for range machines {
		dir := r.Clone()
		for range perMachine {
			svcs = append(svcs, testkit.Service(t, dir))
		}
	}
	led := &ledger{created: map[string]string{}, labels: map[string][]string{}, comments: map[string][]string{},
		closed: map[string]string{}, errors: map[string]int{}, renamed: map[string]string{}}
	var allIDs sync.Map
	for _, id := range seedIDs {
		allIDs.Store(id, true)
	}
	randomID := func(rng *rand.Rand) string {
		var ids []string
		allIDs.Range(func(k, _ any) bool { ids = append(ids, k.(string)); return true })
		return ids[rng.IntN(len(ids))]
	}

	var fatal sync.Once
	var fatalErr error
	check := func(op string, err error) bool {
		if err == nil {
			return true
		}
		e := engine.AsError(err)
		if expected[e.Code] {
			led.add(func() { led.errors[e.Code+":"+e.Message[:min(25, len(e.Message))]]++ })
			return false
		}
		// A ticket created on another clone may not have reached this one yet.
		if (e.Code == engine.CodeNotFound || e.Code == engine.CodeDanglingRef) && strings.Contains(e.Message, "JK-") {
			led.add(func() { led.errors["not_yet_synced"]++ })
			return false
		}
		fatal.Do(func() { fatalErr = fmt.Errorf("%s: %v", op, err) })
		return false
	}

	start := time.Now()
	var wg sync.WaitGroup
	for a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc := svcs[a%len(svcs)]
			c := engine.Caller{Actor: fmt.Sprintf("agent/a%02d", a), OnBehalfOf: "tester", Session: fmt.Sprintf("S%02d", a)}
			rng := rand.New(rand.NewPCG(uint64(a), 42))
			for step := range steps {
				switch p := rng.IntN(100); {
				case p < 25: // work a ticket end to end
					n, err := svc.Next(ctx, c, engine.NextIn{})
					if !check("next", err) {
						continue
					}
					id := n.Ticket.ID
					w, err := svc.Comment(ctx, c, engine.CommentIn{ID: id, Type: "progress", Body: "working on it"})
					if check("progress", err) {
						led.add(func() { led.comments[id] = append(led.comments[id], w.Comment) })
					}
					if len(n.Ticket.AcceptanceAll) == 0 {
						// follow-ups have no acceptance items
					} else if _, _, err := svc.CheckItems(ctx, c, engine.CheckIn{ID: id, Items: []engine.CheckItem{{Text: "part one", Done: true}}}); !check("check", err) {
						continue
					}
					tr, err := svc.Transition(ctx, c, engine.TransitionIn{ID: id, Status: "done", Summary: fmt.Sprintf("done by %s", c.Actor)})
					if check("transition", err) {
						led.add(func() { led.closed[id] = tr.Comment })
					}
				case p < 45: // file follow-ups
					var batch []engine.CreateIn
					for i := range 1 + rng.IntN(3) {
						batch = append(batch, engine.CreateIn{Title: fmt.Sprintf("%s follow-up %d.%d", c.Actor, step, i), Status: "ready"})
					}
					out, err := svc.Create(ctx, c, batch)
					if check("create", err) {
						led.add(func() {
							for _, cr := range out.Created {
								led.created[cr.ID] = cr.Title
								allIDs.Store(cr.ID, true)
							}
						})
					}
				case p < 65: // label something
					id := randomID(rng)
					label := fmt.Sprintf("l-%02d-%d", a, step)
					_, err := svc.Update(ctx, c, []engine.UpdateIn{{ID: id, Add: map[string][]string{"labels": {label}}}})
					if check("update", err) {
						led.add(func() { led.labels[id] = append(led.labels[id], label) })
					}
				case p < 85: // comment on something
					id := randomID(rng)
					w, err := svc.Comment(ctx, c, engine.CommentIn{ID: id, Body: fmt.Sprintf("note from %s", c.Actor)})
					if check("comment", err) {
						led.add(func() { led.comments[id] = append(led.comments[id], w.Comment) })
					}
				default:
					out := svc.Sync(ctx, true)
					for _, e := range out.Errors {
						check("sync", e)
					}
				}
			}
		}()
	}
	wg.Wait()
	if fatalErr != nil {
		t.Fatal(fatalErr)
	}
	// Converge: every process syncs (twice, so late pushes reach everyone).
	for range 2 {
		for _, s := range svcs {
			if out := s.Sync(ctx, true); len(out.Errors) > 0 {
				t.Fatalf("final sync: %v", out.Errors[0])
			}
		}
	}
	t.Logf("%d agents, %d processes on %d clones: %d accepted operations in %v; expected races: %v",
		agents, len(svcs), machines, led.ops, time.Since(start).Round(time.Millisecond), led.errors)

	dir := r.Clone()
	final := testkit.Service(t, dir)
	// Valid, linear, and every clone converged on the remote.
	for _, d := range final.Validate(time.Now()) {
		if d.Severity != kaspec.SevInfo {
			t.Errorf("check: %s", d)
		}
	}
	if merges := testkit.Git(t, dir, "rev-list", "--merges", "HEAD"); merges != "" {
		t.Errorf("history has merge commits: %s", merges)
	}
	head := testkit.Git(t, dir, "rev-parse", "HEAD")
	for i, s := range svcs {
		if h := testkit.Git(t, s.WS.Stores[0].Root, "rev-parse", "HEAD"); h != head {
			t.Errorf("process %d did not converge: %s vs %s", i, h, head)
		}
	}
	// Nothing accepted went missing.
	get := func(id string) *engine.Detail {
		d, err := final.Get(engine.GetIn{ID: id, Include: []string{"comments"}})
		if err != nil {
			t.Errorf("%s: %v", id, err)
			return nil
		}
		return d
	}
	for id, title := range led.created {
		if d := get(id); d != nil && d.Title != title {
			t.Errorf("%s: title %q, want %q", id, d.Title, title)
		}
	}
	for id, labels := range led.labels {
		if d := get(id); d != nil {
			for _, l := range labels {
				if !contains(d.Labels, l) {
					t.Errorf("%s lost label %s (has %v)", id, l, d.Labels)
				}
			}
		}
	}
	systemComments := 0
	for id, names := range led.comments {
		d := get(id)
		if d == nil {
			continue
		}
		have := map[string]bool{}
		for _, c := range d.Comments {
			have[c.Name] = true
			if c.Type == "system" {
				systemComments++
			}
		}
		for _, n := range names {
			if !have[n] {
				t.Errorf("%s lost comment %s", id, n)
			}
		}
	}
	for id, name := range led.closed {
		if d := get(id); d != nil {
			if d.Status != "done" {
				t.Errorf("%s: status %s after an accepted close", id, d.Status)
			}
			found := false
			for _, c := range d.Comments {
				found = found || (c.Name == name && c.Type == "resolution")
			}
			if !found {
				t.Errorf("%s lost its resolution comment %s", id, name)
			}
		}
	}
	all, _ := final.Search(engine.SearchIn{All: true, Limit: 100000})
	if all.Total != len(seedIDs)+len(led.created) {
		t.Errorf("ticket count %d, want %d seeded + %d created", all.Total, len(seedIDs), len(led.created))
	}
	if systemComments > 0 {
		t.Logf("%d preserved-conflict comment(s)", systemComments)
	}
}
