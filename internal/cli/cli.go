// Package cli implements the ka command line (PRD §14). Every read command
// supports --json; writes print one line per change.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/render"
	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// Version is set by the main package.
var Version = "dev"

// IO bundles the streams a command uses.
type IO struct {
	In       io.Reader
	Out, Err io.Writer
	Dir      string // working directory (default: cwd)
	// Interactive makes commands prompt for missing arguments even when
	// stdin is not a terminal (tests).
	Interactive bool
	reader      *bufio.Reader
}

type command struct {
	name, args, help string
	run              func(ctx context.Context, x *IO, args []string) int
	hidden           bool
}

var commands []command

func init() {
	commands = []command{
		{"init", "[--project KEY [--prefix P]] [--name N] [--extends URL] [DIR]", "create a store (and git repo) and register the merge driver", cmdInit, false},
		{"next", "[--claim] [QUERY...]", "show (or claim) the best ready ticket", cmdNext, false},
		{"ls", "[--limit N] [--all] [--sort KEY] [QUERY...]", "list tickets matching a query", cmdLs, false},
		{"show", "ID [--comments]", "show a ticket", cmdShow, false},
		{"new", "-t TITLE [--kind K] [--priority P] [--label L]... [--accept TEXT]... [-m BODY] [--project KEY]", "create a ticket", cmdNew, false},
		{"edit", "ID [--set K=V]... [--add K=V]... [--remove K=V]...", "change fields (no flags: open $EDITOR)", cmdEdit, false},
		{"claim", "ID [--steal]", "claim a ticket", cmdClaim, false},
		{"release", "ID [--status S]", "release a claim", cmdRelease, false},
		{"move", "ID STATUS [-m SUMMARY|-] [--resolution R]", "change status (closing needs -m; - reads stdin)", cmdMove, false},
		{"comment", "ID [-m TEXT] [--type note|progress|decision]", "add a comment (text from -m or stdin)", cmdComment, false},
		{"check-item", "ID INDEX|TEXT [--undo]", "mark an acceptance item done", cmdCheckItem, false},
		{"ask", "ID -m QUESTION [--option O]... [--to WHO] [--approval] [--wait 5m]", "ask a human", cmdAsk, false},
		{"answer", "ID [--choice N] [-m TEXT] [--question NAME]", "answer a ticket's open question", cmdAnswer, false},
		{"approve", "ID [-m TEXT]", "approve a ticket's open request", cmdApprove, false},
		{"reject", "ID [-m TEXT]", "reject a ticket's open request", cmdReject, false},
		{"watch", "[--history N] [--agents|--people] [--all] [QUERY...]", "live activity stream in the terminal", cmdWatch, false},
		{"inbox", "[--since 24h]", "answers to your questions and questions for you", cmdInbox, false},
		{"schema", "[PROJECT]", "projects, statuses, kinds, labels and fields", cmdSchema, false},
		{"sync", "", "fetch, replay and push every store", cmdSync, false},
		{"check", "[--ci] [--format text|json|github|sarif] [-o FILE] [--staged] [--map URL=DIR]... [STORE_DIR...]", "validate stores against the Store Spec", cmdCheck, false},
		{"fmt", "[--check] [--staged] [STORE_DIR...]", "rewrite tickets and comments in canonical form", cmdFmt, false},
		{"hooks", "install [--force] | uninstall | status", "a git pre-commit hook running ka fmt --staged and ka check --staged", cmdHooks, false},
		{"fix", "[--dry-run]", "repair data (timestamps, actor aliases, formatting); one commit per kind", cmdFix, false},
		{"doctor", "[--fix]", "check git, merge driver, attributes and clone health", cmdDoctor, false},
		{"store", "ls | add URL|PATH [--name N] [--branch B] | rm NAME", "manage the stores in your workspace", cmdStore, false},
		{"workspace", "", "show the resolved workspace", cmdWorkspace, false},
		{"config", "explain [--project KEY] [KEY]", "show effective config and the layer that set each value", cmdConfig, false},
		{"project", "move KEY --to STORE [--dry-run]", "move a project to another store (IDs unchanged)", cmdProject, false},
		{"mcp", "", "serve MCP over stdio", cmdMCP, false},
		{"daemon", "[run [--listen ADDR] [-v]] | status | stop | openapi", "serve the REST API on 127.0.0.1:7711", cmdDaemon, false},
		{"open", "[ID] [--print]", "open the web UI (starting the daemon if needed)", cmdOpen, false},
		{"agents-md", "[--project KEY]", "print the hand-writing protocol for AGENTS.md", cmdAgentsMD, false},
		{"template", "[--project KEY]", "print a blank ticket", cmdTemplate, false},
		{"version", "", "print the version", cmdVersion, false},
		{"completion", "bash|zsh|fish|powershell", "print a shell completion script", cmdCompletion, false},
		{"__complete", "WORDS...", "completion candidates (used by the completion scripts)", cmdComplete, true},
		{"merge-driver", "%O %A %B %P", "git merge driver (registered by ka init / ka doctor --fix)", cmdMergeDriver, true},
	}
}

