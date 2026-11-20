package engine

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/KanArtist/ka/pkg/kaspec"
)

// RequireOut reports RequireKa.
type RequireOut struct {
	Store    string `json:"store"`
	From     string `json:"from,omitempty" doc:"requires_ka before (empty: none)."`
	To       string `json:"to" doc:"requires_ka now."`
	Changed  bool   `json:"changed" doc:"false when the store already required this version or newer."`
	Commit   string `json:"commit,omitempty"`
	Pushed   bool   `json:"pushed"`
	Local    bool   `json:"local_only,omitempty"`
	PushNote string `json:"push_error,omitempty"`
}

var (
	requireLine = regexp.MustCompile(`(?m)^requires_ka:.*$`)
	extendsLine = regexp.MustCompile(`(?m)^extends:`)
	releaseRe   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// RequireKa raises a store's requires_ka (spec §5.1) to at least version,
// so clients older than that stop with an explanation instead of
// misreading data they do not understand (slugs need 1.2.0, initiatives and
// epics 1.3.0). It never lowers the requirement and refuses a version newer
// than this ka. The edit is one commit to .ka/store.yml, pushed like any
// other write. Raise it only once every client of the store has upgraded.
func (s *Service) RequireKa(ctx context.Context, c Caller, store, version string) (*RequireOut, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if !releaseRe.MatchString(version) {
		return nil, Errf(CodeInvalid, "pass a release version such as "+kaspec.ContainersMinKa, "%q is not a release version", version)
	}
	if !kaspec.KaVersionSatisfies(version) {
		return nil, Errf(CodeInvalid, "upgrade this ka first ("+kaspec.UpgradeHint+")", "this is ka %s; it cannot require %s", kaspec.KaVersion, version)
	}
	st, err := s.requireStore(store)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	unlock, err := st.lock(ctx)
	if err != nil {
		return nil, err
	}
	if err := st.guard(); err != nil {
		unlock()
		return nil, err
	}
	const rel = ".ka/store.yml"
	p := filepath.Join(st.Root, filepath.FromSlash(rel))
	if fi, lerr := os.Lstat(p); lerr == nil && !fi.Mode().IsRegular() {
		unlock()
		return nil, irregular(p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		unlock()
		return nil, Errf(CodeInvalid, "run ka check", "%s has no readable .ka/store.yml: %v", st.Name, err)
	}
	out := &RequireOut{Store: st.Name, From: requiresKa(data), To: version}
	if out.From != "" && kaspec.VersionAtLeastRelease(out.From, version) {
		unlock()
		out.To = out.From
		if out.From != version {
			return nil, Errf(CodeInvalid, "requires_ka is never lowered; edit .ka/store.yml by hand if you really mean it",
				"store %s already requires ka %s, newer than %s", st.Name, out.From, version)
		}
		out.Pushed = true
		return out, nil
	}
	text := string(data)
	switch line := "requires_ka: " + version; {
	case requireLine.MatchString(text):
		text = requireLine.ReplaceAllLiteralString(text, line)
	case extendsLine.MatchString(text):
		i := extendsLine.FindStringIndex(text)[0]
		text = text[:i] + line + "\n" + text[i:]
	default:
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += line + "\n"
	}
	if sf, ds := kaspec.ParseStoreFile(rel, []byte(text)); sf == nil || sf.RequiresKa != version || kaspec.HasErrors(ds, false) {
		unlock()
		return nil, Errf(CodeInvalid, "fix .ka/store.yml (ka check), or add `requires_ka: "+version+"` by hand", "could not set requires_ka in %s's store.yml", st.Name)
	}
	before := st.git.Head(ctx)
	err = os.WriteFile(p, []byte(text), 0o644)
	var sha string
	if err == nil {
		sha, err = st.commitPaths(ctx, c, "store: require ka "+version, nil, []string{rel})
	}
	if err != nil {
		st.rollback(ctx, []string{st.repoPath(rel)})
		unlock()
		return nil, err
	}
	if sha != "" {
		st.own(before, sha)
	}
	unlock()
	out.Changed, out.Commit = sha != "", short(sha)
	s.ix.refresh(st, []string{rel})
	s.publish(Event{Type: EvStoreChanged, At: s.now(), Store: st.Name, Actor: c.Actor, Data: map[string]string{"what": "store", "path": rel}})
	res, err := s.pushStore(ctx, st)
	if err != nil {
		return out, Errf(CodeGit, "the change is committed locally; ka sync retries the push", "push %s: %v", st.Name, err)
	}
	if res != nil {
		out.Pushed, out.Local, out.PushNote = res.Pushed || !res.HasRemote, !res.HasRemote, res.PushError
	}
	return out, nil
}

// requireStore picks the store: the one named, else the only writable one.
func (s *Service) requireStore(name string) (*Store, error) {
	if name != "" {
		return s.writableStore(name)
	}
	var w []*Store
	var names []string
	for _, st := range s.WS.Stores {
		if !st.ReadOnly {
			w = append(w, st)
			names = append(names, st.Name)
		}
	}
	if len(w) == 1 {
		return w[0], nil
	}
	return nil, Errf(CodeInvalid, "pass the store (one of: "+strings.Join(names, ", ")+")", "no default store")
}

// requireHint tells a person how to raise requires_ka to version.
func requireHint(version string) string {
	return "once every client of the store runs ka " + version + " or newer, run `ka store require " + version + "` (it sets requires_ka in .ka/store.yml, commits and pushes)"
}
