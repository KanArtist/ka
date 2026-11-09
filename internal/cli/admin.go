package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/gitx"
	"github.com/JumpKickOSS/ka/internal/mcpserver"
	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// findStore walks up from dir to the nearest directory containing .ka/store.yml.
func findStore(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".ka", "store.yml")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no KanArtist store found at or above %s (looked for .ka/store.yml)", abs)
		}
	}
}

// newKaspecWorkspace loads explicit store directories for ka check/fmt.
// extends URLs that are local paths resolve relative to the extending store.
func newKaspecWorkspace(dirs []string, maps mapFlag) *kaspec.Workspace {
	ws := kaspec.NewWorkspace(nil)
	ws.Resolve = func(from *kaspec.Store, p kaspec.Parent) (fs.FS, string, string, error) {
		var dir string
		switch {
		case maps[p.URL] != "":
			dir = maps[p.URL]
		case strings.HasPrefix(p.URL, "file://"):
			dir = strings.TrimPrefix(p.URL, "file://")
		case strings.HasPrefix(p.URL, "./"), strings.HasPrefix(p.URL, "../"), filepath.IsAbs(p.URL):
			dir = p.URL
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(ws.KeyOf(from), dir)
			}
		default:
			return nil, "", "", fmt.Errorf("no local copy (map it with --map %s=DIR)", p.URL)
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, "", "", err
		}
		if _, err := os.Stat(filepath.Join(abs, ".ka", "store.yml")); err != nil {
			return nil, "", "", fmt.Errorf("%s is not a store", abs)
		}
		return os.DirFS(abs), abs, filepath.Base(abs), nil
	}
	for _, d := range dirs {
		label := ""
		if len(dirs) > 1 {
			label = filepath.Base(d)
		}
		ws.Add(os.DirFS(d), d, label)
	}
	return ws
}

