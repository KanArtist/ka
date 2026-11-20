package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KanArtist/ka/internal/api"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// Shell completion (PRD §14: ka completion <shell>). The scripts call the
// hidden `ka __complete WORDS...`, which lists candidates for the last word:
// commands, a command's flags (read from its own -h output, so they never
// drift), and ticket IDs for commands that take one.

// takesID lists commands whose first argument is a ticket ID.
var takesID = map[string]bool{"show": true, "edit": true, "claim": true, "release": true, "move": true, "comment": true,
	"check-item": true, "ask": true, "answer": true, "approve": true, "reject": true, "open": true, "tree": true}

func cmdComplete(ctx context.Context, x *IO, args []string) int {
	cur := ""
	if len(args) > 0 {
		cur = args[len(args)-1]
		args = args[:len(args)-1]
	}
	var out []string
	switch {
	case len(args) == 0:
		for _, c := range commands {
			if !c.hidden && strings.HasPrefix(c.name, cur) {
				out = append(out, c.name)
			}
		}
		for _, s := range []string{"help", "completion"} {
			if strings.HasPrefix(s, cur) {
				out = append(out, s)
			}
		}
	case strings.HasPrefix(cur, "-"):
		out = commandFlags(ctx, args[0], cur)
	case args[0] == "completion":
		for _, s := range []string{"bash", "zsh", "fish", "powershell"} {
			if strings.HasPrefix(s, cur) {
				out = append(out, s)
			}
		}
	case takesID[args[0]] && positional(args[1:]) == 0:
		out = ticketIDs(ctx, x, cur)
	case args[0] == "move" && positional(args[1:]) == 1:
		out = statuses(ctx, x, cur)
	}
	sort.Strings(out)
	for _, s := range out {
		fmt.Fprintln(x.Out, s)
	}
	return 0
}

// positional counts non-flag words (a rough count: flag values are not
// told apart from positionals, which is fine for completion).
func positional(words []string) int {
	n := 0
	for _, w := range words {
		if !strings.HasPrefix(w, "-") {
			n++
		}
	}
	return n
}

func commandFlags(ctx context.Context, name, cur string) []string {
	for _, c := range commands {
		if c.name != name {
			continue
		}
		var buf bytes.Buffer
		c.run(ctx, &IO{Out: &buf, Err: &buf, In: strings.NewReader(""), Dir: "."}, []string{"-h"})
		var out []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if !strings.HasPrefix(line, "  -") {
				continue
			}
			f := strings.Fields(line)[0]
			flag := "-" + strings.TrimPrefix(f, "-")
			if strings.HasPrefix("-"+flag, cur) {
				out = append(out, "-"+flag)
			} else if strings.HasPrefix(flag, cur) {
				out = append(out, flag)
			}
		}
		return out
	}
	return nil
}

// Ticket ID and status completion runs on every TAB, so it must not build
// the index (every ticket and comment parsed, git spawned per store). It
// tries, in order:
//
//  1. a running daemon for the workspace (api.Discover: a runtime-file read
//     and a /healthz probe), asked over its REST API with a short timeout;
//  2. a scan of the store files: projects/*/tickets/ listings filtered by
//     the typed prefix, reading only ticket.md of the candidates (no
//     comments, attachments, history or boards) plus the store and project
//     config needed to tell open statuses from closed ones;
//  3. the index (indexTicketIDs, indexStatuses): only if the scan cannot
//     resolve a project's config. It is also the reference the other two
//     must agree with (completion_test.go).
//
// All three return the same thing: the open tickets whose ID starts with the
// typed word (upper-cased), the first completeCap of them in the default
// search order (priority, then most recently updated, then ID).

// completeCap is the most ticket IDs completion offers.
const completeCap = 300

// completeDaemonTimeout bounds the whole exchange with a running daemon;
// past it completion reads the files itself.
const completeDaemonTimeout = 400 * time.Millisecond

// openWorkspaceQuiet finds the workspace without building the index, never
// cloning or fetching.
func openWorkspaceQuiet(ctx context.Context, x *IO) *engine.Workspace {
	ws, err := engine.Open(ctx, engine.OpenOptions{Dir: x.Dir, Offline: true})
	if err != nil {
		return nil
	}
	return ws
}

