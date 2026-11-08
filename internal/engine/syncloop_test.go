package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// countPushes installs a post-receive hook on the test remote and returns
// a function reporting how many pushes it has received.
func (e *testEnv) countPushes() func() int {
	e.t.Helper()
	if runtime.GOOS == "windows" {
		e.t.Skip("post-receive hook counting needs sh")
	}
	log := filepath.Join(e.root, "pushes.log")
	hook := filepath.Join(e.remote, "hooks", "post-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho push >> '"+log+"'\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "push")
	}
}

func TestParsePushMode(t *testing.T) {
	for in, want := range map[string]string{"": "immediate", "immediate": "immediate", "manual": "manual", "debounce": "debounce:2s", "debounce:500ms": "debounce:500ms"} {
		mode, d, err := ParsePushMode(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		st := &Store{PushMode: mode, PushDelay: d}
		if st.PushSetting() != want {
			t.Errorf("%q: %s, want %s", in, st.PushSetting(), want)
		}
	}
	for _, bad := range []string{"sometimes", "debounce:x", "debounce:-1s", "debounce:5m"} {
		if _, _, err := ParsePushMode(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// A long-running process with push: debounce batches a burst of writes into
// one push; claims still push synchronously; Flush sends what is pending.
func TestDebouncedPush(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/config.yml": "runtime:\n  sync:\n    push: debounce:300ms\n    fetch_interval: 3s\n"})
	pushes := e.countPushes()
	s, _ := e.clone()
	st := s.WS.Stores[0]
	if st.PushSetting() != "debounce:300ms" || st.FetchInterval != 3*time.Second {
		t.Fatalf("store recommendation not applied: %s %v", st.PushSetting(), st.FetchInterval)
	}
	s.Background = true
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		out := must[*CreateOut](t)(s.Create(ctx, agent("a"), []CreateIn{{Title: "Burst " + itoaN(i), Status: "ready"}}))
		if out.Pushed || !out.PushPending {
			t.Fatalf("create %d: pushed=%v pending=%v", i, out.Pushed, out.PushPending)
		}
		ids = append(ids, out.Created[0].ID)
	}
	w := must[*WriteOut](t)(s.Comment(ctx, agent("a"), CommentIn{ID: ids[0], Body: "noted"}))
	if !w.PushPending {
		t.Fatalf("comment: %+v", w)
	}
	if n := pushes(); n != 0 {
		t.Fatalf("%d pushes before the quiet period", n)
	}
	// A claim pushes right away, carrying the pending commits with it.
	c := must[*ClaimOut](t)(s.Claim(ctx, agent("a"), ids[1], false))
	if !c.Confirmed || pushes() != 1 {
		t.Fatalf("claim: confirmed=%v pushes=%d", c.Confirmed, pushes())
	}
	must[*WriteOut](t)(s.Comment(ctx, agent("a"), CommentIn{ID: ids[2], Body: "one more"}))
	must[*WriteOut](t)(s.Comment(ctx, agent("b"), CommentIn{ID: ids[2], Body: "and another"}))
	// Wait for the quiet period. On a slow machine the two comments may
	// fall into separate debounce windows, so allow one extra push.
	deadline := time.Now().Add(10 * time.Second)
	for (pushes() < 2 || s.debouncePending(st)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := pushes(); n < 2 || n > 3 {
		t.Fatalf("after the quiet period: %d pushes, want 2 (3 on a slow machine)", n)
	}
	// Everything reached the remote.
	fresh, _ := e.clone()
	d := must[*Detail](t)(fresh.Get(GetIn{ID: ids[2], Include: []string{"comments"}}))
	if len(d.Comments) != 2 {
		t.Fatalf("remote has %d comments on %s", len(d.Comments), ids[2])
	}
	// Flush pushes immediately instead of waiting.
	before := pushes()
	must[*WriteOut](t)(s.Comment(ctx, agent("a"), CommentIn{ID: ids[0], Body: "flush me"}))
	s.Flush(ctx)
	if n := pushes(); n != before+1 {
		t.Fatalf("after Flush: %d pushes, want %d", n, before+1)
	}

	// A CLI-style process (not Background) pushes immediately even in debounce mode.
	cli, _ := e.clone()
	if out := must[*CreateOut](t)(cli.Create(ctx, agent("c"), []CreateIn{{Title: "Now"}})); !out.Pushed || out.PushPending {
		t.Fatalf("non-background create: %+v", out)
	}
}

// The user's workspace settings win over the store's recommendation.
func TestWorkspacePushWins(t *testing.T) {
	e := newEnv(t, map[string]string{".ka/config.yml": "runtime:\n  sync:\n    push: debounce:300ms\n"})
	_, dir := e.clone()
	wsFile := filepath.Join(e.root, "ws.yml")
	os.WriteFile(wsFile, []byte("name: t\nstores:\n  - name: s\n    path: "+filepath.ToSlash(dir)+"\n    push: manual\n    fetch_interval: 5s\n"), 0o644)
	ws, err := Open(context.Background(), OpenOptions{Dir: e.root, Workspace: wsFile})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if st := s.WS.Stores[0]; st.PushSetting() != "manual" || st.FetchInterval != 5*time.Second {
		t.Fatalf("workspace settings lost: %s %v", st.PushSetting(), st.FetchInterval)
	}
	os.WriteFile(wsFile, []byte("name: t\nstores:\n  - name: s\n    path: "+filepath.ToSlash(dir)+"\n    push: often\n"), 0o644)
	if _, err := Open(context.Background(), OpenOptions{Dir: e.root, Workspace: wsFile}); err == nil || !strings.Contains(err.Error(), "debounce") {
		t.Fatalf("bad push mode: %v", err)
	}
}

// The fetch loop brings in other clones' pushes on its interval and when
// kicked.
func TestFetchLoop(t *testing.T) {
	e := newEnv(t, nil)
	s, _ := e.clone()
	st := s.WS.Stores[0]
	st.FetchInterval = time.Hour
	s.Background = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.FetchLoop(ctx)

	other, _ := e.clone()
	out := must[*CreateOut](t)(other.Create(context.Background(), agent("o"), []CreateIn{{Title: "Elsewhere"}}))
	id := out.Created[0].ID
	time.Sleep(400 * time.Millisecond)
	if _, err := s.Get(GetIn{ID: id}); err == nil {
		t.Fatal("fetched before the interval or a kick")
	}
	s.Kick("")
	waitFor(t, func() bool { _, err := s.Get(GetIn{ID: id}); return err == nil })

	// On the interval.
	st.FetchInterval = 300 * time.Millisecond
	s.Kick(st.Name) // reschedules with the new interval
	out = must[*CreateOut](t)(other.Create(context.Background(), agent("o"), []CreateIn{{Title: "Later"}}))
	waitFor(t, func() bool { _, err := s.Get(GetIn{ID: out.Created[0].ID}); return err == nil })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRepoKey(t *testing.T) {
	want := "github.com/jumpkickoss/ka-org"
	for _, u := range []string{"git@github.com:JumpKickOSS/ka-org.git", "https://github.com/JumpKickOSS/ka-org", "https://github.com/JumpKickOSS/ka-org.git/",
		"ssh://git@github.com:22/JumpKickOSS/ka-org.git", "https://token@github.com/JumpKickOSS/ka-org.git"} {
		if got := repoKey(u); got != want {
			t.Errorf("repoKey(%q) = %q", u, got)
		}
	}
	if repoKey("/srv/git/ka.git") != "/srv/git/ka" {
		t.Errorf("local path: %q", repoKey("/srv/git/ka.git"))
	}
}
