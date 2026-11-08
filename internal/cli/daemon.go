package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/JumpKickOSS/ka/internal/api"
	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/mcpserver"
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
	fl, c := newFlags("daemon", "[run] [--listen ADDR] [-v]", x)
	listen := fl.String("listen", api.DefaultAddr, "address to listen on (loopback only)")
	verbose := fl.Bool("v", false, "log every request")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	if r := api.Discover(ctx, svc.WS.Source); r != nil {
		return fail(x, c, engine.Errf(engine.CodeInvalid, "use it, or stop it with `ka daemon stop`",
			"a daemon for this workspace is already running at %s (pid %d)", r.URL, r.PID))
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(x.Err, &slog.HandlerOptions{Level: level}))
	ln, err := api.Listen(*listen)
	if err != nil {
		return fail(x, c, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mcpserver.Version = Version
	mcpSrv := mcpserver.New(svc, mcpserver.Options{}) // actors come from each client, not the daemon's environment
	srv := api.New(svc, api.Options{Token: api.NewToken(), Version: Version, Logger: log, Shutdown: cancel, MCP: mcpSrv.HTTPHandler()})
	addr := ln.Addr().String()
	rt := &api.Runtime{Version: Version, PID: os.Getpid(), Addr: addr, URL: "http://" + addr, Workspace: svc.WS.Source,
		WorkspaceHash: api.WorkspaceHash(svc.WS.Source), Token: srv.Token(), Started: time.Now().UTC().Format(time.RFC3339)}
	if err := api.WriteRuntime(rt); err != nil {
		ln.Close()
		return fail(x, c, fmt.Errorf("writing the runtime file: %v", err))
	}
	defer api.RemoveRuntime(rt)
	go svc.WatchLocal(ctx, 250*time.Millisecond)
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
