package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Domain events (PRD §8.1, §12.2). The index diffs every ticket it reloads,
// before and after, so writes through this process, commits by other
// processes on the clone and fetched upstream changes all produce the same
// events. Payloads carry the new summary, so clients need no refetch.

// Event types.
const (
	EvTicketCreated     = "ticket.created"
	EvTicketUpdated     = "ticket.updated"
	EvTicketMoved       = "ticket.moved" // status changed
	EvTicketRemoved     = "ticket.removed"
	EvCommentCreated    = "comment.created"
	EvClaimChanged      = "claim.changed"
	EvQuestionOpened    = "question.opened"
	EvQuestionAnswered  = "question.answered"
	EvSyncState         = "sync.state"
	EvConflictPreserved = "sync.conflict_preserved"
	EvStoreChanged      = "store.changed"
	EvPresenceChanged   = "presence.changed"
	EvReset             = "reset"
)

// EventTypes lists every event type.
var EventTypes = []string{EvTicketCreated, EvTicketUpdated, EvTicketMoved, EvTicketRemoved, EvCommentCreated, EvClaimChanged,
	EvQuestionOpened, EvQuestionAnswered, EvSyncState, EvConflictPreserved, EvStoreChanged, EvPresenceChanged, EvReset}

// Event is one domain event.
type Event struct {
	Seq     uint64 `json:"id" doc:"Monotonic per daemon; also the SSE id."`
	Type    string `json:"type"`
	At      string `json:"at"`
	Store   string `json:"store,omitempty"`
	Project string `json:"project,omitempty"`
	Ticket  string `json:"ticket,omitempty"`
	Actor   string `json:"actor,omitempty" doc:"Who caused it, when known."`
	Data    any    `json:"data,omitempty"`
}

// FieldChange is one frontmatter field's old and new value.
type FieldChange struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// TicketEvent is the payload of ticket.created, ticket.updated and ticket.moved.
type TicketEvent struct {
	Ticket      Summary                `json:"ticket"`
	Changes     map[string]FieldChange `json:"changes,omitempty"`
	BodyChanged bool                   `json:"body_changed,omitempty"`
	From        string                 `json:"from,omitempty"` // ticket.moved: the old status
	To          string                 `json:"to,omitempty"`   // ticket.moved: the new status
	Category    string                 `json:"category,omitempty"`
}

// ClaimEvent is the payload of claim.changed.
type ClaimEvent struct {
	Ticket Summary `json:"ticket"`
	From   *Claim  `json:"from,omitempty"`
	To     *Claim  `json:"to,omitempty"`
}

// CommentEvent is the payload of comment.created and question.*.
type CommentEvent struct {
	Ticket  Ref        `json:"ticket"`
	Comment CommentOut `json:"comment"`
}

// EventSink receives events.
type EventSink interface {
	Publish(evs ...Event)
}

// SetEvents makes the service publish domain events to sink.
func (s *Service) SetEvents(sink EventSink) {
	s.ix.mu.Lock()
	defer s.ix.mu.Unlock()
	s.events = sink
	if sink == nil {
		s.ix.emit = nil
		return
	}
	s.ix.emit = s.ticketEvents
}

func (s *Service) publish(evs ...Event) {
	if s.events != nil && len(evs) > 0 {
		s.events.Publish(evs...)
	}
}

// ticketEvents turns reloaded tickets into events. It runs inside
// Index.refresh with the index write lock held, so it reads the index
// directly.
func (s *Service) ticketEvents(changes []recChange, actor string, rebuilt []*Store) {
	v := view{ix: s.ix, now: s.Now()}
	at := s.now()
	var evs []Event
	for _, st := range rebuilt {
		evs = append(evs, Event{Type: EvStoreChanged, At: at, Store: st.Name})
	}
	for _, c := range changes {
		evs = append(evs, s.diffEvents(v, at, actor, c.old, c.new)...)
	}
	s.publish(evs...)
}

type recChange struct {
	id       string
	old, new *Rec
}

