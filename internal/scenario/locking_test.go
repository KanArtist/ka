package scenario

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/engine"
)

// holdPushes makes a bare remote hold the first push that arrives in its
// pre-receive hook until the returned release is called. entered reports
// when that push is waiting.
func holdPushes(t *testing.T, remote string) (entered func() bool, release func()) {
	t.Helper()
	d := t.TempDir()
	closed, in, once := filepath.Join(d, "closed"), filepath.Join(d, "entered"), filepath.Join(d, "once")
	shHook(t, filepath.Join(remote, "hooks", "pre-receive"), "cat >/dev/null\n"+
		"if [ -e '"+closed+"' ] && mkdir '"+once+"' 2>/dev/null; then\n"+
		"  touch '"+in+"'\n"+
		"  while [ -e '"+closed+"' ]; do sleep 0.05; done\n"+
		"fi")
	if err := os.WriteFile(closed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	release = func() { os.Remove(closed) }
	t.Cleanup(release)
	return func() bool { _, err := os.Stat(in); return err == nil }, release
}

// P1: a write to one store is not held up by a push to another store that
// is waiting on a slow remote, nor by a sync of it. (The service once had
// one write lock for every store, held across the network.)
func TestWriteNotBlockedByOtherStoresPush(t *testing.T) {
	svc, remotes := layered(t)
	me := svc.CallerFor("bsant", "")
	jkt := seedTickets(t, svc, engine.CreateIn{Project: "jk", Title: "slow remote", Status: "ready"})[0]
	gyt := seedTickets(t, svc, engine.CreateIn{Project: "gyggly", Title: "fast remote", Status: "ready"})[0]
	entered, release := holdPushes(t, remotes["jk"].Path)

	type res struct {
		w   *engine.WriteOut
		err error
	}
	slow := make(chan res, 1)
	go func() {
		w, err := svc.Comment(ctx, me, engine.CommentIn{ID: jkt, Body: "waits for the jk remote"})
		slow <- res{w, err}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for !entered() {
		if time.Now().After(deadline) {
			t.Fatal("the jk push never reached its remote")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A sync of every store runs meanwhile, and waits for jk's remote too.
	synced := make(chan *engine.SyncOut, 1)
	go func() { synced <- svc.Sync(ctx, true) }()

	fast := make(chan res, 1)
	go func() {
		w, err := svc.Comment(ctx, me, engine.CommentIn{ID: gyt, Body: "another store"})
		fast <- res{w, err}
	}()
	select {
	case r := <-fast:
		if r.err != nil || !r.w.Pushed {
			t.Fatalf("gyggly write: %+v %v", r.w, r.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a write to gyggly waited for jk's push")
	}
	// So does a batch, and a claim.
	outs, err := svc.Update(ctx, me, []engine.UpdateIn{{ID: gyt, Set: map[string]any{"priority": "P1"}}})
	if err != nil || !outs[0].Pushed {
		t.Fatalf("gyggly update: %+v %v", outs, err)
	}
	if co, err := svc.Claim(ctx, svc.CallerFor("agent/claude", "S1"), gyt, false); err != nil || !co.Confirmed {
		t.Fatalf("gyggly claim: %+v %v", co, err)
	}
	select {
	case r := <-slow:
		t.Fatalf("the jk write finished while its push was held: %+v %v", r.w, r.err)
	default:
	}

	release()
	if r := <-slow; r.err != nil || !r.w.Pushed {
		t.Fatalf("jk write: %+v %v", r.w, r.err)
	}
	if so := <-synced; len(so.Errors) > 0 {
		t.Fatalf("sync: %+v", so.Errors)
	}
	for _, st := range svc.Sync(ctx, false).Stores {
		if st.State != "synced" {
			t.Fatalf("store %s: %+v", st.Store, st)
		}
	}
}
