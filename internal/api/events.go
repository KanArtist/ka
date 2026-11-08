package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/presence"
)

// SeqHeader carries the hub's last event ID on every API response, so a
// client can take a snapshot and then subscribe from exactly that point.
const SeqHeader = "Ka-Event-Seq"

// events serves GET /api/v1/events (PRD §12.2) as Server-Sent Events.
//
// A client without Last-Event-ID gets events from now on. One with a
// Last-Event-ID still in the ring buffer gets everything after it; one that
// is too old (or from an earlier daemon) gets a reset event and should
// refetch its snapshot.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := engine.ParseEventFilter(q.Get("types"), q.Get("projects"), q.Get("tickets"))
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = q.Get("last_event_id")
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	hub := s.hub
	cursor := hub.Seq()
	if last != "" {
		n, err := strconv.ParseUint(last, 10, 64)
		if err != nil {
			n = 0 // unparseable: treat as too old
		}
		cursor = n
	}
	fmt.Fprintf(w, "retry: 2000\n: ka %s\n\n", s.opts.Version)
	rc.Flush()

	heartbeat := time.NewTicker(s.heartbeat)
	defer heartbeat.Stop()
	for {
		evs, ok, more := hub.Since(cursor)
		if !ok {
			cursor = hub.Seq()
			if err := writeEvent(w, engine.Event{Seq: cursor, Type: engine.EvReset, At: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				return
			}
			rc.Flush()
			continue
		}
		wrote := false
		for _, e := range evs {
			cursor = e.Seq
			if !filter.Match(e) {
				continue
			}
			if err := writeEvent(w, e); err != nil {
				return
			}
			wrote = true
		}
		if wrote {
			rc.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-more:
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			rc.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e engine.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, data)
	return err
}

// documentEvents adds the SSE endpoint, which is served outside Huma, to
// the OpenAPI document.
func (s *Server) documentEvents() {
	reg := s.api.OpenAPI().Components.Schemas
	evSchema := huma.SchemaFromType(reg, reflect.TypeFor[engine.Event]())
	for _, t := range []any{engine.TicketEvent{}, engine.ClaimEvent{}, engine.CommentEvent{}, engine.SyncState{}, presence.Change{}} {
		huma.SchemaFromType(reg, reflect.TypeOf(t))
	}
	str := &huma.Schema{Type: huma.TypeString}
	s.api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "events",
		Method:      http.MethodGet,
		Path:        "/api/v1/events",
		Summary:     "Live domain events (Server-Sent Events)",
		Description: "Each SSE message has `id` (the event's sequence number), `event` (its type) and `data` (the Event as JSON). " +
			"Reconnect with Last-Event-ID to replay what you missed; if it is too old you get a `reset` event and should refetch. " +
			"Every API response carries " + SeqHeader + ", the last event ID at that moment. A `: ping` comment is sent every 15 s. " +
			"Data payloads: TicketEvent (ticket.*), ClaimEvent (claim.changed), CommentEvent (comment.created, question.*), SyncState (sync.state), Change (presence.changed).",
		Tags: []string{"events"},
		Parameters: []*huma.Param{
			{Name: "types", In: "query", Description: "Comma-separated event types; a trailing .* matches a prefix (ticket.*).", Schema: str},
			{Name: "projects", In: "query", Description: "Comma-separated project keys.", Schema: str},
			{Name: "tickets", In: "query", Description: "Comma-separated ticket IDs.", Schema: str},
			{Name: "last_event_id", In: "query", Description: "Same as the Last-Event-ID header.", Schema: str},
			{Name: "Last-Event-ID", In: "header", Description: "Resume after this event.", Schema: str},
		},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "An event stream. Event types: " + fmt.Sprint(engine.EventTypes),
				Content: map[string]*huma.MediaType{
					"text/event-stream": {Schema: &huma.Schema{Type: huma.TypeArray, Items: evSchema}},
				},
			},
			"default": {
				Description: "Error",
				Content:     map[string]*huma.MediaType{"application/problem+json": {Schema: huma.SchemaFromType(reg, reflect.TypeFor[Problem]())}},
			},
		},
	})
}
