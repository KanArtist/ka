package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KanArtist/ka/internal/api"
)

// ka watch (PRD §14): the activity stream in the terminal. It reads the
// daemon's history and then its live event stream (starting the daemon if
// needed), the same events the web UI's Activity view shows.

type watchEvent struct {
	ID      uint64          `json:"id"`
	Type    string          `json:"type"`
	At      string          `json:"at"`
	Store   string          `json:"store"`
	Project string          `json:"project"`
	Ticket  string          `json:"ticket"`
	Actor   string          `json:"actor"`
	Commit  string          `json:"commit"`
	Text    string          `json:"text"`
	Data    json.RawMessage `json:"data"`
}

func cmdWatch(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("watch", "[--history N] [--agents|--people] [--all] [QUERY...]", x)
	history := fl.Int("history", 15, "show this many past events first")
	agents := fl.Bool("agents", false, "only what agents do")
	people := fl.Bool("people", false, "only what people do")
	all := fl.Bool("all", false, "include sync and session (presence) events")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	rt, ws, err := runningDaemon(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	if rt == nil {
		if rt, err = startDaemon(ctx, x, c, ws); err != nil {
			return fail(x, c, err)
		}
		fmt.Fprintf(x.Err, "started ka daemon at %s (stop it with `ka daemon stop`)\n", rt.URL)
	}
	w := &watcher{x: x, rt: rt, json: c.json, agents: *agents, people: *people, all: *all,
		color: !c.json && isTerminal(x.Out) && os.Getenv("NO_COLOR") == "", client: &http.Client{}}
	if q := strings.Join(pos, " "); q != "" {
		if err := w.refreshMatch(ctx, q); err != nil {
			return fail(x, c, err)
		}
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
					w.refreshMatch(ctx, q)
				}
			}
		}()
	}
	var last uint64
	if *history > 0 {
		last = w.showHistory(ctx, *history)
	}
	if !c.json {
		fmt.Fprintln(x.Err, w.dim("watching "+rt.URL+" (Ctrl-C to stop)"))
	}
	for ctx.Err() == nil {
		n, err := w.stream(ctx, last)
		if n > last {
			last = n
		}
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			fmt.Fprintln(x.Err, w.dim("disconnected ("+err.Error()+"); reconnecting…"))
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return 0
}

type watcher struct {
	x              *IO
	rt             *api.Runtime
	client         *http.Client
	json, color    bool
	agents, people bool
	all            bool
	mu             sync.Mutex
	match          map[string]bool // nil: no query
	titles         map[string]string
	lastDay        string
}

// get fetches JSON and returns the response's event sequence (Ka-Event-Seq).
func (w *watcher) get(ctx context.Context, path string, out any) (uint64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.rt.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+w.rt.Token)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var p struct{ Detail, Fix string }
		json.NewDecoder(resp.Body).Decode(&p)
		return 0, fmt.Errorf("%s (%s)", p.Detail, p.Fix)
	}
	seq, _ := strconv.ParseUint(resp.Header.Get(api.SeqHeader), 10, 64)
	return seq, json.NewDecoder(resp.Body).Decode(out)
}

func (w *watcher) refreshMatch(ctx context.Context, q string) error {
	var res struct {
		Tickets []struct{ ID, Title string } `json:"tickets"`
	}
	if _, err := w.get(ctx, "/api/v1/tickets?all=true&limit=-1&q="+url.QueryEscape(q), &res); err != nil {
		return err
	}
	m := map[string]bool{}
	for _, t := range res.Tickets {
		m[t.ID] = true
	}
	w.mu.Lock()
	w.match = m
	w.mu.Unlock()
	return nil
}

func (w *watcher) showHistory(ctx context.Context, n int) uint64 {
	var out struct {
		Events []watchEvent `json:"events"`
	}
	// Fetch more than shown: filters drop some. Live events resume after
	// the sequence current when the history was read.
	seq, err := w.get(ctx, "/api/v1/activity?limit="+strconv.Itoa(min(n*4, 500)), &out)
	if err != nil {
		fmt.Fprintln(w.x.Err, "ka watch: history:", err)
		return 0
	}
	var keep []watchEvent
	for _, e := range out.Events { // newest first
		if w.wanted(e) {
			keep = append(keep, e)
		}
		if len(keep) == n {
			break
		}
	}
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].At < keep[j].At })
	for _, e := range keep {
		w.print(e)
	}
	return seq
}