func ticketIDs(ctx context.Context, x *IO, cur string) []string {
	ws := openWorkspaceQuiet(ctx, x)
	if ws == nil {
		return nil
	}
	if out, ok := daemonTicketIDs(ctx, ws.Source, cur); ok {
		return out
	}
	if out, ok := scanTicketIDs(ws, cur); ok {
		return out
	}
	return indexTicketIDs(ctx, ws, cur)
}

func statuses(ctx context.Context, x *IO, cur string) []string {
	ws := openWorkspaceQuiet(ctx, x)
	if ws == nil {
		return nil
	}
	if out, ok := daemonStatuses(ctx, ws.Source, cur); ok {
		return out
	}
	if out, ok := scanStatuses(ws, cur); ok {
		return out
	}
	return indexStatuses(ctx, ws, cur)
}

// ---- 3: the index (reference) ------------------------------------------------

// indexTicketIDs completes ticket IDs from the full index. It is the slow
// reference the faster paths reproduce.
func indexTicketIDs(ctx context.Context, ws *engine.Workspace, cur string) []string {
	svc, err := engine.New(ctx, ws)
	if err != nil {
		return nil
	}
	res, err := svc.Search(engine.SearchIn{Limit: -1})
	if err != nil {
		return nil
	}
	var out []string
	up := strings.ToUpper(cur)
	for _, t := range res.Tickets {
		if strings.HasPrefix(t.ID, up) {
			out = append(out, t.ID)
		}
		if len(out) >= completeCap {
			break
		}
	}
	return out
}

// indexStatuses completes status names from the full index (reference).
func indexStatuses(ctx context.Context, ws *engine.Workspace, cur string) []string {
	svc, err := engine.New(ctx, ws)
	if err != nil {
		return nil
	}
	sch, err := svc.Schema(engine.Caller{}, "")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range sch.Projects {
		for _, s := range p.Statuses {
			if !seen[s.Name] && strings.HasPrefix(s.Name, cur) {
				seen[s.Name] = true
				out = append(out, s.Name)
			}
		}
	}
	return out
}

// ---- 1: a running daemon ----------------------------------------------------------

// daemonGet asks the workspace's running daemon for path and decodes the
// JSON answer into out. It never starts a daemon; false means no answer.
func daemonGet(ctx context.Context, source, path string, out any) bool {
	ctx, cancel := context.WithTimeout(ctx, completeDaemonTimeout)
	defer cancel()
	rt := api.Discover(ctx, source)
	if rt == nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.URL+path, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+rt.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(out) == nil
}

// daemonTicketIDs asks the daemon for open tickets whose ID matches the
// typed prefix (id:PREFIX*), in the default order, at most completeCap.
func daemonTicketIDs(ctx context.Context, source, cur string) ([]string, bool) {
	for _, c := range cur {
		// Anything else could mean something to the query language.
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return nil, false
		}
	}
	up := strings.ToUpper(cur)
	q := "is:open"
	if up != "" {
		q += " id:" + up + "*"
	}
	var res struct {
		Total   int `json:"total"`
		Tickets []struct {
			ID string `json:"id"`
		} `json:"tickets"`
	}
	if !daemonGet(ctx, source, "/api/v1/tickets?limit="+strconv.Itoa(completeCap)+"&q="+neturl.QueryEscape(q), &res) {
		return nil, false
	}
	var out []string
	for _, t := range res.Tickets {
		if strings.HasPrefix(t.ID, up) {
			out = append(out, t.ID)
		}
	}
	// id: globs ignore case; an ID with lower-case letters can match there
	// and not here. If such IDs took places in a truncated answer, the
	// answer is short of tickets the files would give: read the files.
	if len(out) < len(res.Tickets) && res.Total > len(res.Tickets) {
		return nil, false
	}
	return out, true
}