func (s *Service) diffEvents(v view, at, actor string, old, nw *Rec) []Event {
	base := func(typ string, r *Rec) Event {
		return Event{Type: typ, At: at, Store: r.Store.Name, Project: r.Project.Key, Ticket: r.ID, Actor: actor}
	}
	if nw == nil {
		e := base(EvTicketRemoved, old)
		e.Data = TicketEvent{Ticket: s.summary(v, old)}
		return []Event{e}
	}
	sum := s.summary(v, nw)
	if old == nil {
		e := base(EvTicketCreated, nw)
		if e.Actor == "" {
			e.Actor = nw.T.CreatedBy
		}
		e.Data = TicketEvent{Ticket: sum}
		return []Event{e}
	}
	var evs []Event
	if !bytes.Equal(old.Raw, nw.Raw) {
		changes := fieldChanges(old.T, nw.T)
		bodyChanged := old.T.Body != nw.T.Body
		if old.T.Status != nw.T.Status {
			e := base(EvTicketMoved, nw)
			e.Data = TicketEvent{Ticket: sum, Changes: changes, BodyChanged: bodyChanged, From: old.T.Status, To: nw.T.Status, Category: category(nw.Project.Cfg, nw.T.Status)}
			evs = append(evs, e)
		} else if len(changes) > 0 || bodyChanged {
			e := base(EvTicketUpdated, nw)
			e.Data = TicketEvent{Ticket: sum, Changes: changes, BodyChanged: bodyChanged}
			evs = append(evs, e)
		}
		if !reflect.DeepEqual(old.T.Claim, nw.T.Claim) {
			e := base(EvClaimChanged, nw)
			ce := ClaimEvent{Ticket: sum}
			if c := old.T.Claim; c != nil {
				ce.From = &Claim{By: c.By, At: c.At}
				if e.Actor == "" && nw.T.Claim == nil {
					e.Actor = c.By
				}
			}
			if c := nw.T.Claim; c != nil {
				ce.To = &Claim{By: c.By, At: c.At}
				if e.Actor == "" {
					e.Actor = c.By
				}
			}
			e.Data = ce
			evs = append(evs, e)
		}
	}
	seen := make(map[string]bool, len(old.Comments))
	for _, c := range old.Comments {
		seen[c.Name] = true
	}
	ref := Ref{ID: nw.ID, Title: nw.T.Title, Status: nw.T.Status}
	for _, c := range nw.Comments {
		if seen[c.Name] || c.Comment == nil {
			continue
		}
		co := commentOut(c)
		e := base(EvCommentCreated, nw)
		e.Actor = co.Author
		e.Data = CommentEvent{Ticket: ref, Comment: co}
		evs = append(evs, e)
		switch co.Type {
		case "question":
			q := e
			q.Type = EvQuestionOpened
			evs = append(evs, q)
		case "answer", "approval":
			q := e
			q.Type = EvQuestionAnswered
			evs = append(evs, q)
		}
	}
	return evs
}

