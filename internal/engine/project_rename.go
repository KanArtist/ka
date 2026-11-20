package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/KanArtist/ka/pkg/kaspec"
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

// renamePlan is what a rename changes; it does not depend on the tickets.
type renamePlan struct {
	key, newKey, oldPrefix, newPrefix string
	keepFormer                        bool
	home                              *Store
	idRe, keyRe, defRe                *regexp.Regexp
}

// RenameProject gives a project a new key and/or ID prefix (PRD §9.2).
// Every ticket keeps its suffix (KA-7QK3M becomes KAN-7QK3M); its
// directory and id change, and every reference to it in every writable
// store is rewritten: parent, blocked_by, related and the rest, mentions
// in descriptions, comments and docs, and project:<key> in saved boards
// and default_project. One commit per store, the home store pushed first.
// keepFormer records the old prefix in former_prefixes, so old IDs keep
// resolving during a transition.
//
// Every writable store is locked and synced before anything is read. The
// rewrite is idempotent: if a push is rejected because the remote moved,
// the commits not yet pushed are undone and the rename is redone on top of
// upstream, so tickets that arrived meanwhile are renamed too.
func (s *Service) RenameProject(ctx context.Context, c Caller, key, newKey, newPrefix string, keepFormer, dryRun bool) (*RenameProjectOut, error) {
	s.ix.catchUp()
	p, err := s.planRename(key, newKey, newPrefix, keepFormer)
	if err != nil {
		return nil, err
	}
	out := &RenameProjectOut{From: key, To: p.newKey, OldPrefix: p.oldPrefix, NewPrefix: p.newPrefix,
		Files: map[string]int{}, Commits: map[string]string{}, KeptOld: p.keepFormer, DryRun: dryRun}
	var stores []*Store
	for _, st := range s.WS.Stores {
		if st.ReadOnly {
			out.Skipped = append(out.Skipped, st.Name)
		} else {
			stores = append(stores, st)
		}
	}
	sort.Slice(stores, func(i, j int) bool { return stores[i].Name < stores[j].Name })
	// Fetch every store before taking any lock; the first attempt
	// integrates that. Then every writable store's write mutex, in name order.
	var pre map[*Store]*fetchedUp
	if !dryRun {
		pre = s.prefetch(ctx, stores)
	}
	hm, err := s.hold(ctx, stores...)
	if err != nil {
		return nil, err
	}
	defer hm.unlock()
	s.ix.catchUp()
	if p, err = s.planRename(key, newKey, newPrefix, keepFormer); err != nil {
		return nil, err
	}
	if dryRun {
		ids, files, err := renameChanges(p, stores, nil)
		if err != nil {
			return nil, err
		}
		out.IDs, out.Tickets, out.Files = ids, len(ids), files
		return out, nil
	}

	unlock, err := lockStores(ctx, stores)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// The home store is pushed first, so references never name IDs that
	// are not published yet.
	order := []*Store{p.home}
	for _, st := range stores {
		if st != p.home {
			order = append(order, st)
		}
	}
	for attempt := 1; ; attempt++ {
		if err := s.syncHeld(ctx, order, pre); err != nil {
			return nil, err
		}
		pre = nil // later attempts fetch again, under the locks
		if attempt == 1 {
			// Check again on the latest state (B16).
			if p, err = s.planRename(key, newKey, newPrefix, keepFormer); err != nil {
				return nil, err
			}
		}
		js := map[*Store]*journal{}
		for _, st := range stores {
			js[st] = newJournal(ctx, st)
		}
		undo := func(from int) error {
			var errs []error
			for _, st := range order[from:] {
				errs = append(errs, js[st].undo(ctx))
			}
			s.ix.refresh(nil, nil)
			return errors.Join(errs...)
		}
		ids, files, err := renameChanges(p, stores, js)
		if err == nil {
			err = s.commitRename(ctx, c, p, order, js, out, len(ids))
		}
		if err != nil {
			return nil, errors.Join(err, undo(0))
		}
		s.ix.refresh(nil, nil)
		if out.IDs == nil {
			out.IDs = map[string]string{}
		}
		for k, v := range ids {
			out.IDs[k] = v
		}
		out.Tickets = len(out.IDs)
		for k, n := range files {
			out.Files[k] += n
		}

		var committed []*Store
		for _, st := range order {
			if out.Commits[st.Name] != "" {
				committed = append(committed, st)
			}
		}
		pushed, rejected, why := s.pushChain(ctx, committed)
		out.Pushed = pushed
		if rejected == nil {
			out.PushError = why
			return out, nil
		}
		from := 0
		for i, st := range order {
			if st == rejected {
				from = i
			}
		}
		if attempt == moveAttempts {
			if rejected == p.home {
				if err := undo(0); err != nil {
					return nil, err
				}
				return nil, Errf(CodeGit, "retry the rename once the stores are quieter", "the remote of %s kept changing while the rename was pushed; nothing was renamed", p.home.Name)
			}
			out.PushError = rejected.Name + ": the remote kept changing; its reference updates are committed locally and the next ka sync publishes them"
			return out, nil
		}
		if err := undo(from); err != nil {
			return nil, err
		}
		for _, st := range order[from:] {
			delete(out.Commits, st.Name)
			delete(out.Files, st.Name)
		}
	}
}