// daemonStatuses asks the daemon for every project's statuses.
func daemonStatuses(ctx context.Context, source, cur string) ([]string, bool) {
	var res struct {
		Projects []struct {
			Statuses []struct {
				Name string `json:"name"`
			} `json:"statuses"`
		} `json:"projects"`
	}
	if !daemonGet(ctx, source, "/api/v1/schema", &res) {
		return nil, false
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range res.Projects {
		for _, s := range p.Statuses {
			if !seen[s.Name] && strings.HasPrefix(s.Name, cur) {
				seen[s.Name] = true
				out = append(out, s.Name)
			}
		}
	}
	return out, true
}

// ---- 2: a scan of the store files ------------------------------------------------

// completeFS is a store directory as completion needs to see it: ticket
// directories whose name starts with prefix (none when !tickets), each with
// only its ticket.md, and no history/ or boards/. kaspec.LoadStore over it
// loads the store, project and config files as the index does, and only
// the candidate tickets' ticket.md.
type completeFS struct {
	fs.FS
	tickets bool
	prefix  string
}

func (f completeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	ents, err := fs.ReadDir(f.FS, name)
	if err != nil {
		return ents, err
	}
	parts := strings.Split(name, "/")
	var keep func(fs.DirEntry) bool
	switch {
	case name == ".":
		keep = func(e fs.DirEntry) bool { return e.Name() != "history" && e.Name() != "boards" }
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "tickets":
		keep = func(e fs.DirEntry) bool { return f.tickets && strings.HasPrefix(e.Name(), f.prefix) }
	case len(parts) == 4 && parts[0] == "projects" && parts[2] == "tickets":
		keep = func(e fs.DirEntry) bool { return e.Name() == "ticket.md" }
	default:
		return ents, nil
	}
	out := ents[:0]
	for _, e := range ents {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// scanWorkspace loads every store of ws through completeFS, in parallel,
// into a kaspec workspace that resolves extends parents the way the index
// does. It returns nil when a store requires a newer ka (engine.New
// refuses such a workspace, so completion offers nothing).
func scanWorkspace(ws *engine.Workspace, tickets bool, prefix string) (*kaspec.Workspace, []*kaspec.Store) {
	kss := make([]*kaspec.Store, len(ws.Stores))
	var wg sync.WaitGroup
	for i, st := range ws.Stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kss[i] = kaspec.LoadStore(completeFS{FS: os.DirFS(st.Root), tickets: tickets, prefix: prefix}, st.Name)
		}()
	}
	wg.Wait()
	for _, ks := range kss {
		if ks.File != nil && !kaspec.KaVersionSatisfies(ks.File.RequiresKa) {
			return nil, nil
		}
	}
	root := map[*kaspec.Store]string{}
	kws := kaspec.NewWorkspace(func(from *kaspec.Store, p kaspec.Parent) (fs.FS, string, string, error) {
		if st := engine.FindParent(ws.Stores, root[from], p); st != nil {
			return os.DirFS(st.Root), st.Root, st.Name, nil
		}
		return nil, "", "", fmt.Errorf("no local copy of %s", p.URL)
	})
	for i, st := range ws.Stores {
		root[kss[i]] = st.Root
		kws.AddLoaded(kss[i], st.Root)
	}
	return kws, kss
}

// scanProjects calls fn for each project the index would hold, in its
// order (stores in workspace order, keys sorted, the first of a duplicated
// key), with the project's effective config. fn returning false stops.
func scanProjects(kws *kaspec.Workspace, kss []*kaspec.Store, fn func(*kaspec.Project, *kaspec.Config) bool) bool {
	seen := map[string]bool{}
	for _, ks := range kss {
		for _, key := range ks.Keys {
			if seen[key] {
				continue
			}
			seen[key] = true
			pr := ks.Projects[key]
			cfg, _ := kaspec.ProjectConfig(kws, ks, pr)
			if cfg == nil || !fn(pr, cfg) {
				return false
			}
		}
	}
	return true
}

// scanTicketIDs completes ticket IDs from the files. Only ticket
// directories matching the prefix are read, but all of them: the first
// completeCap open ones in priority/updated order need every candidate's
// status, priority and updated. With an empty prefix that is every
// ticket.md (still no comments, attachments or git).
func scanTicketIDs(ws *engine.Workspace, cur string) ([]string, bool) {
	up := strings.ToUpper(cur)
	kws, kss := scanWorkspace(ws, true, up)
	if kws == nil {
		return nil, true
	}
	type cand struct {
		id, updated string
		prio        int
		open        bool
	}
	byID := map[string]cand{}
	ok := scanProjects(kws, kss, func(pr *kaspec.Project, cfg *kaspec.Config) bool {
		for _, id := range pr.IDs {
			// As the index: a later project's ticket with the same ID wins,
			// and an unparsable ticket.md has no status, so it is open.
			c := cand{id: id, prio: len(kaspec.Priorities)}
			status := ""
			if t := pr.Tickets[id].Ticket; t != nil {
				status, c.updated = t.Status, t.Updated
				if i := slices.Index(kaspec.Priorities, t.Priority); i >= 0 {
					c.prio = i
				}
			}
			st, _ := cfg.StatusByName(status)
			c.open = st.Category != "completed" && st.Category != "canceled"
			byID[id] = c
		}
		return true
	})
	if !ok {
		return nil, false
	}
	var open []cand
	for _, c := range byID {
		if c.open && strings.HasPrefix(c.id, up) {
			open = append(open, c)
		}
	}
	slices.SortFunc(open, func(a, b cand) int {
		if a.prio != b.prio {
			return a.prio - b.prio
		}
		if c := strings.Compare(b.updated, a.updated); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	var out []string
	for _, c := range open[:min(len(open), completeCap)] {
		out = append(out, c.id)
	}
	return out, true
}

// scanStatuses completes status names from the store and project config,
// reading no tickets.
func scanStatuses(ws *engine.Workspace, cur string) ([]string, bool) {
	kws, kss := scanWorkspace(ws, false, "")
	if kws == nil {
		return nil, true
	}
	seen := map[string]bool{}
	var out []string
	ok := scanProjects(kws, kss, func(_ *kaspec.Project, cfg *kaspec.Config) bool {
		for _, s := range cfg.Statuses {
			if !seen[s.Name] && strings.HasPrefix(s.Name, cur) {
				seen[s.Name] = true
				out = append(out, s.Name)
			}
		}
		return true
	})
	return out, ok
}

func cmdCompletion(ctx context.Context, x *IO, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(x.Err, "usage: ka completion bash|zsh|fish|powershell")
		return 2
	}
	script, ok := completionScripts[args[0]]
	if !ok {
		fmt.Fprintf(x.Err, "ka: no completion for %q; use bash, zsh, fish or powershell\n", args[0])
		return 2
	}
	fmt.Fprint(x.Out, script)
	return 0
}

var completionScripts = map[string]string{
	"bash": `# ka completion for bash. Load it with:
#   source <(ka completion bash)
# or save it to /etc/bash_completion.d/ka (or ~/.local/share/bash-completion/completions/ka).
_ka() {
  local cur="${COMP_WORDS[COMP_CWORD]}"
  local IFS=$'\n'
  COMPREPLY=($(ka __complete "${COMP_WORDS[@]:1:COMP_CWORD-1}" "$cur" 2>/dev/null))
}
complete -o default -F _ka ka
`,
	"zsh": `#compdef ka
# ka completion for zsh. Load it with:
#   source <(ka completion zsh)
# or save it as _ka in a directory on $fpath.
_ka() {
  local -a items
  items=("${(@f)$(ka __complete "${words[@]:1:CURRENT-2}" "${words[CURRENT]}" 2>/dev/null)}")
  if (( ${#items} )) && [[ -n "${items[1]}" ]]; then
    compadd -a items
  else
    _files
  fi
}
compdef _ka ka
`,
	"fish": `# ka completion for fish. Load it with:
#   ka completion fish | source
# or save it to ~/.config/fish/completions/ka.fish.
function __ka_complete
    set -l words (commandline -opc)
    set -e words[1]
    ka __complete $words (commandline -ct) 2>/dev/null
end
complete -c ka -f -a '(__ka_complete)'
`,
	"powershell": `# ka completion for PowerShell. Load it with:
#   ka completion powershell | Out-String | Invoke-Expression
# or add that line to your $PROFILE.
Register-ArgumentCompleter -Native -CommandName ka -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = @($commandAst.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.ToString() })
    if ($wordToComplete -ne '' -and $words.Count -gt 0) { $words = $words[0..($words.Count - 2)] }
    $words += $wordToComplete
    ka __complete @words 2>$null | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`,
}