// stream reads the SSE stream until it ends; it returns the last event ID.
func (w *watcher) stream(ctx context.Context, since uint64) (uint64, error) {
	path := "/api/v1/events"
	if since > 0 {
		path += "?last_event_id=" + strconv.FormatUint(since, 10)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.rt.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+w.rt.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := w.client.Do(req)
	if err != nil {
		return since, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return since, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	last := since
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if data.Len() == 0 {
				continue
			}
			var e watchEvent
			if json.Unmarshal([]byte(data.String()), &e) == nil {
				if e.ID > last {
					last = e.ID
				}
				if w.wanted(e) {
					w.print(e)
				}
			}
			data.Reset()
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return last, err
	}
	return last, io.EOF
}

func (w *watcher) wanted(e watchEvent) bool {
	switch e.Type {
	case "reset", "question.opened", "question.answered":
		return false
	case "presence.changed":
		if !w.all || strings.Contains(string(e.Data), `"state":"active"`) {
			return false
		}
	case "sync.state", "sync.conflict_preserved", "store.changed":
		if !w.all {
			return false
		}
	}
	agent := strings.HasPrefix(e.Actor, "agent/")
	if (w.agents && !agent) || (w.people && agent) {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.match != nil && (e.Ticket == "" || !w.match[e.Ticket]) {
		return false
	}
	return true
}

func (w *watcher) print(e watchEvent) {
	if w.json {
		b, _ := json.Marshal(e)
		fmt.Fprintln(w.x.Out, string(b))
		return
	}
	verb, quote, title := describeEvent(e)
	if w.titles == nil {
		w.titles = map[string]string{}
	}
	if title != "" {
		w.titles[e.Ticket] = title
	} else {
		title = w.titles[e.Ticket]
	}
	t, err := time.Parse(time.RFC3339, e.At)
	stamp := e.At
	if err == nil {
		t = t.Local()
		if day := t.Format("Mon Jan 2"); day != w.lastDay {
			if w.lastDay != "" || time.Since(t) > 20*time.Hour {
				fmt.Fprintln(w.x.Out, w.dim("── "+day))
			}
			w.lastDay = day
		}
		stamp = t.Format("15:04:05")
	}
	actor := e.Actor
	if actor == "" {
		actor = "someone"
	}
	line := w.dim(stamp) + "  " + w.actor(actor)
	if e.Ticket != "" {
		line += "  " + w.bold(e.Ticket)
	}
	line += "  " + verb
	if title != "" {
		line += w.dim("  " + clipText(title, 70))
	}
	fmt.Fprintln(w.x.Out, line)
	if quote != "" {
		fmt.Fprintln(w.x.Out, "          "+w.dim("│ ")+clipText(quote, 160))
	}
}

func (w *watcher) dim(s string) string {
	if !w.color {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

func (w *watcher) bold(s string) string {
	if !w.color {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// actor prints agents in verdigris and people in ochre, as the web UI does.
func (w *watcher) actor(a string) string {
	name := strings.TrimPrefix(a, "agent/")
	if !w.color {
		if strings.HasPrefix(a, "agent/") {
			return "⬡ " + name
		}
		return "● " + name
	}
	if strings.HasPrefix(a, "agent/") {
		return "\x1b[38;5;73m⬡ " + name + "\x1b[0m"
	}
	return "\x1b[38;5;179m● " + name + "\x1b[0m"
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// describeEvent puts an event in plain words (the web UI's describe.ts).
func describeEvent(e watchEvent) (verb, quote, title string) {
	var d struct {
		Ticket struct {
			Title, Status string
		} `json:"ticket"`
		From     json.RawMessage `json:"from"`
		To       json.RawMessage `json:"to"`
		Category string          `json:"category"`
		Changes  map[string]struct {
			From, To any
		} `json:"changes"`
		BodyChanged bool `json:"body_changed"`
		Comment     struct {
			Type, Body, To, Resolution string
			ChoiceText                 string `json:"choice_text"`
			Approved                   *bool
		} `json:"comment"`
		State   string `json:"state"`
		Detail  string `json:"detail"`
		Session struct {
			Client string `json:"client"`
		} `json:"session"`
	}
	json.Unmarshal(e.Data, &d)
	title = d.Ticket.Title
	str := func(r json.RawMessage) string {
		var s string
		json.Unmarshal(r, &s)
		return s
	}
	switch e.Type {
	case "ticket.created":
		return "created it", "", title
	case "ticket.moved":
		from, to := str(d.From), str(d.To)
		if to == "" {
			to = d.Ticket.Status
		}
		if from != "" {
			return "moved it " + from + " → " + to, "", title
		}
		return "moved it to " + to, "", title
	case "ticket.removed":
		return "removed it", "", title
	case "claim.changed":
		if len(d.To) > 0 && string(d.To) != "null" {
			return "claimed it", "", title
		}
		return "released it", "", title
	case "ticket.updated":
		var parts []string
		keys := make([]string, 0, len(d.Changes))
		for k := range d.Changes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c := d.Changes[k]
			switch v := c.To.(type) {
			case string:
				if k == "acceptance" {
					parts = append(parts, "checked acceptance "+v)
				} else {
					parts = append(parts, "set "+strings.TrimPrefix(k, "fields.")+" to "+v)
				}
			case nil:
				parts = append(parts, "cleared "+strings.TrimPrefix(k, "fields."))
			case map[string]any:
				var ch []string
				for _, a := range asStrings(v["add"]) {
					ch = append(ch, "+"+a)
				}
				for _, r := range asStrings(v["remove"]) {
					ch = append(ch, "−"+r)
				}
				parts = append(parts, k+" "+strings.Join(ch, " "))
			case []any:
				from := map[string]bool{}
				for _, f := range asStrings(c.From) {
					from[f] = true
				}
				now := map[string]bool{}
				var ch []string
				for _, t := range asStrings(v) {
					now[t] = true
					if !from[t] {
						ch = append(ch, "+"+t)
					}
				}
				for _, f := range asStrings(c.From) {
					if !now[f] {
						ch = append(ch, "−"+f)
					}
				}
				parts = append(parts, strings.TrimSpace(k+" "+strings.Join(ch, " ")))
			default:
				parts = append(parts, "changed "+k)
			}
		}
		if d.BodyChanged {
			parts = append(parts, "edited the description")
		}
		if len(parts) == 0 {
			return "updated it", "", title
		}
		return strings.Join(parts, ", "), "", title
	case "comment.created":
		c := d.Comment
		switch c.Type {
		case "progress":
			return "posted progress", c.Body, title
		case "decision":
			return "recorded a decision", c.Body, title
		case "resolution":
			res := c.Resolution
			if res == "" {
				res = "done"
			}
			return "closed it as " + res, c.Body, title
		case "question":
			if c.To != "" {
				return "asked " + c.To, c.Body, title
			}
			return "asked a question", c.Body, title
		case "answer":
			if c.ChoiceText != "" {
				return "answered “" + c.ChoiceText + "”", c.Body, title
			}
			return "answered", c.Body, title
		case "approval":
			if c.Approved != nil && *c.Approved {
				return "approved", c.Body, title
			}
			return "rejected", c.Body, title
		case "system":
			return "kept a conflicting edit", c.Body, title
		}
		return "commented", c.Body, title
	case "presence.changed":
		if d.State == "connected" {
			if d.Session.Client != "" {
				return "connected from " + d.Session.Client, "", ""
			}
			return "connected", "", ""
		}
		return "disconnected", "", ""
	case "sync.state":
		s := e.Store + " is " + d.State
		if d.Detail != "" {
			s += " (" + d.Detail + ")"
		}
		return s, "", ""
	case "sync.conflict_preserved":
		return "kept conflicting edits as comments", "", ""
	case "store.changed":
		return "changed the configuration of " + e.Store, "", ""
	case "commit":
		return e.Text, "", ""
	}
	if e.Text != "" {
		return e.Text, "", ""
	}
	return e.Type, "", ""
}

func asStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
