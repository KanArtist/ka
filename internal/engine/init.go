package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/KanArtist/ka/internal/gitx"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// MergeDriverCommand is the command registered as git's merge.ka.driver,
// e.g. `"/usr/local/bin/ka" merge-driver %O %A %B %P`. The CLI sets it from
// its own executable path; when empty, ka does not register a driver.
var MergeDriverCommand string

// Gitattributes is what ka init writes (and ka doctor expects).
const Gitattributes = "* text=auto eol=lf\nticket.md merge=ka\n"

// RegisterMergeDriver records the semantic merge driver in a repository's config.
func RegisterMergeDriver(ctx context.Context, g *gitx.Repo) error {
	if MergeDriverCommand == "" {
		return nil
	}
	if cur, _ := g.Run(ctx, "config", "--local", "merge.ka.driver"); cur == MergeDriverCommand {
		return nil
	}
	if _, err := g.Run(ctx, "config", "--local", "merge.ka.name", "KanArtist semantic ticket merge"); err != nil {
		return err
	}
	_, err := g.Run(ctx, "config", "--local", "merge.ka.driver", MergeDriverCommand)
	return err
}

// InitOptions configures ka init.
type InitOptions struct {
	Dir     string
	Name    string // store name (default: directory name)
	Project string // optional first project key
	Prefix  string // its ID prefix (default derived from the key)
	Extends string // optional parent store URL
}

// InitStore creates a store (and git repo, if needed) in Dir. Existing files
// are left alone. It returns the files it created.
func InitStore(ctx context.Context, o InitOptions) ([]string, error) {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := gitx.Toplevel(ctx, dir); err != nil {
		g := &gitx.Repo{Dir: dir}
		if _, err := g.Run(ctx, "init", "-q", "-b", "main"); err != nil {
			return nil, err
		}
	}
	name := o.Name
	if name == "" {
		name = strings.Trim(nonName.ReplaceAllString(strings.ToLower(filepath.Base(dir)), "-"), "-")
	}
	var created []string
	write := func(rel, content string) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
		created = append(created, rel)
		return nil
	}
	storeYML := []kaspec.KV{{K: "spec", V: kaspec.Raw("1")}, {K: "name", V: name}}
	if o.Extends != "" {
		storeYML = append(storeYML, kaspec.KV{K: "extends", V: []any{[]kaspec.KV{{K: "url", V: o.Extends}}}})
	}
	if err := write(".ka/store.yml", kaspec.EmitYAML(storeYML)); err != nil {
		return nil, err
	}
	if err := write(".gitattributes", Gitattributes); err != nil {
		return nil, err
	}
	if o.Project != "" {
		if !kaspec.ProjectKeyRe.MatchString(o.Project) {
			return nil, Errf(CodeInvalid, "project keys are lower-case letters, digits and dashes, e.g. jk", "invalid project key %q", o.Project)
		}
		prefix := o.Prefix
		if prefix == "" {
			prefix = kaspec.DefaultPrefix(o.Project)
		}
		if !kaspec.PrefixRe.MatchString(prefix) {
			return nil, Errf(CodeInvalid, "prefixes are upper-case letters and digits, e.g. JK", "invalid prefix %q", prefix)
		}
		if err := write("projects/"+o.Project+"/project.yml", kaspec.EmitYAML([]kaspec.KV{{K: "key", V: o.Project}, {K: "prefix", V: prefix}, {K: "name", V: o.Project}})); err != nil {
			return nil, err
		}
	}
	g := &gitx.Repo{Dir: dir}
	RegisterMergeDriver(ctx, g)
	return created, nil
}

// AgentsMD is the hand-writing protocol for AGENTS.md (PRD §7.12).
func AgentsMD(project, prefix string) string {
	if prefix == "" {
		prefix = "PREFIX"
	}
	if project == "" {
		project = "<key>"
	}
	return fmt.Sprintf(`## Tickets (KanArtist)

Tickets live in this store under `+"`projects/%[1]s/tickets/<ID>/`"+`. Prefer the `+"`ka`"+` MCP tools or CLI
(`+"`ka next`, `ka show <id>`, `ka move <id> done -m \"…\"`"+`). Refer to tickets by the reference `+"`ka`"+`
prints, the ID plus a slug (`+"`%[2]s-7QK3M-compiler-warnings`"+`), in commits, PRs and messages.
Without `+"`ka`"+`, follow this protocol:

1. **New ticket:** pick 5 random characters from `+"`0123456789ABCDEFGHJKMNPQRSTVWXYZ`"+`, form
   `+"`%[2]s-<SUFFIX>`"+`, and check that `+"`projects/%[1]s/tickets/<ID>/`"+` does not exist. Write
   `+"`ticket.md`"+` with at least `+"`id`, `title`, `status`, `created` and `updated`"+` (`+"`ka template`"+` prints one).
2. **Comment:** add a new file under `+"`comments/`"+` named `+"`<yyyymmddThhmmssZ>-<4 random chars>.md`"+`
   (UTC). Never edit an existing comment.
3. **Claim:** set `+"`claim: {by, at}`"+` and `+"`assignee`"+`, commit, and push *before* starting work. If the push
   is rejected, `+"`git pull --rebase`"+` and check whether someone else claimed it first.
4. Always `+"`git pull --rebase`"+`; never create merge commits.
5. Run `+"`ka check`"+` before pushing if you can; `+"`ka fix`"+` repairs formatting.
`, project, prefix)
}

// Template is a blank ticket for hand-writing (ka template).
func Template(prefix, status, now string) string {
	if prefix == "" {
		prefix = "PREFIX"
	}
	if status == "" {
		status = "backlog"
	}
	id := kaspec.NewID(prefix, 5)
	return fmt.Sprintf("---\nid: %s\ntitle: One-line summary of the work\nstatus: %s\npriority: P2\nkind: feature\nacceptance:\n  - text: An observable outcome that proves this is done\n    done: false\ncreated: %s\nupdated: %s\n---\n\n## Problem\n\nWhat is wrong or missing, and why it matters.\n\n## Approach\n\nHow you intend to solve it.\n", id, status, now, now)
}