// Run executes a command line and returns the exit code.
func Run(ctx context.Context, x *IO, args []string) int {
	if x.Dir == "" {
		x.Dir, _ = os.Getwd()
	}
	if len(args) == 0 {
		usage(x.Err)
		return 2
	}
	name := args[0]
	switch name {
	case "help", "-h", "--help":
		usage(x.Out)
		return 0
	case "--version":
		name = "version"
	}
	for _, c := range commands {
		if c.name == name {
			return c.run(ctx, x, args[1:])
		}
	}
	fmt.Fprintf(x.Err, "ka: unknown command %q\n", name)
	usage(x.Err)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: ka <command> [flags]\n\ncommands:")
	for _, c := range commands {
		if c.hidden {
			continue
		}
		fmt.Fprintf(w, "  %-11s %s\n", c.name, c.help)
	}
	fmt.Fprint(w, `
common flags: --json (machine-readable output), --as ACTOR (act as e.g. agent/claude),
  --no-push (commit locally only), --workspace FILE, --offline
Run "ka <command> -h" for a command's flags. Queries: project:jk kind:bug -label:infra is:ready text
`)
}

// ---- flags -------------------------------------------------------------------

type common struct {
	json      bool
	as        string
	noPush    bool
	workspace string
	offline   bool
	maps      mapFlag
}

func newFlags(name, args string, x *IO) (*flag.FlagSet, *common) {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(x.Err)
	c := &common{maps: mapFlag{}}
	fl.BoolVar(&c.json, "json", false, "print JSON")
	fl.StringVar(&c.as, "as", os.Getenv("KA_ACTOR"), "act as this actor (default: you; $KA_ACTOR)")
	fl.BoolVar(&c.noPush, "no-push", false, "commit locally; do not push")
	fl.StringVar(&c.workspace, "workspace", "", "workspace file")
	fl.BoolVar(&c.offline, "offline", false, "never clone or fetch")
	fl.Var(c.maps, "map", "use a local directory for a store URL (URL=DIR, repeatable)")
	fl.Usage = func() {
		fmt.Fprintf(x.Err, "usage: ka %s %s\n", name, args)
		fl.PrintDefaults()
	}
	return fl, c
}

// parse parses flags interleaved with positional arguments.
func parse(fl *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fl.Parse(args); err != nil {
			return nil, err
		}
		rest := fl.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

type mapFlag map[string]string

func (m mapFlag) String() string { return "" }
func (m mapFlag) Set(v string) error {
	url, dir, ok := strings.Cut(v, "=")
	if !ok || url == "" || dir == "" {
		return errors.New("expected URL=DIR")
	}
	m[url] = dir
	return nil
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// ---- service -----------------------------------------------------------------

func open(ctx context.Context, x *IO, c *common) (*engine.Service, engine.Caller, error) {
	ws, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Workspace: c.workspace, Map: c.maps, Offline: c.offline})
	if err != nil {
		return nil, engine.Caller{}, err
	}
	svc, err := engine.New(ctx, ws)
	if err != nil {
		return nil, engine.Caller{}, err
	}
	svc.NoPush = c.noPush || c.offline
	return svc, svc.CallerFor(c.as, os.Getenv("KA_SESSION")), nil
}

func fail(x *IO, c *common, err error) int {
	e := engine.AsError(err)
	if c != nil && c.json {
		enc := json.NewEncoder(x.Out)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"error": e})
	} else {
		fmt.Fprintln(x.Err, "ka: "+render.Error(e))
	}
	return 1
}

