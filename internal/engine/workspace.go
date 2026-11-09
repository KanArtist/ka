package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/JumpKickOSS/ka/internal/gitx"
	"github.com/JumpKickOSS/ka/pkg/kaspec"
)

// WorkspaceFile is a workspace definition (PRD §9.1), read from
// --workspace, $KA_WORKSPACE, ./.ka-workspace.yml (searched upward) or
// <user config dir>/ka/workspace.yml.
type WorkspaceFile struct {
	Path     string
	Name     string
	Stores   []StoreSpec
	Actor    string            // identity.actor
	Repos    map[string]string // repo name -> local checkout
	Project  string            // default project
	Offline  bool
	Explicit bool
}

// StoreSpec is one stores: entry.
type StoreSpec struct {
	Name   string
	URL    string // remote; ka keeps a managed clone
	Branch string
	Path   string // attached checkout (store root)
	Push   string // immediate | manual | debounce[:2s]
	// FetchInterval overrides how often the daemon fetches this store.
	FetchInterval string
}

// Pointer is a code repo's .ka.yml (PRD §9.4).
type Pointer struct {
	Dir     string // the checkout containing .ka.yml
	Store   string // URL or path
	Branch  string
	Project string
}

// OpenOptions controls workspace discovery.
type OpenOptions struct {
	Dir       string            // where discovery starts (default: cwd)
	Workspace string            // explicit workspace file
	Map       map[string]string // remote URL -> local store directory
	Offline   bool              // never clone or fetch
}

// ConfigDir is where ka keeps user configuration.
func ConfigDir() string {
	if d := os.Getenv("KA_CONFIG_DIR"); d != "" {
		return d
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "ka")
		}
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, ".config", "ka")
		}
	}
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "ka")
}

// DataDir is where ka keeps managed clones.
func DataDir() string {
	if d := os.Getenv("KA_DATA_DIR"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "ka")
		}
	case "darwin":
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, "Library", "Application Support", "ka")
		}
	default:
		if x := os.Getenv("XDG_DATA_HOME"); x != "" {
			return filepath.Join(x, "ka")
		}
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, ".local", "share", "ka")
		}
	}
	return filepath.Join(os.TempDir(), "ka")
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[min(2, len(p)):])
		}
	}
	return p
}

// ParseWorkspaceFile reads a workspace definition.
func ParseWorkspaceFile(p string) (*WorkspaceFile, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	n, err := kaspec.ParseYAML(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", p, err)
	}
	if n.Kind != kaspec.MapNode {
		return nil, fmt.Errorf("%s: must be a mapping", p)
	}
	base := filepath.Dir(p)
	w := &WorkspaceFile{Path: p, Repos: map[string]string{}}
	str := func(m *kaspec.Node, k string) string {
		if m == nil || m.Map[k] == nil {
			return ""
		}
		return m.Map[k].Value
	}
	w.Name = str(n, "name")
	w.Project = str(n, "project")
	w.Offline = str(n, "offline") == "true"
	if id := n.Map["identity"]; id != nil {
		w.Actor = str(id, "actor")
	}
	if r := n.Map["repos"]; r != nil {
		for _, k := range r.Keys {
			w.Repos[k] = absFrom(base, expandHome(r.Map[k].Value))
		}
	}
	if st := n.Map["stores"]; st != nil {
		for i, it := range st.Items {
			sp := StoreSpec{Name: str(it, "name"), URL: str(it, "url"), Branch: str(it, "branch"), Push: str(it, "push"), FetchInterval: str(it, "fetch_interval")}
			if p := str(it, "path"); p != "" {
				sp.Path = absFrom(base, expandHome(p))
			}
			if sp.URL == "" && sp.Path == "" {
				return nil, fmt.Errorf("%s: stores[%d] needs url or path", p, i)
			}
			w.Stores = append(w.Stores, sp)
		}
	}
	for k := range n.Map {
		switch k {
		case "name", "project", "offline", "identity", "repos", "stores":
		default:
			return nil, fmt.Errorf("%s: unknown key %q", p, k)
		}
	}
	return w, nil
}

func absFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(base, p))
}