func cmdCheck(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("check", "[--strict] [--ci] [--format text|json|github|sarif] [-o FILE] [STORE_DIR...]", x)
	strict := fl.Bool("strict", false, "fail on warnings too")
	ci := fl.Bool("ci", false, "for CI: fail on warnings, and print GitHub annotations when run in GitHub Actions")
	format := fl.String("format", "", "output: text (default), json, github (workflow annotations) or sarif (code scanning)")
	output := fl.String("o", "", "write the report to this file instead of stdout (the summary still prints)")
	staged := fl.Bool("staged", false, "check the stores as staged in the git index (for the pre-commit hook)")
	nowFlag := fl.String("now", "", "evaluate stale claims as of this RFC 3339 time (default: now)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	*strict = *strict || *ci
	if c.json && *format == "" {
		*format = "json"
	}
	if *format == "" {
		*format = "text"
		if *ci && os.Getenv("GITHUB_ACTIONS") == "true" {
			*format = "github"
		}
	}
	switch *format {
	case "text", "json", "github", "sarif":
	default:
		fmt.Fprintf(x.Err, "ka: --format %q: use text, json, github or sarif\n", *format)
		return 2
	}
	now := time.Now().UTC()
	if *nowFlag != "" {
		if now, err = time.Parse(time.RFC3339, *nowFlag); err != nil {
			fmt.Fprintln(x.Err, "ka: --now:", err)
			return 2
		}
	}
	var ws *kaspec.Workspace
	var diags []kaspec.Diagnostic
	if *staged {
		sws, cleanup, err := stagedStores(ctx, x.Dir, c.maps)
		if err != nil {
			return fail(x, c, err)
		}
		defer cleanup()
		ws = sws
		diags = kaspec.Check(ws, kaspec.CheckOptions{Now: now})
	} else if len(pos) > 0 {
		var dirs []string
		for _, a := range pos {
			if !filepath.IsAbs(a) {
				a = filepath.Join(x.Dir, a)
			}
			dirs = append(dirs, filepath.Clean(a))
		}
		ws = newKaspecWorkspace(dirs, c.maps)
		diags = kaspec.Check(ws, kaspec.CheckOptions{Now: now})
	} else {
		ews, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Workspace: c.workspace, Map: c.maps, Offline: true})
		if err != nil {
			// Not in a workspace: fall back to the store around the directory.
			d, ferr := findStore(x.Dir)
			if ferr != nil {
				return fail(x, c, err)
			}
			ws = newKaspecWorkspace([]string{d}, c.maps)
			diags = kaspec.Check(ws, kaspec.CheckOptions{Now: now})
		} else {
			svc, err := engine.New(ctx, ews)
			if err != nil {
				return fail(x, c, err)
			}
			diags = svc.Validate(now)
			ws = svc.Index().Workspace()
		}
	}
	out := x.Out
	if *output != "" {
		p := *output
		if !filepath.IsAbs(p) {
			p = filepath.Join(x.Dir, p)
		}
		f, err := os.Create(p)
		if err != nil {
			return fail(x, c, err)
		}
		defer f.Close()
		out = f
	}
	base := x.Dir
	if gw := os.Getenv("GITHUB_WORKSPACE"); gw != "" {
		base = gw
	}
	counts := map[kaspec.Severity]int{}
	for _, d := range diags {
		counts[d.Severity]++
	}
	tickets, comments := 0, 0
	for _, s := range ws.Stores {
		for _, pr := range s.Projects {
			tickets += len(pr.Tickets)
			for _, te := range pr.Tickets {
				comments += len(te.Comments)
			}
		}
	}
	summary := fmt.Sprintf("checked %d store(s), %d ticket(s), %d comment(s): %d error(s), %d warning(s), %d info",
		len(ws.Stores), tickets, comments, counts[kaspec.SevError], counts[kaspec.SevWarning], counts[kaspec.SevInfo])
	switch *format {
	case "json":
		if diags == nil {
			diags = []kaspec.Diagnostic{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(diags)
	case "sarif":
		if err := writeSARIF(out, ws, diags, base, Version); err != nil {
			return fail(x, c, err)
		}
	case "github":
		writeGitHub(out, ws, diags, base)
	default:
		for _, d := range diags {
			fmt.Fprintln(out, d.String())
		}
	}
	if *format == "text" || *format == "github" || *output != "" {
		fmt.Fprintln(x.Out, summary)
	}
	if kaspec.HasErrors(diags, *strict) {
		return 1
	}
	return 0
}

func cmdFmt(ctx context.Context, x *IO, args []string) int {
	fl, _ := newFlags("fmt", "[--check] [--staged] [STORE_DIR...]", x)
	checkOnly := fl.Bool("check", false, "list files that are not canonical; change nothing")
	staged := fl.Bool("staged", false, "format staged tickets and comments and stage the result (for the pre-commit hook)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if *staged {
		return fmtStaged(ctx, x)
	}
	var dirs []string
	if len(pos) == 0 {
		d, err := findStore(x.Dir)
		if err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 2
		}
		dirs = []string{d}
	}
	for _, a := range pos {
		if !filepath.IsAbs(a) {
			a = filepath.Join(x.Dir, a)
		}
		dirs = append(dirs, a)
	}
	changed, skipped := 0, 0
	for _, root := range dirs {
		err := filepath.WalkDir(filepath.Join(root, "projects"), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out, isStore, ok := formatFile(rel, data)
			if !isStore {
				return nil
			}
			if !ok {
				fmt.Fprintf(x.Err, "skip %s: has errors (run ka check)\n", rel)
				skipped++
				return nil
			}
			if string(out) == string(data) {
				return nil
			}
			changed++
			if *checkOnly {
				fmt.Fprintln(x.Out, rel)
				return nil
			}
			fmt.Fprintln(x.Out, "formatted", rel)
			return os.WriteFile(p, out, 0o644)
		})
		if err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
	}
	if (*checkOnly && changed > 0) || skipped > 0 {
		return 1
	}
	return 0
}

// ---- doctor ---------------------------------------------------------------------

type finding struct {
	Store  string `json:"store,omitempty"`
	Status string `json:"status"` // ok, warn, fail, fixed
	Check  string `json:"check"`
	Detail string `json:"detail,omitempty"`
}

