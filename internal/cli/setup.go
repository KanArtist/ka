package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	karepo "github.com/KanArtist/ka"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/gitx"
	"github.com/KanArtist/ka/internal/setup"
	"github.com/KanArtist/ka/internal/uisrc"
)

// cmdSetup gets this machine ready: git, the web UI's source, and ka's MCP
// server and skill registered with every coding agent installed.
func cmdSetup(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("setup", "[--agents LIST] [--dry-run] [--checkout DIR]", x)
	only := fl.String("agents", "", "configure only these agents, comma-separated ("+strings.Join(setup.Agents(), ", ")+"); default: every agent found")
	dryRun := fl.Bool("dry-run", false, "show what would change")
	checkout := fl.String("checkout", "", "the ka checkout to serve the web UI from (default: the one containing this directory, if any)")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	var res []setup.Result
	add := func(id, name, status, detail string) {
		res = append(res, setup.Result{Agent: id, Name: name, Status: status, Detail: detail})
	}

	maj, min, raw, err := gitx.Version(ctx)
	switch {
	case err != nil:
		add("git", "git", "failed", "git not found: install git 2.40 or later")
	case maj < gitx.MinVersion[0] || (maj == gitx.MinVersion[0] && min < gitx.MinVersion[1]):
		add("git", "git", "failed", raw+" is too old; ka needs 2.40 or later")
	default:
		add("git", "git", "ok", raw)
	}

	// The web UI: serve this checkout's ui/ (the daemon re-clones it if it goes away).
	dir := *checkout
	if dir == "" {
		dir = x.Dir
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(x.Dir, dir)
	}
	if root, ok := uisrc.FindCheckout(dir); ok {
		if *dryRun {
			add("ui", "web UI", "planned", "serve "+filepath.Join(root, "ui"))
		} else {
			cfg, _ := uisrc.Load()
			cfg.Checkout = root
			if cfg.URL == "" {
				cfg.URL = uisrc.DefaultURL
			}
			if err := uisrc.Save(cfg); err != nil {
				add("ui", "web UI", "failed", err.Error())
			} else if err := uisrc.Check(filepath.Join(root, "ui")); err != nil {
				add("ui", "web UI", "ok", "recorded "+root+"; until it matches this binary the embedded UI serves ("+err.Error()+")")
			} else {
				add("ui", "web UI", "configured", "served from "+filepath.Join(root, "ui"))
			}
		}
	} else if *checkout != "" {
		add("ui", "web UI", "failed", *checkout+" is not a checkout of github.com/KanArtist/ka")
	} else {
		add("ui", "web UI", "ok", "embedded in this binary (run ka setup in a ka checkout to serve its ui/)")
	}

	o := setup.Options{Ka: kaPath(), DryRun: *dryRun}
	if *only != "" {
		for _, id := range strings.Split(*only, ",") {
			if id = strings.TrimSpace(id); id != "" {
				o.Only = append(o.Only, id)
			}
		}
	}
	agentRes, err := setup.Run(ctx, o, karepo.Skill)
	if err != nil {
		return fail(x, c, engine.Errf(engine.CodeInvalid, "", "%v", err))
	}
	res = append(res, agentRes...)
	found := 0
	for _, r := range agentRes {
		if r.Agent != "skill" {
			found++
		}
	}

	stores := 0
	if ws, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Workspace: c.workspace, Offline: true}); err == nil {
		stores = len(ws.Stores)
	} else if c.workspace == "" {
		// An empty workspace, so `ka mcp` starts (and agents connect) before
		// any store is added.
		p := filepath.Join(engine.ConfigDir(), "workspace.yml")
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if *dryRun {
				add("workspace", "workspace", "planned", "create "+p)
			} else if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil && os.WriteFile(p, []byte("stores: []\n"), 0o644) == nil {
				add("workspace", "workspace", "configured", "created "+p)
			}
		}
	}

	if c.json {
		enc := json.NewEncoder(x.Out)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"results": res, "agents_found": found, "stores": stores})
	} else {
		for _, r := range res {
			fmt.Fprintf(x.Out, "%-10s  %-20s  %s\n", r.Status, r.Name, r.Detail)
		}
		if found == 0 {
			fmt.Fprintln(x.Out, "\nNo coding agents found. Install one, or name it with --agents ("+strings.Join(setup.Agents(), ", ")+").")
		} else if !*dryRun {
			fmt.Fprintln(x.Out, "\nRestart running agent sessions to load the ka MCP server and skill.")
		}
		if stores == 0 {
			fmt.Fprintln(x.Out, "\nNo ticket stores yet. Add the repositories that hold your tickets:\n  ka store add git@github.com:ORG/REPO.git\nor start a new one: ka init --project tickets DIR")
		} else {
			fmt.Fprintf(x.Out, "\n%d ticket store(s) in your workspace. `ka open` shows the board.\n", stores)
		}
	}
	for _, r := range res {
		if r.Status == "failed" {
			return 1
		}
	}
	return 0
}

// kaPath is the ka agents should run: the one on PATH, else this binary.
func kaPath() string {
	if p, err := exec.LookPath("ka"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "ka"
}

// cmdSkill prints the agent skill (the same text ka setup installs).
func cmdSkill(ctx context.Context, x *IO, args []string) int {
	fl, _ := newFlags("skill", "", x)
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	fmt.Fprint(x.Out, karepo.Skill)
	return 0
}
