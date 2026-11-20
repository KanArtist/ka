// Package setup registers ka with the coding agents installed on this
// machine: the ka MCP server and the ka skill, at the user level, so every
// project the user opens gets them. `ka setup` runs it; it is safe to run
// again (it replaces ka's own entries and touches nothing else).
//
// Each agent is configured through its own CLI where the vendor documents
// an add command, and otherwise by merging into its JSON config file. The
// formats follow the vendors' documentation as of 2026-11.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Result is what happened for one agent or step.
type Result struct {
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Status string `json:"status"` // configured, skipped, failed, planned
	Detail string `json:"detail,omitempty"`
}

// Options control a run.
type Options struct {
	// Ka is the command agents run (an absolute path, so GUI apps that don't
	// read the shell's PATH still find it).
	Ka string
	// Only limits setup to these agent IDs (empty: every detected agent).
	Only []string
	// DryRun reports what would change without changing it.
	DryRun bool
	// Home overrides the home directory (tests).
	Home string
	// LookPath overrides exec.LookPath (tests).
	LookPath func(string) (string, error)
	// Run overrides running an agent's CLI (tests).
	Run func(ctx context.Context, name string, args ...string) error
}

func (o *Options) home() string {
	if o.Home != "" {
		return o.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

func (o *Options) lookPath(name string) bool {
	lp := o.LookPath
	if lp == nil {
		lp = exec.LookPath
	}
	_, err := lp(name)
	return err == nil
}

func (o *Options) run(ctx context.Context, name string, args ...string) error {
	if o.Run != nil {
		return o.Run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// agent describes how to detect and configure one coding agent.
type agent struct {
	id, name string
	bins     []string // any of these on PATH means it is installed
	dirs     []string // or any of these config dirs (relative to home) exists
	// cli is the binary mcp runs, for agents configured through their CLI;
	// without it on PATH the agent is skipped.
	cli string
	// skills are extra user skill dirs (relative to home) for agents that
	// don't read ~/.agents/skills.
	skills []string
	mcp    func(ctx context.Context, o *Options, actor string) (string, error)
}

// Agents lists the agents setup knows, by ID.
func Agents() []string {
	var ids []string
	for _, a := range agents() {
		ids = append(ids, a.id)
	}
	return ids
}

func agents() []agent {
	return []agent{
		{id: "claude", name: "Claude Code", bins: []string{"claude"}, dirs: []string{".claude"}, skills: []string{".claude/skills"}, cli: "claude",
			// An option between -e and the name, or the name parses as another env pair.
			mcp: cliMCP("claude", "claude mcp add -s user", []string{"mcp", "remove", "-s", "user", "ka"},
				func(o *Options, actor string) []string {
					return []string{"mcp", "add", "-s", "user", "-e", "KA_ACTOR=" + actor, "--transport", "stdio", "ka", "--", o.Ka, "mcp"}
				})},
		{id: "codex", name: "Codex", bins: []string{"codex"}, dirs: []string{".codex"}, cli: "codex",
			mcp: cliMCP("codex", "codex mcp add", []string{"mcp", "remove", "ka"},
				func(o *Options, actor string) []string {
					return []string{"mcp", "add", "ka", "--env", "KA_ACTOR=" + actor, "--", o.Ka, "mcp"}
				})},
		{id: "cursor", name: "Cursor", bins: []string{"cursor-agent", "cursor"}, dirs: []string{".cursor"},
			mcp: jsonMCP(func(o *Options) string { return filepath.Join(o.home(), ".cursor", "mcp.json") },
				"mcpServers", map[string]any{"type": "stdio"})},
		{id: "agy", name: "Antigravity", bins: []string{"agy"}, dirs: []string{".gemini/config"}, skills: []string{".gemini/config/skills"},
			mcp: jsonMCP(func(o *Options) string { return filepath.Join(o.home(), ".gemini", "config", "mcp_config.json") },
				"mcpServers", nil)},
		{id: "copilot", name: "GitHub Copilot CLI", bins: []string{"copilot"}, dirs: []string{".copilot"},
			mcp: jsonMCP(func(o *Options) string {
				dir := os.Getenv("COPILOT_HOME")
				if dir == "" {
					dir = filepath.Join(o.home(), ".copilot")
				}
				return filepath.Join(dir, "mcp-config.json")
			}, "mcpServers", map[string]any{"type": "local", "tools": []string{"*"}})},
		{id: "vscode", name: "VS Code (Copilot)", bins: []string{"code"}, cli: "code",
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				cfg, _ := json.Marshal(map[string]any{"name": "ka", "type": "stdio", "command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
				return "code --add-mcp", o.run(ctx, "code", "--add-mcp", string(cfg))
			}},
		{id: "grok", name: "Grok Build", bins: []string{"grok"}, dirs: []string{".grok"}, cli: "grok",
			mcp: cliMCP("grok", "grok mcp add", []string{"mcp", "remove", "ka"},
				func(o *Options, actor string) []string {
					return []string{"mcp", "add", "ka", "-e", "KA_ACTOR=" + actor, "--", o.Ka, "mcp"}
				})},
		{id: "muse", name: "Muse Code", bins: []string{"muse"}, dirs: []string{".config/muse"},
			// optional: a failing server must not abort the agent's run.
			mcp: jsonMCP(func(o *Options) string { return filepath.Join(xdgConfig(o), "muse", "settings.json") },
				"mcp_servers", map[string]any{"transport": "stdio", "mode": "optional"}, jsonDefault{"schema_version", 1})},
		{id: "amp", name: "Amp", bins: []string{"amp"}, dirs: []string{".config/amp"},
			mcp: jsonMCP(func(o *Options) string { return filepath.Join(xdgConfig(o), "amp", "settings.json") },
				"amp.mcpServers", nil)},
		{id: "devin", name: "Devin", bins: []string{"devin"}, dirs: []string{".config/devin"},
			mcp: jsonMCP(func(o *Options) string {
				dir := filepath.Join(xdgConfig(o), "devin")
				if runtime.GOOS == "windows" && os.Getenv("APPDATA") != "" && o.Home == "" {
					dir = filepath.Join(os.Getenv("APPDATA"), "devin")
				}
				return filepath.Join(dir, "mcp_config.json")
			}, "mcpServers", nil)},
		{id: "junie", name: "Junie", bins: []string{"junie"}, dirs: []string{".junie"},
			mcp: jsonMCP(func(o *Options) string { return filepath.Join(o.home(), ".junie", "mcp", "mcp.json") },
				"mcpServers", nil)},
	}
}

// cliMCP configures an agent through its own CLI. It adds first and only
// removes ka's old entry (then adds again) when that fails, so a failing add
// does not lose a working entry.
func cliMCP(bin, where string, remove []string, add func(o *Options, actor string) []string) func(context.Context, *Options, string) (string, error) {
	return func(ctx context.Context, o *Options, actor string) (string, error) {
		if err := o.run(ctx, bin, add(o, actor)...); err == nil {
			return where, nil
		}
		_ = o.run(ctx, bin, remove...)
		return where, o.run(ctx, bin, add(o, actor)...)
	}
}

// jsonMCP configures an agent by merging the ka server, with extra fields,
// under key in the JSON file at path.
func jsonMCP(path func(o *Options) string, key string, extra map[string]any, defaults ...jsonDefault) func(context.Context, *Options, string) (string, error) {
	return func(ctx context.Context, o *Options, actor string) (string, error) {
		v := map[string]any{"command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}}
		maps.Copy(v, extra)
		p := path(o)
		return p, mergeJSON(p, o.DryRun, []string{key, "ka"}, v, defaults...)
	}
}

func xdgConfig(o *Options) string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" && o.Home == "" {
		return d
	}
	return filepath.Join(o.home(), ".config")
}

func (a agent) detected(o *Options) bool {
	for _, b := range a.bins {
		if o.lookPath(b) {
			return true
		}
	}
	for _, d := range a.dirs {
		if _, err := os.Stat(filepath.Join(o.home(), filepath.FromSlash(d))); err == nil {
			return true
		}
	}
	return false
}

// Run configures every detected agent (or those in o.Only) and installs the
// skill. skill is the SKILL.md content.
func Run(ctx context.Context, o Options, skill string) ([]Result, error) {
	only := map[string]bool{}
	for _, id := range o.Only {
		only[id] = true
	}
	known := map[string]bool{}
	for _, a := range agents() {
		known[a.id] = true
	}
	for id := range only {
		if !known[id] {
			return nil, fmt.Errorf("unknown agent %q (known: %s)", id, strings.Join(Agents(), ", "))
		}
	}
	var res []Result
	skillDirs := map[string]bool{filepath.Join(o.home(), ".agents", "skills"): true}
	for _, a := range agents() {
		if len(only) > 0 && !only[a.id] {
			continue
		}
		if len(only) == 0 && !a.detected(&o) {
			continue
		}
		for _, d := range a.skills {
			skillDirs[filepath.Join(o.home(), filepath.FromSlash(d))] = true
		}
		actor := "agent/" + a.id
		if a.cli != "" && !o.lookPath(a.cli) {
			// Found by its config dir only: there is no CLI to configure it with.
			status := "skipped"
			if only[a.id] {
				status = "failed"
			}
			res = append(res, Result{a.id, a.name, status, a.cli + " is not on PATH; install it or add it to PATH, then run ka setup again"})
			continue
		}
		if o.DryRun {
			res = append(res, Result{a.id, a.name, "planned", "register the ka MCP server as " + actor})
			continue
		}
		where, err := a.mcp(ctx, &o, actor)
		if err != nil {
			res = append(res, Result{a.id, a.name, "failed", where + ": " + err.Error()})
			continue
		}
		res = append(res, Result{a.id, a.name, "configured", "MCP server ka (" + actor + ") via " + where})
	}
	var dirs []string
	for d := range skillDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		p := filepath.Join(d, "ka", "SKILL.md")
		if o.DryRun {
			res = append(res, Result{"skill", "ka skill", "planned", "write " + p})
			continue
		}
		if err := writeFile(p, []byte(skill)); err != nil {
			res = append(res, Result{"skill", "ka skill", "failed", err.Error()})
			continue
		}
		res = append(res, Result{"skill", "ka skill", "configured", p})
	}
	return res, nil
}

type jsonDefault struct {
	key   string
	value any
}

// mergeJSON sets doc[path...] = value in the JSON file at p, keeping every
// other key; a missing file starts empty. defaults are set at the top level
// when absent.
func mergeJSON(p string, dryRun bool, path []string, value any, defaults ...jsonDefault) error {
	doc := map[string]any{}
	b, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case len(bytes.TrimSpace(b)) > 0:
		// UseNumber keeps integers above 2^53 as written.
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return fmt.Errorf("%s is not plain JSON (%v); add the ka server by hand", p, err)
		}
		if dec.More() {
			return fmt.Errorf("%s has more than one JSON value; add the ka server by hand", p)
		}
	}
	for _, d := range defaults {
		if _, ok := doc[d.key]; !ok {
			doc[d.key] = d.value
		}
	}
	m := doc
	for _, k := range path[:len(path)-1] {
		switch next := m[k].(type) {
		case map[string]any:
			m = next
		case nil:
			n := map[string]any{}
			m[k] = n
			m = n
		default:
			return fmt.Errorf("%s: %q is not an object; add the ka server by hand", p, k)
		}
	}
	m[path[len(path)-1]] = value
	if dryRun {
		return nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(p, append(out, '\n'))
}

// writeFile writes atomically, creating parent directories. A symlinked file
// (stow, chezmoi) is written through: its target is replaced, not the link.
func writeFile(p string, data []byte) error {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(p); err == nil {
		mode = fi.Mode().Perm() // keep a private config private
	}
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".ka-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
