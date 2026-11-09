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
		{id: "claude", name: "Claude Code", bins: []string{"claude"}, dirs: []string{".claude"}, skills: []string{".claude/skills"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				_ = o.run(ctx, "claude", "mcp", "remove", "-s", "user", "ka")
				// An option between -e and the name, or the name parses as another env pair.
				return "claude mcp add -s user", o.run(ctx, "claude", "mcp", "add", "-s", "user", "-e", "KA_ACTOR="+actor, "--transport", "stdio", "ka", "--", o.Ka, "mcp")
			}},
		{id: "codex", name: "Codex", bins: []string{"codex"}, dirs: []string{".codex"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				_ = o.run(ctx, "codex", "mcp", "remove", "ka")
				return "codex mcp add", o.run(ctx, "codex", "mcp", "add", "ka", "--env", "KA_ACTOR="+actor, "--", o.Ka, "mcp")
			}},
		{id: "cursor", name: "Cursor", bins: []string{"cursor-agent", "cursor"}, dirs: []string{".cursor"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				p := filepath.Join(o.home(), ".cursor", "mcp.json")
				return p, mergeJSON(p, o.DryRun, []string{"mcpServers", "ka"}, map[string]any{"type": "stdio", "command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
			}},
		{id: "gemini", name: "Gemini CLI", bins: []string{"gemini"}, dirs: []string{".gemini/settings.json"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				p := filepath.Join(o.home(), ".gemini", "settings.json")
				return p, mergeJSON(p, o.DryRun, []string{"mcpServers", "ka"}, map[string]any{"command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
			}},
		{id: "agy", name: "Antigravity", bins: []string{"agy"}, dirs: []string{".gemini/config"}, skills: []string{".gemini/config/skills"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				p := filepath.Join(o.home(), ".gemini", "config", "mcp_config.json")
				return p, mergeJSON(p, o.DryRun, []string{"mcpServers", "ka"}, map[string]any{"command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
			}},
		{id: "copilot", name: "GitHub Copilot CLI", bins: []string{"copilot"}, dirs: []string{".copilot"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				dir := os.Getenv("COPILOT_HOME")
				if dir == "" {
					dir = filepath.Join(o.home(), ".copilot")
				}
				p := filepath.Join(dir, "mcp-config.json")
				return p, mergeJSON(p, o.DryRun, []string{"mcpServers", "ka"}, map[string]any{"type": "local", "command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}, "tools": []string{"*"}})
			}},
		{id: "vscode", name: "VS Code (Copilot)", bins: []string{"code"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				cfg, _ := json.Marshal(map[string]any{"name": "ka", "type": "stdio", "command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
				return "code --add-mcp", o.run(ctx, "code", "--add-mcp", string(cfg))
			}},
		{id: "grok", name: "Grok Build", bins: []string{"grok"}, dirs: []string{".grok"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				_ = o.run(ctx, "grok", "mcp", "remove", "ka")
				return "grok mcp add", o.run(ctx, "grok", "mcp", "add", "ka", "-e", "KA_ACTOR="+actor, "--", o.Ka, "mcp")
			}},
		{id: "muse", name: "Muse Code", bins: []string{"muse"}, dirs: []string{".config/muse"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				p := filepath.Join(xdgConfig(o), "muse", "settings.json")
				// optional: a failing server must not abort the agent's run.
				return p, mergeJSON(p, o.DryRun, []string{"mcp_servers", "ka"}, map[string]any{"transport": "stdio", "command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}, "mode": "optional"},
					jsonDefault{"schema_version", 1})
			}},
		{id: "amp", name: "Amp", bins: []string{"amp"}, dirs: []string{".config/amp"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				p := filepath.Join(xdgConfig(o), "amp", "settings.json")
				return p, mergeJSON(p, o.DryRun, []string{"amp.mcpServers", "ka"}, map[string]any{"command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
			}},
		{id: "devin", name: "Devin", bins: []string{"devin"}, dirs: []string{".config/devin"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				dir := filepath.Join(xdgConfig(o), "devin")
				if runtime.GOOS == "windows" && os.Getenv("APPDATA") != "" && o.Home == "" {
					dir = filepath.Join(os.Getenv("APPDATA"), "devin")
				}
				p := filepath.Join(dir, "mcp_config.json")
				return p, mergeJSON(p, o.DryRun, []string{"mcpServers", "ka"}, map[string]any{"command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
			}},
		{id: "junie", name: "Junie", bins: []string{"junie"}, dirs: []string{".junie"},
			mcp: func(ctx context.Context, o *Options, actor string) (string, error) {
				p := filepath.Join(o.home(), ".junie", "mcp", "mcp.json")
				return p, mergeJSON(p, o.DryRun, []string{"mcpServers", "ka"}, map[string]any{"command": o.Ka, "args": []string{"mcp"}, "env": map[string]string{"KA_ACTOR": actor}})
			}},
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
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s is not plain JSON (%v); add the ka server by hand", p, err)
		}
	}
	for _, d := range defaults {
		if _, ok := doc[d.key]; !ok {
			doc[d.key] = d.value
		}
	}
	m := doc
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
	if dryRun {
		return nil
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return writeFile(p, append(out, '\n'))
}

// writeFile writes atomically, creating parent directories.
func writeFile(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(p); err == nil {
		mode = fi.Mode().Perm() // keep a private config private
	}
	tmp := p + ".ka-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