func cmdDoctor(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("doctor", "[--fix]", x)
	fix := fl.Bool("fix", false, "repair what can be repaired")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	var fs []finding
	add := func(store, status, check, detail string) {
		fs = append(fs, finding{store, status, check, detail})
	}
	maj, min, raw, err := gitx.Version(ctx)
	switch {
	case err != nil:
		add("", "fail", "git", "git not found: install git 2.40 or later")
	case maj < gitx.MinVersion[0] || (maj == gitx.MinVersion[0] && min < gitx.MinVersion[1]):
		add("", "fail", "git", raw+" is too old; ka needs 2.40 or later")
	default:
		add("", "ok", "git", raw)
	}
	// Checking never touches the network; --fix also clones stores that have
	// no local clone yet (as on a machine where they were just added).
	ws, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Workspace: c.workspace, Map: c.maps, Offline: !*fix || c.offline})
	if err != nil {
		add("", "fail", "workspace", engine.AsError(err).Error())
	} else {
		for _, st := range ws.Stores {
			g := st.Git()
			if p := g.InProgress(ctx); p != "" {
				if *fix && st.Managed {
					g.Run(ctx, p, "--abort")
					add(st.Name, "fixed", "git state", "aborted a "+p)
				} else {
					add(st.Name, "fail", "git state", "a "+p+" is in progress in "+g.Dir+"; finish it or run git "+p+" --abort")
				}
			}
			if g.Branch(ctx) == "" {
				add(st.Name, "fail", "branch", "HEAD is detached; check out the store's branch")
			}
			if st.ReadOnly {
				continue
			}
			drv, _ := g.Run(ctx, "config", "merge.ka.driver")
			switch {
			case drv != "" && (engine.MergeDriverCommand == "" || drv == engine.MergeDriverCommand):
				add(st.Name, "ok", "merge driver", "registered")
			case *fix && engine.MergeDriverCommand != "":
				if err := engine.RegisterMergeDriver(ctx, g); err == nil {
					add(st.Name, "fixed", "merge driver", "registered "+engine.MergeDriverCommand)
				}
			case drv != "":
				add(st.Name, "warn", "merge driver", "registered as "+drv+"; ka doctor --fix points it at this binary")
			default:
				add(st.Name, "warn", "merge driver", "not registered (git falls back to text merges); run ka doctor --fix")
			}
			attrs, _ := os.ReadFile(filepath.Join(st.Root, ".gitattributes"))
			if strings.Contains(string(attrs), "ticket.md merge=ka") {
				add(st.Name, "ok", ".gitattributes", "ticket.md merge=ka")
			} else if *fix {
				content := string(attrs)
				if content != "" && !strings.HasSuffix(content, "\n") {
					content += "\n"
				}
				if !strings.Contains(content, "eol=lf") {
					content += "* text=auto eol=lf\n"
				}
				content += "ticket.md merge=ka\n"
				os.WriteFile(filepath.Join(st.Root, ".gitattributes"), []byte(content), 0o644)
				g.Run(ctx, "add", "--", filepath.Join(st.Root, ".gitattributes"))
				g.RunRaw(ctx, []byte("ka doctor: route ticket.md through the ka merge driver\n"), "commit", "-q", "--no-verify", "-F", "-")
				add(st.Name, "fixed", ".gitattributes", "added ticket.md merge=ka (committed)")
			} else {
				add(st.Name, "warn", ".gitattributes", "ticket.md is not routed to the merge driver; run ka doctor --fix")
			}
			if dirty, _ := g.Status(ctx, st.Root); len(dirty) > 0 {
				status := "warn"
				if st.Managed {
					status = "fail"
				}
				add(st.Name, status, "working tree", fmt.Sprintf("%d uncommitted change(s) in the store", len(dirty)))
			}
			if out, err := g.Run(ctx, "log", "-1", "--format=%cI"); err == nil && out != "" {
				if t, err := time.Parse(time.RFC3339, out); err == nil && time.Until(t) > 5*time.Minute {
					add(st.Name, "warn", "clock", "the newest commit is dated "+out+", in the future: check this machine's clock")
				}
			}
			stt := st.State(ctx)
			add(st.Name, "ok", "sync", stt.State)
		}
	}
	if c.json {
		return emit(x, c, fs, "")
	}
	bad := 0
	for _, f := range fs {
		where := ""
		if f.Store != "" {
			where = f.Store + ": "
		}
		fmt.Fprintf(x.Out, "%-5s %s%s: %s\n", f.Status, where, f.Check, f.Detail)
		if f.Status == "fail" {
			bad++
		}
	}
	if bad > 0 {
		return 1
	}
	return 0
}