// planRename checks a rename against the index.
func (s *Service) planRename(key, newKey, newPrefix string, keepFormer bool) (*renamePlan, error) {
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	pi := s.ix.projects[key]
	if pi == nil {
		return nil, Errf(CodeUnknownProject, "projects: "+strings.Join(sortedKeys(s.ix.projects), ", "), "no project %q", key)
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
	p := &renamePlan{key: key, newKey: newKey, oldPrefix: pi.Prefix, newPrefix: newPrefix, home: pi.Store,
		keepFormer: keepFormer && newPrefix != pi.Prefix}
	switch {
	case !kaspec.ProjectKeyRe.MatchString(newKey):
		return nil, Errf(CodeInvalid, "project keys are lower-case letters, digits and -, starting with a letter", "invalid project key %q", newKey)
	case !kaspec.PrefixRe.MatchString(newPrefix):
		return nil, Errf(CodeInvalid, "prefixes are upper-case letters and digits, starting with a letter, at most 10", "invalid prefix %q", newPrefix)
	case newKey == key && newPrefix == p.oldPrefix:
		return nil, Errf(CodeInvalid, "pass --key, --prefix or both", "nothing to rename")
	case newKey != key && s.ix.projects[newKey] != nil:
		return nil, Errf(CodeConflict, "", "project %q already exists", newKey)
	case newPrefix != p.oldPrefix && s.ix.prefixes[newPrefix] != nil && s.ix.prefixes[newPrefix] != pi:
		return nil, Errf(CodeConflict, "", "prefix %s is already used by project %s", newPrefix, s.ix.prefixes[newPrefix].Key)
	case p.home.ReadOnly:
		return nil, Errf(CodeReadOnly, "add the store to your workspace to write to it", "store %s is read-only here", p.home.Name)
	case pi.File.MovedToStore != "":
		return nil, Errf(CodeInvalid, "rename it in the store it moved to", "project %s moved to %s", key, pi.File.MovedToStore)
	case pi.Dir != "projects/"+key:
		return nil, Errf(CodeInvalid, "", "project %s lives in %s, not projects/%s", key, pi.Dir, key)
	}
	p.idRe = regexp.MustCompile(`\b` + regexp.QuoteMeta(p.oldPrefix) + `-[0-9A-Z]+\b`)
	// project:<key> as a whole value: project:jk, not project:jk-web.
	p.keyRe = regexp.MustCompile(`\bproject:` + regexp.QuoteMeta(key) + `([^a-z0-9_-]|$)`)
	p.defRe = regexp.MustCompile(`(?m)^(\s*default_project:\s*)` + regexp.QuoteMeta(key) + `\s*$`)
	return p, nil
}

// renameChanges computes a rename from the files as they are now and, with
// journals, applies it. It is idempotent: on a tree that is already renamed
// it changes nothing, and on one where tickets arrived in the old project
// since, it renames just those. It returns the ID map (old ID to new, for
// every ticket of the project) and the files changed per store.
func renameChanges(p *renamePlan, stores []*Store, js map[*Store]*journal) (map[string]string, map[string]int, error) {
	oldRel, newRel := "projects/"+p.key, "projects/"+p.newKey
	type file struct{ from, to string }
	var moves []file
	ids := map[string]string{}
	for i, rel := range []string{oldRel, newRel} {
		if i == 1 && newRel == oldRel {
			continue
		}
		names, err := regularFiles(p.home.Root, rel)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range names {
			parts := strings.Split(f, "/")
			if len(parts) >= 3 && parts[0] == "tickets" {
				if pfx, sfx, ok := kaspec.ParseID(parts[1]); ok && (pfx == p.oldPrefix || pfx == p.newPrefix) {
					ids[p.oldPrefix+"-"+sfx] = p.newPrefix + "-" + sfx
					parts[1] = p.newPrefix + "-" + sfx
				}
			}
			moves = append(moves, file{rel + "/" + f, newRel + "/" + strings.Join(parts, "/")})
		}
	}
	files := map[string]int{}
	rewrite := func(rel string, data []byte) []byte {
		ext := strings.ToLower(filepath.Ext(rel))
		if ext != ".md" && ext != ".yml" && ext != ".yaml" {
			return data
		}
		text := p.idRe.ReplaceAllStringFunc(string(data), func(m string) string {
			if nid, ok := ids[m]; ok {
				return nid
			}
			return m
		})
		if p.newKey != p.key && ext != ".md" {
			text = p.keyRe.ReplaceAllString(text, "project:"+p.newKey+"${1}")
			text = p.defRe.ReplaceAllString(text, "${1}"+p.newKey)
		}
		return []byte(text)
	}
	write := func(st *Store, rel string, data []byte) error {
		files[st.Name]++
		if js == nil {
			return nil
		}
		return js[st].write(rel, data)
	}

	// 1. The project and its tickets, in the home store.
	seen, froms := map[string]bool{}, map[string]bool{}
	for _, m := range moves {
		if seen[m.to] {
			return nil, nil, Errf(CodeConflict, "", "%s and another file both become %s", m.from, m.to)
		}
		seen[m.to], froms[m.from] = true, true
	}
	for _, m := range moves {
		data, err := readRegular(filepath.Join(p.home.Root, filepath.FromSlash(m.from)))
		if err != nil {
			return nil, nil, err
		}
		nd := rewrite(m.to, data)
		if m.to == newRel+"/project.yml" {
			nd = rewriteProjectYAML(nd, p.newKey, p.newPrefix, p.oldPrefix, p.keepFormer)
		}
		if m.from != m.to && !froms[m.to] {
			if _, err := os.Lstat(filepath.Join(p.home.Root, filepath.FromSlash(m.to))); err == nil {
				return nil, nil, Errf(CodeConflict, "remove or rename it first", "%s already has %s", p.home.Name, m.to)
			}
		}
		if m.from == m.to && string(nd) == string(data) {
			continue
		}
		if string(nd) != string(data) {
			files[p.home.Name]++
		}
		if js != nil {
			if err := js[p.home].write(m.to, nd); err != nil {
				return nil, nil, err
			}
		}
		if m.from != m.to && js != nil {
			if err := js[p.home].remove(m.from); err != nil {
				return nil, nil, err
			}
		}
	}

	// 2. References in every writable store.
	for _, st := range stores {
		err := filepath.WalkDir(st.Root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(st.Root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if d.Name() == ".git" || (st == p.home && (rel == oldRel || rel == newRel)) {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil // never rewrite through a link
			}
			data, err := readRegular(path)
			if err != nil {
				return nil
			}
			if nd := rewrite(rel, data); string(nd) != string(data) {
				return write(st, rel, nd)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		if _, ok := files[st.Name]; !ok && st == p.home {
			files[st.Name] = 0
		}
	}
	return ids, files, nil
}

// commitRename makes one commit per store that changed.
func (s *Service) commitRename(ctx context.Context, c Caller, p *renamePlan, order []*Store, js map[*Store]*journal, out *RenameProjectOut, tickets int) error {
	what := "ka: rename project " + p.key
	if p.newKey != p.key {
		what += " to " + p.newKey
	}
	if p.newPrefix != p.oldPrefix {
		what += " (" + p.oldPrefix + "- to " + p.newPrefix + "-, " + strconv.Itoa(tickets) + " tickets)"
	}
	extra := []string{"Ka-Renamed-Project: " + p.key + " " + p.newKey + " " + p.oldPrefix + " " + p.newPrefix}
	for _, st := range order {
		j := js[st]
		if len(j.paths) == 0 {
			continue
		}
		msg := what
		paths := j.paths
		if st != p.home {
			msg = "ka: references to project " + p.key + " follow its rename"
		} else {
			// The project directories as a whole, plus anything else
			// (docs, boards): keeps the command line short.
			paths = []string{"projects/" + p.key, "projects/" + p.newKey}
			for _, rel := range j.paths {
				if !strings.HasPrefix(rel, paths[0]+"/") && !strings.HasPrefix(rel, paths[1]+"/") {
					paths = append(paths, rel)
				}
			}
		}
		sha, err := j.commit(ctx, c, msg, extra, paths)
		if err != nil {
			return err
		}
		if sha != "" {
			out.Commits[st.Name] = short(sha)
		}
	}
	return nil
}

// rewriteProjectYAML sets key and prefix in project.yml, and adds the old
// prefix to former_prefixes (block or flow style) when asked; other lines
// stay as they are. It is idempotent.
func rewriteProjectYAML(data []byte, key, prefix, oldPrefix string, keepFormer bool) []byte {
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
			v := strings.TrimSpace(strings.TrimPrefix(l, "former_prefixes:"))
			if !keepFormer {
				out = append(out, l)
				break
			}
			if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
				var items []string
				for _, it := range strings.Split(strings.Trim(v, "[]"), ",") {
					if it = strings.Trim(strings.TrimSpace(it), `"'`); it != "" {
						items = append(items, it)
					}
				}
				if !slices.Contains(items, oldPrefix) {
					items = append(items, oldPrefix)
				}
				out = append(out, "former_prefixes: ["+strings.Join(items, ", ")+"]")
				break
			}
			out = append(out, l)
			// a block list follows; append to it
			j, has := i+1, false
			for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "- ") {
				out = append(out, lines[j])
				has = has || strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[j]), "- ")), `"'`) == oldPrefix
				j++
			}
			if !has {
				out = append(out, "  - "+oldPrefix)
			}
			i = j - 1
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
	return []byte(strings.Join(out, "\n") + "\n")
}
