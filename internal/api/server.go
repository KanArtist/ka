// Package api is the daemon's HTTP surface (PRD §12, §15): REST under
// /api/v1 with an OpenAPI 3.1 document, the token exchange for browsers, and
// the Host and Origin checks that keep other web pages out.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/presence"
)

// DefaultAddr is where the daemon listens unless told otherwise.
const DefaultAddr = "127.0.0.1:7711"

// CookieName holds the local token in browsers.
const CookieName = "ka_token"

// Options configures a Server.
type Options struct {
	Token   string
	Version string
	Logger  *slog.Logger
	// Shutdown is called by POST /api/v1/daemon/shutdown.
	Shutdown func()
	// Events is the hub SSE clients read; New creates one (10,000 events)
	// and connects it to the service when nil.
	Events *engine.EventHub
	// Heartbeat is the SSE keep-alive interval (default 15 s).
	Heartbeat time.Duration
	// MCP, when set, is mounted at /mcp (MCP over Streamable HTTP).
	MCP http.Handler
	// Presence lists live agent sessions (GET /api/v1/presence).
	Presence *presence.Tracker
	// Remote enables remote mode: API tokens and trusted proxies (PRD §15).
	Remote *Remote
}

// Server serves one workspace's service over HTTP.
type Server struct {
	svc       *engine.Service
	opts      Options
	api       huma.API
	mux       *http.ServeMux
	log       *slog.Logger
	started   time.Time
	hub       *engine.EventHub
	heartbeat time.Duration

	mu   sync.RWMutex
	port int // the bound port, for Host and Origin checks
	addr string
}

// New builds the server and its routes. Call SetAddr once the listener is
// bound (Serve does it).
func New(svc *engine.Service, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Events == nil {
		opts.Events = engine.NewEventHub(10000)
		if svc != nil {
			svc.SetEvents(opts.Events)
		}
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 15 * time.Second
	}
	s := &Server{svc: svc, opts: opts, mux: http.NewServeMux(), log: opts.Logger, started: time.Now(), hub: opts.Events, heartbeat: opts.Heartbeat}
	s.api = humago.New(s.mux, Config(opts.Version))
	s.routes()
	s.documentEvents()
	s.mux.HandleFunc("GET /api/v1/events", s.events)
	if opts.MCP != nil {
		s.mux.Handle("/mcp", opts.MCP)
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /auth", s.auth)
	return s
}

// Config is the Huma configuration: OpenAPI 3.1 at /api/v1/openapi.json,
// no hosted docs page (it would load scripts from a CDN), no $schema links.
func Config(version string) huma.Config {
	c := huma.DefaultConfig("KanArtist", version)
	c.Info.Description = "The ka daemon's REST API. It is the same service layer as the MCP tools and the CLI; errors are problem+json with the same codes."
	c.OpenAPIPath = "/api/v1/openapi"
	c.DocsPath = ""
	c.SchemasPath = ""
	c.CreateHooks = nil
	c.Components.Schemas = huma.NewMapRegistry("#/components/schemas/", schemaNamer)
	c.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"token":  {Type: "http", Scheme: "bearer", Description: "The local token from the daemon's runtime file."},
		"cookie": {Type: "apiKey", In: "cookie", Name: CookieName, Description: "Set by GET /auth?token=… (ka open)."},
	}
	c.Security = []map[string][]string{{"token": {}}, {"cookie": {}}}
	return c
}

// schemaNamer prefixes spec-library types so they cannot collide with the
// engine's (kaspec.Claim and engine.Claim, for example).
func schemaNamer(t reflect.Type, hint string) string {
	name := huma.DefaultSchemaNamer(t, hint)
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if strings.HasSuffix(t.PkgPath(), "/kaspec") {
		return "Spec" + name
	}
	return name
}

// Token is the local token clients must present.
func (s *Server) Token() string { return s.opts.Token }

// OpenAPI returns the API's OpenAPI document.
func (s *Server) OpenAPI() *huma.OpenAPI { return s.api.OpenAPI() }

// SetAddr records the bound address.
func (s *Server) SetAddr(addr string) {
	_, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	s.mu.Lock()
	s.addr, s.port = addr, port
	s.mu.Unlock()
}

func (s *Server) boundPort() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.port
}