// ---- store management -------------------------------------------------------------

// isBareRepo reports whether dir is a bare git repository: a local remote
// to clone, not a checkout to attach.
func isBareRepo(dir string) bool {
	_, e1 := os.Stat(filepath.Join(dir, "HEAD"))
	_, e2 := os.Stat(filepath.Join(dir, "objects"))
	_, e3 := os.Stat(filepath.Join(dir, ".ka", "store.yml"))
	return e1 == nil && e2 == nil && e3 != nil
}

func cmdStore(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("store", "ls | add URL|PATH [--name N] [--branch B] | rm NAME", x)
	name := fl.String("name", "", "store name")
	branch := fl.String("branch", "", "branch (for orphan-branch stores)")
	pos, err := parse(fl, args)
	if err != nil {
		return 2
	}
	if len(pos) == 0 || pos[0] == "ls" {
		return cmdWorkspace(ctx, x, args[min(1, len(args)):])
	}
	path := c.workspace
	if path == "" {
		path = filepath.Join(engine.ConfigDir(), "workspace.yml")
	}
	wf := &engine.WorkspaceFile{Path: path}
	if _, err := os.Stat(path); err == nil {
		if wf, err = engine.ParseWorkspaceFile(path); err != nil {
			return fail(x, c, err)
		}
	}
	switch pos[0] {
	case "add":
		if len(pos) != 2 {
			return usageErr(x, fl, "store add needs a URL or path")
		}
		sp := engine.StoreSpec{Name: *name, Branch: *branch}
		target := pos[1]
		if fi, err := os.Stat(filepath.Join(x.Dir, target)); err == nil && fi.IsDir() && !filepath.IsAbs(target) {
			target = filepath.Join(x.Dir, target)
		}
		if fi, err := os.Stat(target); err == nil && fi.IsDir() && !isBareRepo(target) {
			root, err := findStore(target)
			if err != nil {
				return fail(x, c, err)
			}
			sp.Path = root
		} else {
			sp.URL = target
		}
		if sp.Name == "" {
			base := filepath.Base(strings.TrimSuffix(strings.TrimSuffix(target, "/"), ".git"))
			if i := strings.LastIndexAny(base, ":/"); i >= 0 {
				base = base[i+1:]
			}
			sp.Name = strings.ToLower(base)
		}
		for _, s := range wf.Stores {
			if s.Name == sp.Name {
				return fail(x, c, engine.Errf(engine.CodeInvalid, "pass --name", "a store named %s is already in %s", sp.Name, path))
			}
		}
		wf.Stores = append(wf.Stores, sp)
	case "rm":
		if len(pos) != 2 {
			return usageErr(x, fl, "store rm needs a name")
		}
		n := len(wf.Stores)
		var keep []engine.StoreSpec
		for _, s := range wf.Stores {
			if s.Name != pos[1] {
				keep = append(keep, s)
			}
		}
		if len(keep) == n {
			return fail(x, c, engine.Errf(engine.CodeNotFound, "ka store ls lists them", "no store named %s", pos[1]))
		}
		wf.Stores = keep
	default:
		return usageErr(x, fl, "unknown store subcommand "+pos[0])
	}
	if err := writeWorkspaceFile(wf); err != nil {
		return fail(x, c, err)
	}
	return emit(x, c, map[string]any{"workspace": path}, "updated "+path)
}

