package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// Runtime sync behavior (PRD §9.3 class 3, §10.1): how a store pushes and
// how often a long-running process fetches it.

// Defaults.
const (
	DefaultFetchInterval = 15 * time.Second
	DefaultPushDelay     = 2 * time.Second
	// A debounced push waits for a quiet period, but never longer than this
	// many delays after the first pending commit.
	maxDebounceFactor = 5
)

// ParsePushMode parses immediate | manual | debounce | debounce:<duration>.
func ParsePushMode(s string) (mode string, delay time.Duration, err error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "immediate":
		return "immediate", 0, nil
	case s == "manual":
		return "manual", 0, nil
	case s == "debounce":
		return "debounce", DefaultPushDelay, nil
	case strings.HasPrefix(s, "debounce:"):
		d, err := time.ParseDuration(strings.TrimPrefix(s, "debounce:"))
		if err != nil || d <= 0 || d > time.Minute {
			return "", 0, fmt.Errorf("push %q: the debounce delay must be a duration up to 1m, such as debounce:2s", s)
		}
		return "debounce", d, nil
	}
	return "", 0, fmt.Errorf("push %q: use immediate, manual or debounce:<duration>", s)
}

// PushSetting renders a store's push mode as configured.
func (st *Store) PushSetting() string {
	if st.PushMode == "debounce" {
		return "debounce:" + st.PushDelay.String()
	}
	if st.PushMode == "" {
		return "immediate"
	}
	return st.PushMode
}

// applyRuntime fills in each store's push mode and fetch interval from the
// store's recommended runtime block (runtime.sync in .ka/config.yml, after
// extends), unless the workspace already set them: the user's settings win.
func (s *Service) applyRuntime() {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	for _, st := range s.WS.Stores {
		var rt *kaspec.Node
		for _, pi := range s.ix.projects {
			if pi.Store == st && pi.Cfg != nil && pi.Cfg.Runtime != nil {
				rt = pi.Cfg.Runtime
				break
			}
		}
		sync := child(rt, "sync")
		if !st.pushSet {
			if v := scalar(child(sync, "push")); v != "" {
				if mode, d, err := ParsePushMode(v); err == nil {
					st.PushMode, st.PushDelay = mode, d
				}
			}
		}
		if !st.fetchSet {
			if v := scalar(child(sync, "fetch_interval")); v != "" {
				if d, err := time.ParseDuration(v); err == nil && d >= time.Second {
					st.FetchInterval = d
				}
			}
		}
	}
}

func child(n *kaspec.Node, key string) *kaspec.Node {
	if n == nil || n.Map == nil {
		return nil
	}
	return n.Map[key]
}

