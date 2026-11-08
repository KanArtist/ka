package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/internal/api"
	"github.com/JumpKickOSS/ka/internal/engine"
)

// cmdOpen opens the web UI (PRD §13.3): it starts the workspace's daemon if
// none is running, then sends the browser to /auth, which trades the local
// token for an HttpOnly cookie and redirects to the view.
func cmdOpen(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("open", "[ID] [--print]", x)
	printOnly := fl.Bool("print", false, "print the URL instead of opening a browser")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) > 1 {
		return usageErr(x, fl, "open takes at most one ticket ID")
	}
	rt, ws, err := runningDaemon(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	if rt == nil {
		if rt, err = startDaemon(ctx, x, c, ws); err != nil {
			return fail(x, c, err)
		}
		fmt.Fprintf(x.Err, "started ka daemon at %s (pid %d; stop it with `ka daemon stop`)\n", rt.URL, rt.PID)
	}
	next := "/"
	if len(pos) == 1 {
		next = "/ticket/" + url.PathEscape(strings.ToUpper(pos[0]))
	}
	u := rt.URL + "/auth?" + url.Values{"token": {rt.Token}, "next": {next}}.Encode()
	if *printOnly {
		fmt.Fprintln(x.Out, u)
		return 0
	}
	if err := openBrowser(u); err != nil {
		fmt.Fprintf(x.Err, "ka: could not open a browser (%v); open this URL:\n", err)
		fmt.Fprintln(x.Out, u)
		return 0
	}
	fmt.Fprintf(x.Out, "opened %s%s\n", rt.URL, next)
	return 0
}

// startDaemon runs `ka daemon` in the background for the workspace and
// waits for it to answer.
func startDaemon(ctx context.Context, x *IO, c *common, ws *engine.Workspace) (*api.Runtime, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"daemon"}
	if c.workspace != "" {
		args = append(args, "--workspace", c.workspace)
	}
	for k, v := range c.maps {
		args = append(args, "--map", k+"="+v)
	}
	if err := os.MkdirAll(api.RuntimeDir(), 0o700); err != nil {
		return nil, err
	}
	logPath := filepath.Join(api.RuntimeDir(), "daemon-"+api.WorkspaceHash(ws.Source)+".log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, args...)
	cmd.Dir = x.Dir
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ka daemon: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if rt := api.Discover(ctx, ws.Source); rt != nil {
			cmd.Process.Release()
			return rt, nil
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("ka daemon exited (%v); see %s", err, logPath)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("ka daemon did not start within 15 s; see %s", logPath)
}

// openBrowser opens a URL in the user's browser.
func openBrowser(u string) error {
	if b := os.Getenv("BROWSER"); b != "" {
		return exec.Command(b, u).Start()
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		if isWSL() {
			if p, err := exec.LookPath("wslview"); err == nil {
				cmd = exec.Command(p, u)
				break
			}
			cmd = exec.Command("cmd.exe", "/c", "start", "", strings.ReplaceAll(u, "&", "^&"))
			break
		}
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

func isWSL() bool {
	b, _ := os.ReadFile("/proc/version")
	return strings.Contains(strings.ToLower(string(b)), "microsoft")
}
