package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// RenameProjectOut reports a project rename.
type RenameProjectOut struct {
	From      string            `json:"from" doc:"The old project key."`
	To        string            `json:"to" doc:"The new project key."`
	OldPrefix string            `json:"old_prefix"`
	NewPrefix string            `json:"new_prefix"`
	Tickets   int               `json:"tickets"`
	IDs       map[string]string `json:"ids,omitempty" doc:"Old ID to new ID."`
	Files     map[string]int    `json:"files,omitempty" doc:"Per store, the files rewritten (references and keys)."`
	Commits   map[string]string `json:"commits,omitempty"`
	Pushed    []string          `json:"pushed,omitempty"`
	PushError string            `json:"push_error,omitempty"`
	Skipped   []string          `json:"skipped,omitempty" doc:"Read-only stores whose references were left as they are."`
	KeptOld   bool              `json:"kept_former_prefix,omitempty"`
	DryRun    bool              `json:"dry_run,omitempty"`
}

// RenameProject gives a project a new key and/or ID prefix (PRD §9.2).
// Every ticket keeps its suffix (KA-7QK3M becomes KAN-7QK3M); its
// directory and id change, and every reference to it in every writable
// store is rewritten: parent, blocked_by, related and the rest, mentions
// in descriptions, comments and docs, and project:<key> in saved boards
// and default_project. One commit per store, the home store pushed first.
// keepFormer records the old prefix in former_prefixes, so old IDs keep
// resolving during a transition.
func (s *Service) RenameProject(ctx context.Context, c Caller, key, newKey, newPrefix string, keepFormer, dryRun bool) (*RenameProjectOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()

	s.ix.mu.RLock()
	pi := s.ix.projects[key]
	if pi == nil {
		s.ix.mu.RUnlock()
		return nil, Errf(CodeUnknownProject, "projects: "+strings.Join(s.Projects(), ", "), "no project %q", key)
	}
	if newKey == "" {
		newKey = key
	}
	if newPrefix == "" {
		newPrefix = pi.Prefix
		if newKey != key {
			newPrefix = kaspec.DefaultPrefix(newKey)
		}
	}
	oldPrefix, home := pi.Prefix, pi.Store
	var err error
	switch {
	case !kaspec.ProjectKeyRe.MatchString(newKey):
		err = Errf(CodeInvalid, "project keys are lower-case letters, digits and -, starting with a letter", "invalid project key %q", newKey)
	case !kaspec.PrefixRe.MatchString(newPrefix):
		err = Errf(CodeInvalid, "prefixes are upper-case letters and digits, starting with a letter, at most 10", "invalid prefix %q", newPrefix)
	case newKey == key && newPrefix == oldPrefix:
		err = Errf(CodeInvalid, "pass --key, --prefix or both", "nothing to rename")
	case newKey != key && s.ix.projects[newKey] != nil:
		err = Errf(CodeConflict, "", "project %q already exists", newKey)
	case newPrefix != oldPrefix && s.ix.prefixes[newPrefix] != nil && s.ix.prefixes[newPrefix] != pi:
		err = Errf(CodeConflict, "", "prefix %s is already used by project %s", newPrefix, s.ix.prefixes[newPrefix].Key)
	case home.ReadOnly:
		err = Errf(CodeReadOnly, "add the store to your workspace to write to it", "store %s is read-only here", home.Name)
	case pi.File.MovedToStore != "":
		err = Errf(CodeInvalid, "rename it in the store it moved to", "project %s moved to %s", key, pi.File.MovedToStore)
	}
	ids := map[string]string{}
	if err == nil {
		for id, r := range s.ix.tickets {
			if r.Project == pi {
				_, sfx, _ := kaspec.ParseID(id)
				ids[id] = newPrefix + "-" + sfx
			}
		}
	}
	s.ix.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	out := &RenameProjectOut{From: key, To: newKey, OldPrefix: oldPrefix, NewPrefix: newPrefix, Tickets: len(ids),
		IDs: ids, Files: map[string]int{}, Commits: map[string]string{}, KeptOld: keepFormer && newPrefix != oldPrefix, DryRun: dryRun}

	// Start from the latest of every store, so no reference is missed.
	if !s.NoPush && !dryRun {
		for _, st := range s.WS.Stores {
			if remote, _ := st.upstream(ctx); remote != "" && !st.ReadOnly {
				if _, err := st.Sync(ctx, s.hooks(st)); err != nil {
					return nil, err
				}
			}
		}
		s.ix.refresh(nil, nil)
	}

	stores := append([]*Store(nil), s.WS.Stores...)
	sort.Slice(stores, func(i, j int) bool { return stores[i].Name < stores[j].Name })
	var unlocks []func()
	unlock := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
		unlocks = nil
	}
	defer unlock()
	if !dryRun {
		for _, st := range stores {
			if st.ReadOnly {
				continue
			}
			u, err := st.lock(ctx)
			if err != nil {
				return nil, err
			}
			unlocks = append(unlocks, u)
		}
	}

	// 1. Move the project and its tickets in the home store.
	oldRel, newRel := pi.Dir, "projects/"+newKey
	oldDir := filepath.Join(home.Root, filepath.FromSlash(oldRel))
	newDir := filepath.Join(home.Root, filepath.FromSlash(newRel))
	if newKey != key {
		if _, err := os.Stat(newDir); err == nil {
			return nil, Errf(CodeConflict, "remove or rename it first", "%s already has %s", home.Name, newRel)
		}
	}
	if !dryRun {
		if newKey != key {
			if err := os.Rename(oldDir, newDir); err != nil {
				return nil, err
			}
		}
		for oldID, newID := range ids {
			if oldID == newID {
				continue
			}
			from := filepath.Join(newDir, "tickets", oldID)
			to := filepath.Join(newDir, "tickets", newID)
			if _, err := os.Stat(to); err == nil {
				return nil, Errf(CodeConflict, "", "%s/tickets/%s already exists", newRel, newID)
			}
			if err := os.Rename(from, to); err != nil {
				return nil, err
			}
		}
		if err := rewriteProjectFile(filepath.Join(newDir, "project.yml"), newKey, newPrefix, oldPrefix, out.KeptOld); err != nil {
			return nil, err
		}
	}

	// 2. Rewrite references in every writable store.
	idRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(oldPrefix) + `-[0-9A-Z]+\b`)
	keyRe := regexp.MustCompile(`\bproject:` + regexp.QuoteMeta(key) + `\b`)
	defRe := regexp.MustCompile(`(?m)^(\s*default_project:\s*)` + regexp.QuoteMeta(key) + `\s*$`)
	for _, st := range stores {
		if st.ReadOnly {
			out.Skipped = append(out.Skipped, st.Name)
			continue
		}
		n := 0
		filepath.WalkDir(st.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".md", ".yml", ".yaml":
			default:
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			text := idRe.ReplaceAllStringFunc(string(data), func(m string) string {
				if nid, ok := ids[m]; ok {
					return nid
				}
				return m
			})
			if ext := strings.ToLower(filepath.Ext(p)); newKey != key && (ext == ".yml" || ext == ".yaml") {
				text = keyRe.ReplaceAllString(text, "project:"+newKey)
				text = defRe.ReplaceAllString(text, "${1}"+newKey)
			}
			if text != string(data) {
				n++
				if !dryRun {
					fi, _ := os.Stat(p)
					os.WriteFile(p, []byte(text), fi.Mode().Perm())
				}
			}
			return nil
		})
		if n > 0 || st == home {
			out.Files[st.Name] = n
		}
	}
	if dryRun {
		return out, nil
	}

	// 3. One commit per store that changed.
	trail := "\n\nKa-Actor: " + c.Actor + "\n"
	if c.OnBehalfOf != "" {
		trail += "Ka-On-Behalf-Of: " + c.OnBehalfOf + "\n"
	}
	trail += "Ka-Spec: 1\n"
	what := "ka: rename project " + key
	if newKey != key {
		what += " to " + newKey
	}
	if newPrefix != oldPrefix {
		what += " (" + oldPrefix + "- to " + newPrefix + "-, " + strconv.Itoa(len(ids)) + " tickets)"
	}
	for _, st := range stores {
		if _, ok := out.Files[st.Name]; !ok {
			continue
		}
		if _, err := st.git.Run(ctx, "add", "-A", "--", st.repoPath(".")); err != nil {
			return nil, err
		}
		if _, err := st.git.Run(ctx, "diff", "--cached", "--quiet"); err == nil { // nothing staged
			delete(out.Files, st.Name)
			continue
		}
		msg := what
		if st != home {
			msg = "ka: references to project " + key + " follow its rename"
		}
		if _, err := st.git.RunRaw(ctx, []byte(msg+trail+"Ka-Renamed-Project: "+key+" "+newKey+" "+oldPrefix+" "+newPrefix+"\n"), "commit", "-q", "--no-verify", "-F", "-"); err != nil {
			return nil, err
		}
		out.Commits[st.Name] = short(st.git.Head(ctx))
	}
	unlock()
	s.ix.refresh(nil, nil)

	// 4. Push the home store first, then the rest.
	if !s.NoPush {
		order := []*Store{home}
		for _, st := range stores {
			if st != home {
				order = append(order, st)
			}
		}
		for _, st := range order {
			if out.Commits[st.Name] == "" {
				continue
			}
			remote, _ := st.upstream(ctx)
			if remote == "" || st.PushMode == "manual" {
				continue
			}
			if _, err := st.Sync(ctx, s.hooks(st)); err != nil {
				out.PushError = st.Name + ": " + err.Error()
				break
			}
			out.Pushed = append(out.Pushed, st.Name)
		}
	}
	return out, nil
}

// rewriteProjectFile sets key and prefix in project.yml, and adds the old
// prefix to former_prefixes when asked; other lines stay as they are.
func rewriteProjectFile(p, key, prefix, oldPrefix string, keepFormer bool) error {
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out []string
	sawPrefix, sawFormer := false, false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "key:"):
			out = append(out, "key: "+key)
		case strings.HasPrefix(l, "prefix:"):
			out = append(out, "prefix: "+prefix)
			sawPrefix = true
		case strings.HasPrefix(l, "former_prefixes:"):
			sawFormer = true
			out = append(out, l)
			if keepFormer && strings.TrimSpace(strings.TrimPrefix(l, "former_prefixes:")) == "" {
				// a block list follows; append to it
				j := i + 1
				for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "- ") {
					out = append(out, lines[j])
					j++
				}
				out = append(out, "  - "+oldPrefix)
				i = j - 1
			}
		default:
			out = append(out, l)
		}
	}
	if !sawPrefix {
		out = append(out, "prefix: "+prefix)
	}
	if keepFormer && !sawFormer {
		out = append(out, "former_prefixes:", "  - "+oldPrefix)
	}
	return os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}