func scalar(n *kaspec.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

// ---- debounced pushes ----------------------------------------------------------

type pendingPush struct {
	timer *time.Timer
	first time.Time
}

type debouncer struct {
	mu      sync.Mutex
	pending map[*Store]*pendingPush
	running map[*Store]int // debounced pushes under way
	wg      sync.WaitGroup
}

// debounced reports whether pushes to st are batched in this process.
// Only long-running processes batch; a CLI command exits right away, so it
// pushes immediately.
func (s *Service) debounced(st *Store) bool {
	return s.Background && st.PushMode == "debounce" && !s.NoPush
}

// schedulePush (re)arms st's push timer: the push happens once writes have
// been quiet for the store's delay, or after maxDebounceFactor delays.
func (s *Service) schedulePush(st *Store) {
	d := &s.deb
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending == nil {
		d.pending = map[*Store]*pendingPush{}
		d.running = map[*Store]int{}
	}
	now := time.Now()
	wait := st.PushDelay
	if p := d.pending[st]; p != nil {
		if limit := p.first.Add(maxDebounceFactor * st.PushDelay); now.Add(wait).After(limit) {
			wait = max(limit.Sub(now), 0)
		}
		if p.timer.Stop() {
			p.timer.Reset(wait)
			return
		}
		// The timer already fired; its push may be running. Schedule anew.
	}
	p := &pendingPush{first: now}
	d.wg.Add(1)
	p.timer = time.AfterFunc(wait, func() {
		defer d.wg.Done()
		d.mu.Lock()
		if d.pending[st] == p {
			delete(d.pending, st)
		}
		d.running[st]++
		d.mu.Unlock()
		s.pushNow(context.Background(), st)
		d.mu.Lock()
		d.running[st]--
		d.mu.Unlock()
	})
	d.pending[st] = p
}

func (s *Service) pushNow(ctx context.Context, st *Store) {
	s.writeMu.Lock()
	res, err := st.Sync(ctx, s.hooks(st))
	s.writeMu.Unlock()
	if err == nil {
		s.syncEvents(st, res)
	}
}

// Flush pushes every store with debounced commits now. Long-running
// processes call it before they exit.
func (s *Service) Flush(ctx context.Context) {
	d := &s.deb
	d.mu.Lock()
	var stores []*Store
	for st, p := range d.pending {
		if p.timer.Stop() {
			d.wg.Done()
			stores = append(stores, st)
		}
		delete(d.pending, st)
	}
	d.mu.Unlock()
	for _, st := range stores {
		s.pushNow(ctx, st)
	}
	d.wg.Wait() // pushes already under way
}

// ---- the fetch loop ------------------------------------------------------------

// Kick asks the fetch loop to sync a store now ("" means every store), as
// a forge webhook does. It never blocks.
func (s *Service) Kick(store string) {
	s.kickOnce.Do(func() { s.kick = make(chan string, 64) })
	select {
	case s.kick <- store:
	default:
	}
}

// FetchLoop syncs each store with a remote every FetchInterval, and
// whenever Kick is called, until ctx is done. Fetched changes reach the
// index (and so the event stream) like any other.
func (s *Service) FetchLoop(ctx context.Context) {
	s.kickOnce.Do(func() { s.kick = make(chan string, 64) })
	next := map[*Store]time.Time{}
	now := time.Now()
	for _, st := range s.WS.Stores {
		next[st] = now.Add(st.fetchEvery())
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		var due []*Store
		select {
		case <-ctx.Done():
			return
		case name := <-s.kick:
			for _, st := range s.WS.Stores {
				if name == "" || st.Name == name {
					due = append(due, st)
				}
			}
		case now := <-tick.C:
			for _, st := range s.WS.Stores {
				if !now.Before(next[st]) {
					due = append(due, st)
				}
			}
		}
		for _, st := range due {
			if ctx.Err() != nil {
				return
			}
			next[st] = time.Now().Add(st.fetchEvery())
			if remote, _ := st.upstream(ctx); remote == "" {
				continue
			}
			s.writeMu.Lock()
			var res *Result
			var err error
			if s.NoPush || st.PushMode == "manual" || st.ReadOnly || s.debouncePending(st) {
				res, err = st.FetchOnly(ctx, s.hooks(st))
			} else {
				res, err = st.Sync(ctx, s.hooks(st))
			}
			s.writeMu.Unlock()
			if err == nil {
				s.syncEvents(st, res)
			}
		}
	}
}

// debouncePending reports whether st has debounced commits not yet pushed:
// a push is scheduled or under way.
func (s *Service) debouncePending(st *Store) bool {
	s.deb.mu.Lock()
	defer s.deb.mu.Unlock()
	return s.deb.pending[st] != nil || s.deb.running[st] > 0
}

func (st *Store) fetchEvery() time.Duration {
	if st.FetchInterval > 0 {
		return st.FetchInterval
	}
	return DefaultFetchInterval
}

// StoresForRepo names the stores whose remote is one of urls, compared as
// host/path whatever the scheme (git@host:org/repo.git and
// https://host/org/repo are the same repository). Forge webhooks use it.
func (s *Service) StoresForRepo(urls ...string) []string {
	want := map[string]bool{}
	for _, u := range urls {
		if k := repoKey(u); k != "" {
			want[k] = true
		}
	}
	var out []string
	for _, st := range s.WS.Stores {
		if st.URL != "" && want[repoKey(st.URL)] {
			out = append(out, st.Name)
		}
	}
	return out
}

func repoKey(u string) string {
	u = strings.ReplaceAll(strings.TrimSpace(u), `\`, "/")
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if at := strings.LastIndex(strings.SplitN(u, "/", 2)[0], "@"); at >= 0 {
			u = u[at+1:]
		}
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[at:], ":") {
		u = strings.Replace(u[at+1:], ":", "/", 1) // scp-style git@host:org/repo
	}
	if h, rest, ok := strings.Cut(u, "/"); ok {
		if hh, _, ok := strings.Cut(h, ":"); ok { // drop a port
			h = hh
		}
		u = h + "/" + rest
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git"))
}
