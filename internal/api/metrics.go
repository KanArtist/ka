package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Prometheus metrics in daemon mode (PRD §17), written in the text
// exposition format without a client library. GET /metrics needs the same
// token as the API (Prometheus: authorization: {credentials: <token>}).

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type httpStats struct {
	mu      sync.Mutex
	count   map[[3]string]uint64 // method, route, code
	buckets map[[2]string][]uint64
	sum     map[[2]string]float64
	n       map[[2]string]uint64
	sse     atomic.Int64
}

var (
	idInPath  = regexp.MustCompile(`/[A-Z][A-Z0-9]{0,9}-[0-9A-HJKMNP-TV-Z]{5,8}(/|$)`)
	shaInPath = regexp.MustCompile(`/[0-9a-f]{7,64}(/|$)`)
)

// route turns a path into a low-cardinality label.
func route(p string) string {
	switch {
	case strings.HasPrefix(p, "/api/"):
		p = idInPath.ReplaceAllString(p, "/{id}$1")
		p = shaInPath.ReplaceAllString(p, "/{sha}$1")
		if strings.HasPrefix(p, "/api/v1/boards/") || strings.HasPrefix(p, "/api/v1/projects/") {
			parts := strings.Split(p, "/")
			if len(parts) > 4 {
				parts[4] = "{name}"
			}
			p = strings.Join(parts, "/")
		}
		return p
	case p == "/mcp", p == "/healthz", p == "/auth", p == "/metrics":
		return p
	}
	return "ui"
}

func (h *httpStats) observe(method, path string, code int, d time.Duration) {
	r := route(path)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == nil {
		h.count, h.buckets, h.sum, h.n = map[[3]string]uint64{}, map[[2]string][]uint64{}, map[[2]string]float64{}, map[[2]string]uint64{}
	}
	h.count[[3]string{method, r, fmt.Sprint(code)}]++
	k := [2]string{method, r}
	b := h.buckets[k]
	if b == nil {
		b = make([]uint64, len(latencyBuckets))
		h.buckets[k] = b
	}
	s := d.Seconds()
	for i, le := range latencyBuckets {
		if s <= le {
			b[i]++
		}
	}
	h.sum[k] += s
	h.n[k]++
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	s.writeMetrics(r.Context(), &b)
	io.WriteString(w, b.String())
}

func (s *Server) writeMetrics(ctx context.Context, b *strings.Builder) {
	h := &s.stats
	h.mu.Lock()
	help(b, "ka_http_requests_total", "counter", "HTTP requests by method, route and status code.")
	keys3 := make([][3]string, 0, len(h.count))
	for k := range h.count {
		keys3 = append(keys3, k)
	}
	sort.Slice(keys3, func(i, j int) bool { return strings.Join(keys3[i][:], " ") < strings.Join(keys3[j][:], " ") })
	for _, k := range keys3 {
		fmt.Fprintf(b, "ka_http_requests_total{method=%q,route=%q,code=%q} %d\n", k[0], k[1], k[2], h.count[k])
	}
	help(b, "ka_http_request_duration_seconds", "histogram", "HTTP request latency.")
	keys2 := make([][2]string, 0, len(h.n))
	for k := range h.n {
		keys2 = append(keys2, k)
	}
	sort.Slice(keys2, func(i, j int) bool { return keys2[i][0]+keys2[i][1] < keys2[j][0]+keys2[j][1] })
	for _, k := range keys2 {
		for i, le := range latencyBuckets {
			fmt.Fprintf(b, "ka_http_request_duration_seconds_bucket{method=%q,route=%q,le=\"%g\"} %d\n", k[0], k[1], le, h.buckets[k][i])
		}
		fmt.Fprintf(b, "ka_http_request_duration_seconds_bucket{method=%q,route=%q,le=\"+Inf\"} %d\n", k[0], k[1], h.n[k])
		fmt.Fprintf(b, "ka_http_request_duration_seconds_sum{method=%q,route=%q} %g\n", k[0], k[1], h.sum[k])
		fmt.Fprintf(b, "ka_http_request_duration_seconds_count{method=%q,route=%q} %d\n", k[0], k[1], h.n[k])
	}
	h.mu.Unlock()

	help(b, "ka_sse_clients", "gauge", "Connected event-stream clients.")
	fmt.Fprintf(b, "ka_sse_clients %d\n", h.sse.Load())
	if s.opts.Presence != nil {
		help(b, "ka_presence_sessions", "gauge", "Connected agent sessions.")
		fmt.Fprintf(b, "ka_presence_sessions %d\n", len(s.opts.Presence.List()))
	}
	help(b, "ka_event_seq", "counter", "The event stream's current sequence number.")
	fmt.Fprintf(b, "ka_event_seq %d\n", s.hub.Seq())

	ops, secs := s.svc.Ops().Snapshot()
	help(b, "ka_operations_total", "counter", "Ticket operations by kind and result (ok or an error code).")
	for _, o := range ops {
		fmt.Fprintf(b, "ka_operations_total{op=%q,result=%q} %d\n", o.Op, o.Result, o.N)
	}
	help(b, "ka_operation_seconds_total", "counter", "Time spent in ticket operations (commit and push).")
	opNames := make([]string, 0, len(secs))
	for k := range secs {
		opNames = append(opNames, k)
	}
	sort.Strings(opNames)
	for _, k := range opNames {
		fmt.Fprintf(b, "ka_operation_seconds_total{op=%q} %g\n", k, secs[k])
	}

	help(b, "ka_store_sync", "gauge", "1 for each store's current sync state.")
	help(b, "ka_store_ahead", "gauge", "Local commits not yet pushed.")
	for _, st := range s.svc.Stores(ctx) {
		fmt.Fprintf(b, "ka_store_sync{store=%q,state=%q} 1\n", st.Store, st.State)
		fmt.Fprintf(b, "ka_store_ahead{store=%q} %d\n", st.Store, st.Ahead)
	}
	help(b, "ka_tickets", "gauge", "Tickets by project and status category.")
	tc := s.svc.TicketCounts()
	tk := make([][2]string, 0, len(tc))
	for k := range tc {
		tk = append(tk, k)
	}
	sort.Slice(tk, func(i, j int) bool { return tk[i][0]+tk[i][1] < tk[j][0]+tk[j][1] })
	for _, k := range tk {
		fmt.Fprintf(b, "ka_tickets{project=%q,category=%q} %d\n", k[0], k[1], tc[k])
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	help(b, "go_goroutines", "gauge", "Goroutines.")
	fmt.Fprintf(b, "go_goroutines %d\n", runtime.NumGoroutine())
	help(b, "go_memstats_heap_alloc_bytes", "gauge", "Heap bytes in use.")
	fmt.Fprintf(b, "go_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	help(b, "process_start_time_seconds", "gauge", "When the daemon started.")
	fmt.Fprintf(b, "process_start_time_seconds %d\n", s.started.Unix())
}

func help(b *strings.Builder, name, typ, text string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ)
}