func emit(x *IO, c *common, v any, text string) int {
	if c.json {
		enc := json.NewEncoder(x.Out)
		enc.SetIndent("", "  ")
		enc.Encode(v)
		return 0
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	fmt.Fprint(x.Out, text)
	return 0
}

func usageErr(x *IO, fl *flag.FlagSet, msg string) int {
	fmt.Fprintln(x.Err, "ka:", msg)
	fl.Usage()
	return 2
}

func readText(x *IO, m string) string {
	if m == "-" || (m == "" && x.In != nil && !isTTY(x.In) && !x.Interactive) {
		b, _ := io.ReadAll(x.In)
		return strings.TrimSpace(string(b))
	}
	return m
}

// readExplicit reads stdin only for "-": for optional text (ka move's
// summary), an unset flag must never wait on an open pipe.
func readExplicit(x *IO, m string) string {
	if m == "-" {
		return readText(x, m)
	}
	return m
}

func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// ---- reading commands ------------------------------------------------------------

func cmdNext(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("next", "[--claim] [QUERY...]", x)
	claim := fl.Bool("claim", false, "claim it")
	project := fl.String("project", "", "limit to a project")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Next(ctx, caller, engine.NextIn{Query: strings.Join(pos, " "), Project: *project, Peek: !*claim})
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Next(out, render.DetailOptions{BodyLimit: 2000}))
}

func cmdLs(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("ls", "[QUERY...]", x)
	limit := fl.Int("limit", 50, "max results")
	all := fl.Bool("all", false, "include closed tickets")
	var sorts multi
	fl.Var(&sorts, "sort", "sort key (priority, created, updated, id, title, status; -KEY descending)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	out, err := svc.Search(engine.SearchIn{Query: strings.Join(pos, " "), Limit: *limit, All: *all, Sort: sorts})
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Search(out))
}

func cmdShow(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("show", "ID [--comments]", x)
	comments := fl.Bool("comments", false, "include every comment")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return usageErr(x, fl, "show needs one ID")
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	in := engine.GetIn{ID: pos[0]}
	if *comments {
		in.Include = []string{"comments", "children"}
	}
	d, err := svc.Get(in)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, d, render.Detail(d, render.DetailOptions{}))
}

func cmdSchema(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("schema", "[PROJECT]", x)
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	p := ""
	if len(pos) > 0 {
		p = pos[0]
	}
	out, err := svc.Schema(caller, p)
	if err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, out, render.Schema(out))
}

func cmdInbox(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("inbox", "[--since 24h]", x)
	since := fl.String("since", "", "only answers newer than this (RFC 3339 or a duration)")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	s := *since
	if d, err := kaspec.ParseDuration(s); err == nil && s != "" {
		s = time.Now().Add(-d).UTC().Format("2006-01-02T15:04:05Z")
	}
	out := svc.Inbox(caller, s)
	return emit(x, c, out, render.Inbox(out))
}

func cmdWorkspace(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("workspace", "", x)
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	svc, caller, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	type st struct {
		Name     string `json:"name"`
		Root     string `json:"root"`
		URL      string `json:"url,omitempty"`
		Managed  bool   `json:"managed"`
		ReadOnly bool   `json:"read_only"`
	}
	out := struct {
		Source  string            `json:"source"`
		Actor   string            `json:"actor"`
		Project string            `json:"default_project,omitempty"`
		Stores  []st              `json:"stores"`
		Repos   map[string]string `json:"repos,omitempty"`
	}{Source: svc.WS.Source, Actor: caller.Actor, Project: svc.WS.Project, Repos: svc.WS.Repos}
	var b strings.Builder
	fmt.Fprintf(&b, "workspace: %s\nyou: %s\n", out.Source, out.Actor)
	if out.Project != "" {
		fmt.Fprintf(&b, "default project: %s\n", out.Project)
	}
	for _, s := range svc.WS.Stores {
		out.Stores = append(out.Stores, st{s.Name, s.Root, s.URL, s.Managed, s.ReadOnly})
		mode := "attached"
		if s.Managed {
			mode = "managed"
		}
		if s.ReadOnly {
			mode += ", read-only (extends parent)"
		}
		fmt.Fprintf(&b, "store %s: %s (%s)\n", s.Name, s.Root, mode)
	}
	keys := make([]string, 0, len(out.Repos))
	for k := range out.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "repo %s: %s\n", k, out.Repos[k])
	}
	return emit(x, c, out, b.String())
}

func cmdVersion(ctx context.Context, x *IO, args []string) int {
	fmt.Fprintf(x.Out, "ka %s (store spec %d)\n", Version, kaspec.SpecVersion)
	return 0
}
