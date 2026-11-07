// Command ka is the KanArtist CLI. M0 ships the spec tooling only:
// `ka check`, `ka fmt` and `ka version`. The engine, MCP server and daemon
// arrive in M1/M2 (docs/prd.md §21).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

var version = "0.0.0-m0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "fmt":
		return cmdFmt(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "ka %s (store spec %d)\n", version, kaspec.SpecVersion)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "ka: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: ka <command> [flags]

commands:
  check [--strict] [--json] [--map URL=DIR]... [STORE_DIR...]
        validate one or more stores (a workspace) against the Store Spec
  fmt [--check] [STORE_DIR...]
        rewrite ticket and comment files in canonical form
  version
        print the version

With no STORE_DIR, ka uses the store containing the current directory.
`)
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

func storeDirs(args []string) ([]string, error) {
	if len(args) == 0 {
		d, err := findStore(".")
		if err != nil {
			return nil, err
		}
		return []string{d}, nil
	}
	var out []string
	for _, a := range args {
		abs, err := filepath.Abs(a)
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return out, nil
}

// newWorkspace loads the given store directories. Store keys are absolute
// paths, so `extends` URLs that are local paths resolve relative to the
// extending store, including parents that extend further parents.
func newWorkspace(dirs []string, maps mapFlag) *kaspec.Workspace {
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

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("check", flag.ContinueOnError)
	fl.SetOutput(stderr)
	strict := fl.Bool("strict", false, "fail on warnings too (use in CI)")
	asJSON := fl.Bool("json", false, "print diagnostics as JSON")
	nowFlag := fl.String("now", "", "evaluate stale claims as of this RFC 3339 time (default: now)")
	maps := mapFlag{}
	fl.Var(maps, "map", "resolve an extends URL to a local store directory (URL=DIR, repeatable)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	dirs, err := storeDirs(fl.Args())
	if err != nil {
		fmt.Fprintln(stderr, "ka:", err)
		return 2
	}
	now := time.Now().UTC()
	if *nowFlag != "" {
		if now, err = time.Parse(time.RFC3339, *nowFlag); err != nil {
			fmt.Fprintln(stderr, "ka: --now:", err)
			return 2
		}
	}
	ws := newWorkspace(dirs, maps)
	diags := kaspec.Check(ws, kaspec.CheckOptions{Now: now})

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if diags == nil {
			diags = []kaspec.Diagnostic{}
		}
		enc.Encode(diags)
	} else {
		counts := map[kaspec.Severity]int{}
		for _, d := range diags {
			counts[d.Severity]++
			fmt.Fprintln(stdout, d.String())
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
		fmt.Fprintf(stdout, "checked %d store(s), %d ticket(s), %d comment(s): %d error(s), %d warning(s), %d info\n",
			len(ws.Stores), tickets, comments, counts[kaspec.SevError], counts[kaspec.SevWarning], counts[kaspec.SevInfo])
	}
	if kaspec.HasErrors(diags, *strict) {
		return 1
	}
	return 0
}

func cmdFmt(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("fmt", flag.ContinueOnError)
	fl.SetOutput(stderr)
	checkOnly := fl.Bool("check", false, "list files that are not canonical; change nothing")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	dirs, err := storeDirs(fl.Args())
	if err != nil {
		fmt.Fprintln(stderr, "ka:", err)
		return 2
	}
	changed, skipped := 0, 0
	for _, root := range dirs {
		err := filepath.WalkDir(filepath.Join(root, "projects"), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			parts := strings.Split(rel, "/")
			// projects/<key>/tickets/<ID>/ticket.md or .../comments/<file>.md
			isTicket := len(parts) == 5 && parts[2] == "tickets" && parts[4] == "ticket.md"
			isComment := len(parts) == 6 && parts[2] == "tickets" && parts[4] == "comments"
			if !isTicket && !isComment {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			norm := kaspec.NormalizeEncoding(data)
			var out []byte
			var diags []kaspec.Diagnostic
			if isTicket {
				var t *kaspec.Ticket
				t, diags = kaspec.ParseTicket(rel, norm)
				if t != nil {
					out = kaspec.FormatTicket(t)
				}
			} else {
				var c *kaspec.Comment
				c, diags = kaspec.ParseComment(rel, norm)
				if c != nil {
					out = kaspec.FormatComment(c)
				}
			}
			if kaspec.HasErrors(diags, false) || out == nil {
				fmt.Fprintf(stderr, "skip %s: has errors (run ka check)\n", rel)
				skipped++
				return nil
			}
			if string(out) == string(data) {
				return nil
			}
			changed++
			if *checkOnly {
				fmt.Fprintln(stdout, rel)
				return nil
			}
			fmt.Fprintln(stdout, "formatted", rel)
			return os.WriteFile(p, out, 0o644)
		})
		if err != nil {
			fmt.Fprintln(stderr, "ka:", err)
			return 1
		}
	}
	if *checkOnly && changed > 0 {
		return 1
	}
	if skipped > 0 {
		return 1
	}
	return 0
}