// findUp walks from dir to the filesystem root looking for rel.
func findUp(dir, rel string) (string, bool) {
	d, _ := filepath.Abs(dir)
	for {
		if _, err := os.Stat(filepath.Join(d, rel)); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// ParsePointer reads a .ka.yml pointer.
func ParsePointer(dir string) (*Pointer, error) {
	p := filepath.Join(dir, ".ka.yml")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	n, err := kaspec.ParseYAML(data)
	if err != nil || n.Kind != kaspec.MapNode {
		return nil, fmt.Errorf("%s: invalid pointer file", p)
	}
	pt := &Pointer{Dir: dir}
	if v := n.Map["store"]; v != nil {
		pt.Store = v.Value
	}
	if v := n.Map["branch"]; v != nil {
		pt.Branch = v.Value
	}
	if v := n.Map["project"]; v != nil {
		pt.Project = v.Value
	}
	if pt.Store == "" {
		return nil, fmt.Errorf("%s: store is required", p)
	}
	return pt, nil
}

// Workspace is a resolved set of stores.
type Workspace struct {
	File    *WorkspaceFile // nil for an implicit workspace
	Stores  []*Store
	Project string            // default project
	Repos   map[string]string // repo name -> local checkout
	Actor   string            // configured identity
	Source  string            // how the workspace was found
}

// Open discovers and opens the workspace.
func Open(ctx context.Context, o OpenOptions) (*Workspace, error) {
	if o.Dir == "" {
		o.Dir, _ = os.Getwd()
	}
	var wf *WorkspaceFile
	var err error
	explicit := o.Workspace
	if explicit == "" {
		explicit = os.Getenv("KA_WORKSPACE")
	}
	ws := &Workspace{Repos: map[string]string{}}
	switch {
	case explicit != "":
		if wf, err = ParseWorkspaceFile(explicit); err != nil {
			return nil, err
		}
		wf.Explicit = true
		ws.Source = explicit
	default:
		if d, ok := findUp(o.Dir, ".ka-workspace.yml"); ok {
			if wf, err = ParseWorkspaceFile(filepath.Join(d, ".ka-workspace.yml")); err != nil {
				return nil, err
			}
			ws.Source = wf.Path
		}
	}
	var global *WorkspaceFile
	if wf == nil {
		gp := filepath.Join(ConfigDir(), "workspace.yml")
		if _, err := os.Stat(gp); err == nil {
			if global, err = ParseWorkspaceFile(gp); err != nil {
				return nil, err
			}
		}
	}

	// Inside a store, or a code repo with a pointer?
	storeDir, inStore := findUp(o.Dir, filepath.Join(".ka", "store.yml"))
	var ptr *Pointer
	if !inStore {
		if d, ok := findUp(o.Dir, ".ka.yml"); ok {
			if ptr, err = ParsePointer(d); err != nil {
				return nil, err
			}
		}
	}
	if wf == nil && global != nil {
		use := !inStore && ptr == nil
		for _, sp := range global.Stores {
			if inStore && sp.Path != "" && samePath(sp.Path, storeDir) {
				use = true
			}
			if ptr != nil && (sameURL(sp.URL, ptr.Store) || (sp.Path != "" && samePath(sp.Path, absFrom(ptr.Dir, expandHome(ptr.Store))))) {
				use = true
			}
		}
		if use {
			wf = global
			ws.Source = global.Path
		}
	}
	offline := o.Offline || (wf != nil && wf.Offline)

	add := func(sp StoreSpec, readOnly bool) (*Store, error) {
		root := sp.Path
		managed := false
		if root == "" {
			if mapped, ok := lookupMap(o.Map, sp.URL); ok {
				root = mapped
			} else {
				root, err = ensureClone(ctx, sp, offline)
				if err != nil {
					return nil, err
				}
				managed = true
			}
		}
		for _, s := range ws.Stores {
			if samePath(s.Root, root) {
				if !readOnly {
					s.ReadOnly = false
				}
				return s, nil
			}
		}
		name := sp.Name
		if name == "" {
			name = storeNameFor(root)
		}
		for _, s := range ws.Stores {
			if s.Name == name {
				name = name + "-" + shortHash(root)
			}
		}
		s, err := OpenStore(ctx, name, root)
		if err != nil {
			return nil, err
		}
		s.URL, s.Managed, s.ReadOnly = sp.URL, managed, readOnly
		if sp.Push != "" {
			mode, d, err := ParsePushMode(sp.Push)
			if err != nil {
				return nil, Errf(CodeInvalid, "fix stores[].push in the workspace file", "store %s: %v", name, err)
			}
			s.PushMode, s.PushDelay, s.pushSet = mode, d, true
		}
		if sp.FetchInterval != "" {
			d, err := time.ParseDuration(sp.FetchInterval)
			if err != nil || d < time.Second {
				return nil, Errf(CodeInvalid, "use a duration of at least 1s, such as 15s", "store %s: fetch_interval %q", name, sp.FetchInterval)
			}
			s.FetchInterval, s.fetchSet = d, true
		}
		if s.URL == "" {
			if r, _ := s.git.Upstream(ctx); r != "" {
				s.URL, _ = s.git.Run(ctx, "remote", "get-url", r)
			}
		}
		ws.Stores = append(ws.Stores, s)
		return s, nil
	}

	switch {
	case wf != nil:
		ws.File = wf
		ws.Project, ws.Actor = wf.Project, wf.Actor
		for k, v := range wf.Repos {
			ws.Repos[k] = v
		}
		for _, sp := range wf.Stores {
			if _, err := add(sp, false); err != nil {
				return nil, err
			}
		}
		if inStore && ws.Project == "" {
			ws.Project = projectAt(storeDir, o.Dir)
		}
	case inStore:
		ws.Source = storeDir
		if _, err := add(StoreSpec{Path: storeDir}, false); err != nil {
			return nil, err
		}
		ws.Project = projectAt(storeDir, o.Dir)
		if global != nil {
			ws.Actor = global.Actor
			for k, v := range global.Repos {
				ws.Repos[k] = v
			}
		}
	case ptr != nil:
		ws.Source = filepath.Join(ptr.Dir, ".ka.yml")
		sp := StoreSpec{Branch: ptr.Branch}
		if dir, ok := localParentPath(ptr.Dir, ptr.Store); ok || strings.HasPrefix(ptr.Store, "~") {
			if !ok {
				dir = expandHome(ptr.Store)
			}
			sp.Path = dir
		} else {
			sp.URL = ptr.Store
		}
		if _, err := add(sp, false); err != nil {
			return nil, err
		}
		ws.Project = ptr.Project
		if global != nil {
			ws.Actor = global.Actor
			for k, v := range global.Repos {
				ws.Repos[k] = v
			}
		}
		if ptr.Project != "" {
			if _, ok := ws.Repos[ptr.Project]; !ok {
				ws.Repos[ptr.Project] = ptr.Dir
			}
		}
	default:
		return nil, Errf("no_workspace", "run inside a store or a code repo with .ka.yml, pass --workspace, or create "+filepath.Join(ConfigDir(), "workspace.yml"), "no KanArtist workspace found from %s", o.Dir)
	}

	// Pull in extends parents (read-only unless listed explicitly).
	for i := 0; i < len(ws.Stores); i++ {
		s := ws.Stores[i]
		data, err := os.ReadFile(filepath.Join(s.Root, ".ka", "store.yml"))
		if err != nil {
			continue
		}
		sf, _ := kaspec.ParseStoreFile(".ka/store.yml", data)
		if sf == nil {
			continue
		}
		for _, p := range sf.Extends {
			if findParent(ws.Stores, s.Root, p) != nil {
				continue
			}
			sp := StoreSpec{URL: p.URL, Branch: p.Ref}
			if dir, ok := localParentPath(s.Root, p.URL); ok && !isGitDir(dir) {
				sp = StoreSpec{Path: dir}
				if !isStoreDir(dir) {
					continue // reported by ka check as config_parent_unavailable
				}
			}
			// Offline, add uses an existing managed clone and never clones.
			if _, err := add(sp, true); err != nil {
				continue // unavailable parents are diagnostics, not fatal
			}
		}
	}
	return ws, nil
}

// projectAt returns the project key when dir is inside projects/<key>/.
func projectAt(storeDir, dir string) string {
	rel, err := filepath.Rel(storeDir, dir)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 2 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}

func lookupMap(m map[string]string, url string) (string, bool) {
	for k, v := range m {
		if sameURL(k, url) {
			return v, true
		}
	}
	return "", false
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

func storeNameFor(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".ka", "store.yml"))
	if err == nil {
		if sf, _ := kaspec.ParseStoreFile("store.yml", data); sf != nil && sf.Name != "" {
			return sf.Name
		}
	}
	n := strings.Trim(nonName.ReplaceAllString(strings.ToLower(filepath.Base(root)), "-"), "-")
	if n == "" {
		n = "store"
	}
	return n
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:8]
}