func writeWorkspaceFile(wf *engine.WorkspaceFile) error {
	var kv []kaspec.KV
	if wf.Name != "" {
		kv = append(kv, kaspec.KV{K: "name", V: wf.Name})
	}
	if wf.Project != "" {
		kv = append(kv, kaspec.KV{K: "project", V: wf.Project})
	}
	var stores []any
	for _, s := range wf.Stores {
		e := []kaspec.KV{{K: "name", V: s.Name}, {K: "url", V: s.URL}, {K: "path", V: s.Path}, {K: "branch", V: s.Branch}, {K: "push", V: s.Push}}
		stores = append(stores, e)
	}
	kv = append(kv, kaspec.KV{K: "stores", V: stores})
	if wf.Actor != "" {
		kv = append(kv, kaspec.KV{K: "identity", V: []kaspec.KV{{K: "actor", V: wf.Actor}}})
	}
	if len(wf.Repos) > 0 {
		var r []kaspec.KV
		for _, k := range sortedKeys(wf.Repos) {
			r = append(r, kaspec.KV{K: k, V: wf.Repos[k]})
		}
		kv = append(kv, kaspec.KV{K: "repos", V: r})
	}
	if err := os.MkdirAll(filepath.Dir(wf.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(wf.Path, []byte(kaspec.EmitYAML(kv)), 0o644)
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

// ---- mcp, protocol helpers --------------------------------------------------------

func cmdMCP(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("mcp", "[--no-daemon]", x)
	noDaemon := fl.Bool("no-daemon", os.Getenv("KA_NO_DAEMON") != "", "run the engine in-process even if a daemon is running ($KA_NO_DAEMON)")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	// With a daemon for this workspace, bridge to it: one index, presence
	// and live events for everyone (PRD §8.3).
	if !*noDaemon && !c.offline && !c.noPush && len(c.maps) == 0 {
		if rt, _, err := runningDaemon(ctx, x, c); err == nil && rt != nil {
			hdr := http.Header{}
			if c.as != "" {
				hdr.Set("X-Ka-Actor", c.as)
			}
			if sid := os.Getenv("KA_SESSION"); sid != "" {
				hdr.Set("X-Ka-Session", sid)
			}
			p := &mcpserver.Proxy{Endpoint: rt.URL + "/mcp", Token: rt.Token, Header: hdr}
			if err := p.Run(ctx, x.In, x.Out); err != nil && ctx.Err() == nil {
				fmt.Fprintln(x.Err, "ka mcp:", err)
				return 1
			}
			return 0
		}
	}
	svc, _, err := open(ctx, x, c)
	if err != nil {
		return fail(x, c, err)
	}
	opt := mcpserver.Env()
	if c.as != "" {
		opt.Actor = c.as
	}
	mcpserver.Version = Version
	svc.Background = true // debounced pushes, flushed below
	defer svc.Flush(context.Background())
	if err := mcpserver.New(svc, opt).RunIO(ctx, x.In, x.Out); err != nil && ctx.Err() == nil {
		fmt.Fprintln(x.Err, "ka mcp:", err)
		return 1
	}
	return 0
}

func projectPrefix(ctx context.Context, x *IO, c *common, project string) (string, string, string) {
	ws, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Workspace: c.workspace, Offline: true})
	if err != nil {
		return project, "", ""
	}
	svc, err := engine.New(ctx, ws)
	if err != nil {
		return project, "", ""
	}
	if project == "" {
		project = ws.Project
	}
	sch, err := svc.Schema(engine.Caller{}, project)
	if err != nil || len(sch.Projects) != 1 {
		return project, "", ""
	}
	return sch.Projects[0].Key, sch.Projects[0].Prefix, sch.Projects[0].Initial
}

func cmdAgentsMD(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("agents-md", "[--project KEY]", x)
	project := fl.String("project", "", "project key")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	key, prefix, _ := projectPrefix(ctx, x, c, *project)
	fmt.Fprint(x.Out, engine.AgentsMD(key, prefix))
	return 0
}

func cmdTemplate(ctx context.Context, x *IO, args []string) int {
	fl, c := newFlags("template", "[--project KEY]", x)
	project := fl.String("project", "", "project key")
	if _, err := parse(fl, args); err != nil {
		return 2
	}
	_, prefix, initial := projectPrefix(ctx, x, c, *project)
	fmt.Fprint(x.Out, engine.Template(prefix, initial, time.Now().UTC().Format("2006-01-02T15:04:05Z")))
	return 0
}
