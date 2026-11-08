package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/internal/api"
	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/mcpserver"
	"github.com/JumpKickOSS/ka/internal/presence"
)

func cmdDaemon(ctx context.Context, x *IO, args []string) int {
	sub := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "run":
		return daemonRun(ctx, x, args)
	case "status":
		return daemonStatus(ctx, x, args)
	case "stop":
		return daemonStop(ctx, x, args)
	case "openapi":
		return daemonOpenAPI(ctx, x, args)
	}
	fmt.Fprintln(x.Err, "usage: ka daemon [run [--listen ADDR] [-v]] | status | stop | openapi")
	return 2
}

func daemonRun(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("daemon", "[run] [--listen ADDR] [--public-url URL] [--trusted-proxy CIDR]... [-v]", x)
	listen := fl.String("listen", api.DefaultAddr, "address to listen on; a non-loopback address is remote mode and needs API tokens or --trusted-proxy")
	publicURL := fl.String("public-url", "", "remote mode: the URL clients use (https://ka.example.com); only its host is accepted")
	var proxies multi
	fl.Var(&proxies, "trusted-proxy", "remote mode: a reverse proxy (CIDR or IP) trusted to assert the user in --proxy-user-header (repeatable)")
	proxyHeader := fl.String("proxy-user-header", "X-Forwarded-User", "the header a trusted proxy sets to the user's handle or email")
	verbose := fl.Bool("v", false, "log every request")
	logFormat := fl.String("log-format", "text", "log format: text or json")
	hookSecretFile := fl.String("webhook-secret-file", "", "verify GitHub webhooks (POST /api/v1/hooks/github) with the secret in this file ($KA_WEBHOOK_SECRET also works)")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	hookSecret := os.Getenv("KA_WEBHOOK_SECRET")
	if *hookSecretFile != "" {
		b, err := os.ReadFile(*hookSecretFile)
		if err != nil {
			return fail(x, c, err)
		}
		hookSecret = strings.TrimSpace(string(b))
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	svc.Background = true
	if r := api.Discover(ctx, svc.WS.Source); r != nil {
		return fail(x, c, engine.Errf(engine.CodeInvalid, "use it, or stop it with `ka daemon stop`",
			"a daemon for this workspace is already running at %s (pid %d)", r.URL, r.PID))
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	var handler slog.Handler = slog.NewTextHandler(x.Err, &slog.HandlerOptions{Level: level})
	if *logFormat == "json" {
		handler = slog.NewJSONHandler(x.Err, &slog.HandlerOptions{Level: level})
	}
	log := slog.New(handler)
	var remote *api.Remote
	var ln net.Listener
	if !loopbackAddr(*listen) || *publicURL != "" || len(proxies) > 0 {
		cidrs, err := api.ParseCIDRs(proxies)
		if err != nil {
			return fail(x, c, err)
		}
		remote = &api.Remote{Tokens: api.NewTokenStore(api.TokensPath()), TrustedProxies: cidrs, ProxyUserHeader: *proxyHeader}
		if *publicURL != "" {
			u, err := url.Parse(*publicURL)
			if err != nil || u.Host == "" {
				return fail(x, c, engine.Errf(engine.CodeInvalid, "pass a URL such as https://ka.example.com", "--public-url %q is not a URL", *publicURL))
			}
			remote.Hosts = []string{u.Hostname(), u.Host}
		}
		if !remote.Configured() {
			return fail(x, c, engine.Errf(engine.CodeInvalid, "create a token with `ka token create --actor NAME` (stored hashed in "+api.TokensPath()+"), or pass --trusted-proxy",
				"refusing remote mode on %s without authentication", *listen))
		}
		if ln, err = api.ListenRemote(*listen); err != nil {
			return fail(x, c, err)
		}
		log.Info("remote mode", "tokens", remote.Tokens.Count(), "trusted_proxies", len(cidrs), "hosts", remote.Hosts)
	} else if ln, err = api.Listen(*listen); err != nil {
		return fail(x, c, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mcpserver.Version = Version
	hub := engine.NewEventHub(10000)
	svc.SetEvents(hub)
	pres := presence.New(hub)
	// Actors come from each client, not the daemon's environment.
	mcpSrv := mcpserver.New(svc, mcpserver.Options{Presence: pres})
	srv := api.New(svc, api.Options{Token: api.NewToken(), Version: Version, Logger: log, Shutdown: cancel,
		Events: hub, MCP: mcpSrv.HTTPHandler(), Presence: pres, Remote: remote, WebhookSecret: hookSecret})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pres.Sweep(presence.IdleTimeout)
			}
		}
	}()
	addr := ln.Addr().String()
	local := addr
	if h, p, err := net.SplitHostPort(addr); err == nil && net.ParseIP(h) != nil && net.ParseIP(h).IsUnspecified() {
		local = net.JoinHostPort("127.0.0.1", p) // local clients use loopback
	}
	rt := &api.Runtime{Version: Version, PID: os.Getpid(), Addr: addr, URL: "http://" + local, Workspace: svc.WS.Source,
		WorkspaceHash: api.WorkspaceHash(svc.WS.Source), Token: srv.Token(), Started: time.Now().UTC().Format(time.RFC3339)}
	if err := api.WriteRuntime(rt); err != nil {
		ln.Close()
		return fail(x, c, fmt.Errorf("writing the runtime file: %v", err))
	}
	defer api.RemoveRuntime(rt)
	go svc.WatchLocal(ctx, 250*time.Millisecond)
	go svc.FetchLoop(ctx)
	defer svc.Flush(context.Background()) // debounced pushes go out before exit
	log.Info("ka daemon listening", "url", rt.URL, "workspace", rt.Workspace, "runtime", api.RuntimePath(rt.WorkspaceHash))
	if err := srv.Serve(ctx, ln); err != nil {
		return fail(x, c, err)
	}
	log.Info("ka daemon stopped")
	return 0
}

// runningDaemon finds the daemon for the current workspace without building
// the index.
func runningDaemon(ctx context.Context, x *IO, c *common) (*api.Runtime, *engine.Workspace, error) {
	ws, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Workspace: c.workspace, Map: c.maps, Offline: true})
	if err != nil {
		return nil, nil, err
	}
	return api.Discover(ctx, ws.Source), ws, nil
}

