package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// MoveProjectOut reports a project move.
type MoveProjectOut struct {
	Project    string   `json:"project"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Files      int      `json:"files"`
	Tickets    int      `json:"tickets"`
	ToCommit   string   `json:"to_commit,omitempty"`
	FromCommit string   `json:"from_commit,omitempty"`
	Pushed     []string `json:"pushed,omitempty" doc:"Stores whose commit was pushed."`
	PushError  string   `json:"push_error,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
}

// MoveProject moves a project to another store in the workspace (PRD §9.2):
// it copies projects/<key>/ into the target with one commit, then replaces
// it in the source with a project.yml tombstone (moved_to_store) in a
// second commit. Ticket IDs and paths do not change, so references from
// other stores still resolve. The target is pushed first; the source is
// pushed only once the target has the project.
func (s *Service) MoveProject(ctx context.Context, c Caller, key, to string, dryRun bool) (*MoveProjectOut, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.ix.catchUp()
	s.ix.mu.RLock()
	pi := s.ix.projects[key]
	var tickets int
	for _, r := range s.ix.tickets {
		if r.Project == pi {
			tickets++
		}
	}
	s.ix.mu.RUnlock()
	if pi == nil {
		return nil, Errf(CodeUnknownProject, "projects: "+strings.Join(s.Projects(), ", "), "no project %q", key)
	}
	src := pi.Store
	var dst *Store
	for _, st := range s.WS.Stores {
		if st.Name == to || (st.URL != "" && sameURL(st.URL, to)) {
			dst = st
		}
	}
	switch {
	case dst == nil:
		var names []string
		for _, st := range s.WS.Stores {
			names = append(names, st.Name)
		}
		return nil, Errf(CodeInvalid, "stores: "+strings.Join(names, ", ")+" (add the target with ka store add)", "no store %q in the workspace", to)
	case dst == src:
		return nil, Errf(CodeInvalid, "", "project %s already lives in %s", key, src.Name)
	case src.ReadOnly || dst.ReadOnly:
		return nil, Errf(CodeReadOnly, "add both stores to your workspace to write to them", "store %s is read-only here", map[bool]string{true: src.Name, false: dst.Name}[src.ReadOnly])
	}
	rel := pi.Dir // projects/<key>
	target := filepath.Join(dst.Root, filepath.FromSlash(rel))
	if _, err := os.Stat(target); err == nil {
		data, _ := os.ReadFile(filepath.Join(target, "project.yml"))
		if !strings.Contains(string(data), "moved_to_store:") {
			return nil, Errf(CodeConflict, "remove or rename it first", "%s already has %s", dst.Name, rel)
		}
	}

	// Start from the latest of both stores so the copy is complete.
	if !s.NoPush && !dryRun {
		for _, st := range []*Store{dst, src} {
			if remote, _ := st.upstream(ctx); remote != "" {
				if _, err := st.Sync(ctx, s.hooks(st)); err != nil {
					return nil, err
				}
			}
		}
		s.ix.refresh(nil, nil)
	}

	var files []string
	srcDir := filepath.Join(src.Root, filepath.FromSlash(rel))
	filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			r, _ := filepath.Rel(srcDir, p)
			files = append(files, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(files)
	out := &MoveProjectOut{Project: key, From: src.Name, To: dst.Name, Files: len(files), Tickets: tickets, DryRun: dryRun}
	if dryRun {
		return out, nil
	}

	// Lock both stores in a fixed order.
	first, second := src, dst
	if dst.Name < src.Name {
		first, second = dst, src
	}
	u1, err := first.lock(ctx)
	if err != nil {
		return nil, err
	}
	u2, err := second.lock(ctx)
	if err != nil {
		u1()
		return nil, err
	}
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			u2()
			u1()
		}
	}
	defer unlock()

	trail := "\n\nKa-Actor: " + c.Actor + "\n"
	if c.OnBehalfOf != "" {
		trail += "Ka-On-Behalf-Of: " + c.OnBehalfOf + "\n"
	}
	trail += "Ka-Spec: 1\n"

	// 1. The target gets the project.
	os.RemoveAll(target)
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(f)))
		if err != nil {
			return nil, err
		}
		p := filepath.Join(target, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, err
		}
	}
	if _, err := dst.git.Run(ctx, "add", "-A", "--", dst.repoPath(rel)); err != nil {
		return nil, err
	}
	msg := "ka: move project " + key + " from " + src.Name + " (" + strconv.Itoa(tickets) + " tickets)" + trail + "Ka-Moved-From: " + storeRef(src) + "\n"
	if _, err := dst.git.RunRaw(ctx, []byte(msg), "commit", "-q", "--no-verify", "-F", "-"); err != nil {
		return nil, err
	}
	out.ToCommit = short(dst.git.Head(ctx))

	// 2. The source keeps a tombstone.
	if _, err := src.git.Run(ctx, "rm", "-r", "-q", "--cached", "--ignore-unmatch", "--", src.repoPath(rel)); err != nil {
		return nil, err
	}
	os.RemoveAll(srcDir)
	tomb := kaspec.EmitYAML([]kaspec.KV{{K: "key", V: key}, {K: "prefix", V: pi.Prefix}, {K: "moved_to_store", V: storeRef(dst)}})
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(srcDir, "project.yml"), []byte(tomb), 0o644); err != nil {
		return nil, err
	}
	if _, err := src.git.Run(ctx, "add", "-A", "--", src.repoPath(rel)); err != nil {
		return nil, err
	}
	msg = "ka: move project " + key + " to " + dst.Name + trail + "Ka-Moved-To: " + storeRef(dst) + "\n"
	if _, err := src.git.RunRaw(ctx, []byte(msg), "commit", "-q", "--no-verify", "-F", "-"); err != nil {
		return nil, err
	}
	out.FromCommit = short(src.git.Head(ctx))
	unlock()
	s.ix.refresh(nil, nil)

	// 3. Push the target, then the source.
	if !s.NoPush {
		for _, st := range []*Store{dst, src} {
			remote, _ := st.upstream(ctx)
			if remote == "" || st.PushMode == "manual" {
				continue
			}
			if _, err := st.Sync(ctx, s.hooks(st)); err != nil {
				out.PushError = st.Name + ": " + err.Error()
				break // never publish the tombstone without the copy
			}
			out.Pushed = append(out.Pushed, st.Name)
		}
	}
	return out, nil
}

// storeRef names a store portably: its remote URL, else its name.
func storeRef(st *Store) string {
	if st.URL != "" {
		return st.URL
	}
	return st.Name
}
