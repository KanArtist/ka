// Package presence tracks who is connected to a daemon right now (PRD §8.1):
// MCP sessions, their last tool call, and when they were last seen. It is
// ephemeral and per daemon, and never written to git.
package presence

import (
	"sort"
	"sync"
	"time"

	"github.com/JumpKickOSS/ka/internal/engine"
)

// IdleTimeout drops a session that made no call for this long (a client
// that went away without closing).
const IdleTimeout = 30 * time.Minute

// Session is one connected agent session.
type Session struct {
	ID         string `json:"id"`
	Actor      string `json:"actor"`
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	Client     string `json:"client,omitempty" doc:"clientInfo name and version."`
	Transport  string `json:"transport" doc:"mcp-http (directly or through the ka mcp proxy)."`
	Connected  string `json:"connected"`
	LastSeen   string `json:"last_seen"`
	LastMethod string `json:"last_method,omitempty"`
	LastTool   string `json:"last_tool,omitempty"`
	LastTicket string `json:"last_ticket,omitempty"`
	LastError  string `json:"last_error,omitempty" doc:"The error code of the last call, if it failed."`
	Calls      int    `json:"calls"`
}

// Change is the presence.changed payload.
type Change struct {
	State   string  `json:"state" doc:"connected, active or disconnected."`
	Session Session `json:"session"`
}

// Call describes one request a session made.
type Call struct {
	Method, Tool, Ticket, Error string
}

// Tracker holds the live sessions.
type Tracker struct {
	mu       sync.Mutex
	sessions map[string]*Session
	sink     engine.EventSink
	Now      func() time.Time
}

// New returns a tracker that publishes presence.changed events to sink (may be nil).
func New(sink engine.EventSink) *Tracker {
	return &Tracker{sessions: map[string]*Session{}, sink: sink, Now: time.Now}
}

func (t *Tracker) stamp() string { return t.Now().UTC().Format(time.RFC3339) }

// Seen records a call. It reports whether the session is new.
func (t *Tracker) Seen(id string, who Session, c Call) bool {
	if t == nil || id == "" {
		return false
	}
	t.mu.Lock()
	now := t.stamp()
	s, ok := t.sessions[id]
	state := "active"
	if !ok {
		who.ID, who.Connected = id, now
		if who.Transport == "" {
			who.Transport = "mcp-http"
		}
		s = &who
		t.sessions[id] = s
		state = "connected"
	} else if who.Actor != "" {
		s.Actor, s.OnBehalfOf = who.Actor, who.OnBehalfOf
		if who.Client != "" {
			s.Client = who.Client
		}
	}
	s.LastSeen = now
	s.Calls++
	if c.Method != "" {
		s.LastMethod = c.Method
	}
	if c.Tool != "" {
		s.LastTool, s.LastTicket = c.Tool, c.Ticket
	}
	s.LastError = c.Error
	snap := *s
	t.mu.Unlock()
	t.publish(state, snap)
	return !ok
}

// Closed removes a session.
func (t *Tracker) Closed(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	s, ok := t.sessions[id]
	if ok {
		delete(t.sessions, id)
	}
	t.mu.Unlock()
	if ok {
		snap := *s
		snap.LastSeen = t.stamp()
		t.publish("disconnected", snap)
	}
}

// Sweep drops sessions not seen for longer than idle (for clients that went
// away without closing).
func (t *Tracker) Sweep(idle time.Duration) {
	if t == nil {
		return
	}
	cutoff := t.Now().Add(-idle).UTC().Format(time.RFC3339)
	t.mu.Lock()
	var gone []string
	for id, s := range t.sessions {
		if s.LastSeen < cutoff {
			gone = append(gone, id)
		}
	}
	t.mu.Unlock()
	for _, id := range gone {
		t.Closed(id)
	}
}

// List returns the live sessions, most recently active first.
func (t *Tracker) List() []Session {
	out := []Session{}
	if t == nil {
		return out
	}
	t.mu.Lock()
	for _, s := range t.sessions {
		out = append(out, *s)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (t *Tracker) publish(state string, s Session) {
	if t.sink == nil {
		return
	}
	t.sink.Publish(engine.Event{Type: engine.EvPresenceChanged, At: t.stamp(), Ticket: s.LastTicket, Actor: s.Actor,
		Data: Change{State: state, Session: s}})
}