func daemonStatus(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("daemon status", "", x)
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	r, ws, err := runningDaemon(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	if r == nil {
		if c.json {
			return emit(x, c, map[string]any{"running": false, "workspace": ws.Source}, "")
		}
		fmt.Fprintf(x.Out, "no daemon is running for %s\n", ws.Source)
		return 1
	}
	pub := *r
	pub.Token = "" // printed only from the runtime file itself
	return emit(x, c, map[string]any{"running": true, "daemon": pub, "runtime": api.RuntimePath(r.WorkspaceHash)},
		fmt.Sprintf("ka daemon %s at %s (pid %d, since %s)\nworkspace %s\nruntime   %s", r.Version, r.URL, r.PID, r.Started, r.Workspace, api.RuntimePath(r.WorkspaceHash)))
}

func daemonStop(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("daemon stop", "", x)
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	r, ws, err := runningDaemon(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	if r == nil {
		fmt.Fprintf(x.Out, "no daemon is running for %s\n", ws.Source)
		return 0
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/api/v1/daemon/shutdown", nil)
	req.Header.Set("Authorization", "Bearer "+r.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail(x, c, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fail(x, c, fmt.Errorf("shutdown: HTTP %d", resp.StatusCode))
	}
	for i := 0; i < 50; i++ {
		if !api.Probe(ctx, r) {
			fmt.Fprintf(x.Out, "stopped the daemon at %s (pid %d)\n", r.URL, r.PID)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fail(x, c, fmt.Errorf("the daemon at %s is still answering", r.URL))
}

func daemonOpenAPI(ctx context.Context, x *IO, args []string) int {
	fl, _ := newFlags("daemon openapi", "", x)
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	doc, err := json.MarshalIndent(api.New(nil, api.Options{Version: Version}).OpenAPI(), "", "  ")
	if err != nil {
		fmt.Fprintln(x.Err, "ka:", err)
		return 1
	}
	fmt.Fprintln(x.Out, string(doc))
	return 0
}

func loopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true // Listen reports the error
	}
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}

// ka token: API tokens for remote mode (PRD §15), stored hashed outside git.
func cmdToken(ctx context.Context, x *IO, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fl, c := newFlags("token "+sub, "create --actor NAME [--note TEXT] | ls | rm ID", x)
	actor := fl.String("actor", "", "who the token acts as (a human handle; agents using it act on that person's behalf)")
	note := fl.String("note", "", "what the token is for")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	ts := api.NewTokenStore(api.TokensPath())
	switch sub {
	case "create":
		tok, t, err := ts.Create(*actor, *note)
		if err != nil {
			return fail(x, c, err)
		}
		if c.json {
			return emit(x, c, map[string]any{"token": tok, "id": t.ID, "actor": t.Actor}, "")
		}
		fmt.Fprintf(x.Out, "%s\n", tok)
		fmt.Fprintf(x.Err, "token %s for %s created; it is shown only now (only its hash is kept in %s)\n", t.ID, t.Actor, ts.Path)
	case "ls", "":
		list := ts.List()
		var b strings.Builder
		for _, t := range list {
			fmt.Fprintf(&b, "%s  %-16s %s  %s\n", t.ID, t.Actor, t.Created, t.Note)
		}
		if len(list) == 0 {
			b.WriteString("no API tokens (ka token create --actor NAME)\n")
		}
		return emit(x, c, list, strings.TrimRight(b.String(), "\n"))
	case "rm":
		if len(pos) != 1 {
			return usageErr(x, fl, "token rm needs a token ID (ka token ls)")
		}
		if err := ts.Remove(pos[0]); err != nil {
			return fail(x, c, err)
		}
		fmt.Fprintf(x.Out, "removed token %s\n", pos[0])
	default:
		fl.Usage()
		return 2
	}
	return 0
}