// fieldChanges compares frontmatter fields other than claim and updated
// (claims have their own event; updated changes on every write).
func fieldChanges(a, b any) map[string]FieldChange {
	am, bm := frontmatterMap(a), frontmatterMap(b)
	out := map[string]FieldChange{}
	for k, bv := range bm {
		if av, ok := am[k]; !ok || !reflect.DeepEqual(av, bv) {
			out[k] = FieldChange{From: am[k], To: bv}
		}
	}
	for k, av := range am {
		if _, ok := bm[k]; !ok {
			out[k] = FieldChange{From: av, To: nil}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func frontmatterMap(t any) map[string]any {
	b, _ := json.Marshal(t)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"claim", "updated", "body"} {
		delete(m, k)
	}
	return m
}

// ---- the hub -----------------------------------------------------------------

// EventHub numbers events and keeps the most recent ones in a ring buffer
// for replay (Last-Event-ID). Sequence numbers start at the daemon's start
// time in microseconds, so IDs from an earlier daemon are always older than
// the buffer and get a reset.
type EventHub struct {
	mu    sync.Mutex
	seq   uint64
	first uint64 // sequence number of ring[head]
	ring  []Event
	head  int
	n     int
	wake  chan struct{}
}

// NewEventHub keeps the last size events.
func NewEventHub(size int) *EventHub {
	if size <= 0 {
		size = 10000
	}
	seq := uint64(time.Now().UnixMicro())
	return &EventHub{seq: seq, first: seq + 1, ring: make([]Event, size), wake: make(chan struct{})}
}

// Publish assigns sequence numbers and wakes waiting readers.
func (h *EventHub) Publish(evs ...Event) {
	if len(evs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range evs {
		h.seq++
		e.Seq = h.seq
		if h.n < len(h.ring) {
			h.ring[(h.head+h.n)%len(h.ring)] = e
			h.n++
		} else {
			h.ring[h.head] = e
			h.head = (h.head + 1) % len(h.ring)
			h.first++
		}
	}
	close(h.wake)
	h.wake = make(chan struct{})
}

// Seq is the last assigned sequence number.
func (h *EventHub) Seq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// Since returns the events after seq. ok is false when some of them are no
// longer buffered (or seq is not from this hub); the caller must reset. The
// channel is closed when more events arrive.
func (h *EventHub) Since(seq uint64) (evs []Event, ok bool, more <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq > h.seq || seq+1 < h.first {
		return nil, false, h.wake
	}
	for i := 0; i < h.n; i++ {
		e := h.ring[(h.head+i)%len(h.ring)]
		if e.Seq > seq {
			evs = append(evs, e)
		}
	}
	return evs, true, h.wake
}

// ---- filters -----------------------------------------------------------------

// EventFilter selects events by type (prefix patterns such as ticket.*),
// project and ticket. Empty lists match everything; reset always matches.
type EventFilter struct {
	Types, Projects, Tickets []string
}

// ParseEventFilter reads comma-separated lists.
func ParseEventFilter(types, projects, tickets string) EventFilter {
	split := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	f := EventFilter{Types: split(types), Projects: split(projects), Tickets: split(tickets)}
	for i, t := range f.Tickets {
		f.Tickets[i] = strings.ToUpper(t)
	}
	return f
}

// Match reports whether an event passes the filter.
func (f EventFilter) Match(e Event) bool {
	if e.Type == EvReset {
		return true
	}
	if len(f.Types) > 0 && !matchAny(f.Types, e.Type) {
		return false
	}
	if len(f.Projects) > 0 && e.Project != "" && !contains(f.Projects, e.Project) {
		return false
	}
	if len(f.Tickets) > 0 && !contains(f.Tickets, e.Ticket) {
		return false
	}
	return true
}

func matchAny(patterns []string, typ string) bool {
	for _, p := range patterns {
		if p == typ || p == "*" || strings.HasSuffix(p, ".*") && strings.HasPrefix(typ, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- local watching and sync events -------------------------------------------

// WatchLocal notices commits made by other processes sharing the clones
// (the CLI, another ka mcp) every interval, so their events reach
// subscribers without waiting for the next request.
func (s *Service) WatchLocal(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Skip while this process is writing: its own refresh attributes the change.
			if s.writeMu.TryLock() {
				s.ix.catchUp()
				s.writeMu.Unlock()
			}
		}
	}
}

// syncEvents reports a write's push outcome when the store's state changed,
// and any values preserved by the merge driver or replay.
func (s *Service) syncEvents(st *Store, res *Result) {
	if s.events == nil || res == nil {
		return
	}
	at := s.now()
	var evs []Event
	if res.HasRemote {
		state := SyncState{Store: st.Name, Attached: !st.Managed, Head: short(res.Commit)}
		switch {
		case res.Pushed:
			state.State = "synced"
		case res.PushError != "":
			state.State, state.Detail = "ahead", res.PushError
		default:
			state.State = "ahead"
		}
		s.syncMu.Lock()
		changed := s.lastSync[st.Name] != state.State+"\x00"+state.Detail
		if s.lastSync == nil {
			s.lastSync = map[string]string{}
		}
		s.lastSync[st.Name] = state.State + "\x00" + state.Detail
		s.syncMu.Unlock()
		if changed {
			evs = append(evs, Event{Type: EvSyncState, At: at, Store: st.Name, Data: state})
		}
	}
	if len(res.Preserved) > 0 {
		evs = append(evs, Event{Type: EvConflictPreserved, At: at, Store: st.Name, Data: map[string]any{"preserved": res.Preserved}})
	}
	s.publish(evs...)
}

// syncStateEvents publishes every store's full state (after ka sync).
func (s *Service) syncStateEvents(states []SyncState) {
	if s.events == nil {
		return
	}
	at := s.now()
	evs := make([]Event, 0, len(states))
	s.syncMu.Lock()
	if s.lastSync == nil {
		s.lastSync = map[string]string{}
	}
	for _, st := range states {
		s.lastSync[st.Store] = st.State + "\x00" + st.Detail
		evs = append(evs, Event{Type: EvSyncState, At: at, Store: st.Store, Data: st})
	}
	s.syncMu.Unlock()
	sort.Slice(evs, func(i, j int) bool { return evs[i].Store < evs[j].Store })
	s.publish(evs...)
}