// Handler is the full HTTP handler with the security checks in front.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		s.serve(rw, r)
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	r.Header.Del(PrincipalHeader) // only the server sets it
	if !s.hostAllowed(r.Host) {
		writeProblem(w, newProblem(codeForbidden, "connect to 127.0.0.1 or localhost (or the daemon's --public-url)", "Host "+strconv.Quote(r.Host)+" is not allowed"))
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !s.originAllowed(o, r.Host) {
		writeProblem(w, newProblem(codeForbidden, "", "cross-origin requests are not allowed (Origin "+strconv.Quote(o)+")"))
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeProblem(w, newProblem(codeForbidden, "", "cross-site requests are not allowed"))
		return
	}
	if isUIPath(r.URL.Path) {
		s.serveUI(w, r)
		return
	}
	switch r.URL.Path {
	case "/healthz", "/auth":
	default:
		principal, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ka"`)
			fix := "send Authorization: Bearer <token> with the token from the daemon's runtime file, or open the UI with `ka open`"
			if s.opts.Remote != nil {
				fix = "send Authorization: Bearer <API token> (ka token create on the daemon's machine), or sign in through the proxy"
			}
			writeProblem(w, newProblem(codeUnauthorized, fix, "missing or wrong token"))
			return
		}
		if principal != "" {
			r.Header.Set(PrincipalHeader, principal)
		}
	}
	w.Header().Set(SeqHeader, strconv.FormatUint(s.hub.Seq(), 10))
	s.mux.ServeHTTP(w, r)
}

func (s *Server) hostAllowed(host string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		h, p = host, ""
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	if rm := s.opts.Remote; rm != nil {
		if len(rm.Hosts) == 0 {
			return true // any Host; Origin must still match it
		}
		for _, a := range rm.Hosts {
			if strings.EqualFold(a, h) || strings.EqualFold(a, host) {
				return true
			}
		}
	}
	if port, _ := strconv.Atoi(p); port != s.boundPort() {
		return false
	}
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func (s *Server) originAllowed(origin, reqHost string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Path != "" && u.Path != "/" {
		return false
	}
	if s.opts.Remote != nil {
		return (u.Scheme == "https" || u.Scheme == "http") && strings.EqualFold(u.Host, reqHost) && s.hostAllowed(u.Host)
	}
	return u.Scheme == "http" && s.hostAllowed(u.Host)
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if t, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(t)
		}
		return ""
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return c.Value
	}
	return ""
}

// authenticate checks a request's credentials. principal is the
// authenticated actor in remote mode ("" for the local token, whose
// requests act as the daemon's human).
func (s *Server) authenticate(r *http.Request) (principal string, ok bool) {
	tok := bearerToken(r)
	if s.tokenOK(tok) {
		return "", true
	}
	rm := s.opts.Remote
	if rm == nil {
		return "", false
	}
	if a, ok := rm.Tokens.Lookup(tok); ok {
		return a, true
	}
	if len(rm.TrustedProxies) > 0 && rm.fromTrustedProxy(r) {
		hdr := rm.ProxyUserHeader
		if hdr == "" {
			hdr = "X-Forwarded-User"
		}
		if u := strings.TrimSpace(r.Header.Get(hdr)); u != "" {
			return u, true
		}
	}
	return "", false
}

func (s *Server) tokenOK(tok string) bool {
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) == 1
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Health{Status: "ok", Version: s.opts.Version, PID: os.Getpid(), WorkspaceHash: WorkspaceHash(s.svc.WS.Source)})
}

// auth exchanges the local token for an HttpOnly cookie and redirects to
// next (a path on this server). ka open sends the browser here.
func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	valid := s.tokenOK(tok)
	if !valid && s.opts.Remote != nil {
		_, valid = s.opts.Remote.Tokens.Lookup(tok)
	}
	if !valid {
		writeProblem(w, newProblem(codeUnauthorized, "run `ka open` again", "wrong or missing token"))
		return
	}
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	secure := r.TLS != nil || (s.opts.Remote != nil && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: tok, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func writeProblem(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(p)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush keeps streaming responses (SSE) working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Listen binds addr. Only loopback addresses are accepted until remote mode
// (M4). When addr is the default and its port is taken, a free port is used;
// the runtime file tells clients which.
func Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: %v", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, engine.Errf(engine.CodeInvalid, "use 127.0.0.1:PORT; remote mode (with API tokens) comes in M4", "refusing to listen on %s: only loopback addresses are allowed", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil && addr == DefaultAddr && isAddrInUse(err) {
		return net.Listen("tcp", "127.0.0.1:0")
	}
	return ln, err
}

// ListenRemote listens on any address, for remote mode. The caller must
// have configured authentication (Remote.Configured).
func ListenRemote(addr string) (net.Listener, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("--listen %q: %v", addr, err)
	}
	return net.Listen("tcp", addr)
}

func isAddrInUse(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && strings.Contains(strings.ToLower(op.Err.Error()), "in use") ||
		strings.Contains(strings.ToLower(err.Error()), "only one usage")
}

// Serve runs the server on ln until ctx is canceled, then shuts down
// gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.SetAddr(ln.Addr().String())
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