// ClonePath is where ka keeps the managed clone of a store URL.
func ClonePath(sp StoreSpec) string {
	name := sp.Name
	if name == "" {
		base := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(sp.URL, "/")), ".git")
		if i := strings.LastIndexAny(base, ":/"); i >= 0 {
			base = base[i+1:]
		}
		name = strings.Trim(nonName.ReplaceAllString(strings.ToLower(base), "-"), "-")
	}
	return filepath.Join(DataDir(), "stores", name+"-"+shortHash(sp.URL+"#"+sp.Branch))
}

// ensureClone returns the managed clone for a store URL, cloning it on first use.
func ensureClone(ctx context.Context, sp StoreSpec, offline bool) (string, error) {
	dir := ClonePath(sp)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, nil
	}
	if offline {
		return "", Errf("store_unavailable", "go online once so ka can clone it, or map it to a local checkout with --map", "no local clone of %s", sp.URL)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	args := []string{"clone", "-q"}
	if sp.Branch != "" {
		args = append(args, "--branch", sp.Branch)
	}
	args = append(args, sp.URL, dir)
	g := &gitx.Repo{Dir: filepath.Dir(dir)}
	if _, err := g.Run(ctx, args...); err != nil {
		os.RemoveAll(dir)
		return "", Errf("store_unavailable", "check the URL and your git credentials", "cannot clone %s: %s", sp.URL, firstLine(err.Error()))
	}
	return dir, nil
}

// WriteStore picks the store new tickets for a project go to.
func (ws *Workspace) StoreByName(name string) *Store {
	for _, s := range ws.Stores {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// isGitDir reports whether dir is a bare git repository (a local remote).
func isGitDir(dir string) bool {
	if isStoreDir(dir) {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "HEAD"))
	_, err2 := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && err2 == nil
}
