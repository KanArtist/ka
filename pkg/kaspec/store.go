package kaspec

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Parent is one entry of store.yml `extends`.
type Parent struct {
	URL string
	Ref string
}

// StoreFile is .ka/store.yml.
type StoreFile struct {
	Spec        int
	Name        string
	Description string
	// RequiresKa is the oldest ka release that may read or write the store
	// (spec §5.1), e.g. "1.2.0"; empty means any.
	RequiresKa string
	Extends    []Parent
}

// Repo is a code repository a project's links can point into.
type Repo struct {
	Forge         string
	DefaultBranch string
}

// ProjectFile is projects/<key>/project.yml.
type ProjectFile struct {
	Key    string
	Prefix string
	// FormerPrefixes are prefixes the project used before a rename; IDs
	// with them still resolve to the ticket with the same suffix.
	FormerPrefixes []string
	Name           string
	Description    string
	Archived       bool
	Repos          map[string]Repo
	Config         *Node
	MovedToStore   string
}

// Human is a human actor.
type Human struct {
	Name    string
	Emails  []string
	Aliases []string
}

// Actors is .ka/actors.yml.
type Actors struct {
	Humans map[string]Human
	Agents map[string]string // handle -> display name
	Roles  map[string][]string
}

// Board is boards/<name>.yml.
type Board struct {
	Name, Description, Filter, GroupBy, Sort, Swimlanes string
	Columns, CardFields                                 []string
}

var (
	storeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	boardFileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.yml$`)
)

func parseYAMLFile(p string, data []byte) (*Node, []Diagnostic) {
	var diags []Diagnostic
	if strings.HasPrefix(string(data), "\xEF\xBB\xBF") {
		diags = append(diags, diag("bom", p, 1, "save without a byte-order mark", "file starts with a UTF-8 byte-order mark"))
	}
	if strings.ContainsRune(string(data), '\r') {
		diags = append(diags, diag("crlf", p, 0, "use LF line endings", "file contains CR characters (CRLF line endings)"))
	}
	n, err := ParseYAML(NormalizeEncoding(data))
	if err != nil {
		ye := err.(*YAMLError)
		return nil, append(diags, diag(ye.Code, p, ye.Line, "", "%s", ye.Msg))
	}
	if n.Kind != MapNode {
		return nil, append(diags, diag("yaml_syntax", p, 1, "", "file must be a YAML mapping"))
	}
	return n, diags
}

// ParseStoreFile parses .ka/store.yml.
func ParseStoreFile(p string, data []byte) (*StoreFile, []Diagnostic) {
	n, diags := parseYAMLFile(p, data)
	if n == nil {
		return nil, diags
	}
	r := newFieldReader(p, n)
	sf := &StoreFile{}
	spec, specOK := r.integer("spec")
	if specOK {
		sf.Spec = int(spec)
	} else if r.node.Map["spec"] == nil {
		r.diags = append(r.diags, diag("missing_field", p, 1, "add `spec: 1`", "required field \"spec\" is missing"))
	}
	sf.Name = r.strRe("name", storeNameRe, "store names are lower-case: letters, digits, . _ -")
	if sf.Name == "" && r.node.Map["name"] == nil {
		r.diags = append(r.diags, diag("missing_field", p, 1, "", "required field \"name\" is missing"))
	}
	sf.Description = r.str("description")
	sf.RequiresKa = r.strRe("requires_ka", versionRe, "a release version such as 1.2.0")
	if e := r.get("extends"); e != nil {
		if e.Kind != SeqNode {
			r.bad("extends", "", "must be a list of {url, ref}")
		} else {
			for _, it := range e.Items {
				if it.Kind != MapNode {
					r.bad("extends", "", "each parent must be a mapping with url and optionally ref")
					continue
				}
				er := newFieldReader(p, it)
				par := Parent{URL: er.str("url"), Ref: er.str("ref")}
				if par.URL == "" {
					er.diags = append(er.diags, diag("missing_field", p, it.Line, "", "extends: url is required"))
				}
				er.unknown([]string{"url", "ref"})
				r.diags = append(r.diags, er.diags...)
				sf.Extends = append(sf.Extends, par)
			}
		}
	}
	r.unknown([]string{"spec", "name", "description", "requires_ka", "extends"})
	diags = append(diags, r.diags...)
	if !KaVersionSatisfies(sf.RequiresKa) {
		diags = append(diags, diag("ka_outdated", p, n.KeyLines["requires_ka"], UpgradeHint, "store %s requires ka %s or newer; this is ka %s", sf.Name, sf.RequiresKa, KaVersion))
	}
	if specOK && spec != SpecVersion {
		diags = append(diags, diag("spec_unsupported", p, n.KeyLines["spec"], "upgrade ka, or run `ka migrate`", "store uses spec %d; this implementation supports spec %d", spec, SpecVersion))
	}
	return sf, diags
}

// ParseProjectFile parses projects/<key>/project.yml.
func ParseProjectFile(p string, data []byte) (*ProjectFile, []Diagnostic) {
	n, diags := parseYAMLFile(p, data)
	if n == nil {
		return nil, diags
	}
	r := newFieldReader(p, n)
	pf := &ProjectFile{Repos: map[string]Repo{}}
	pf.Key = r.strRe("key", ProjectKeyRe, "project keys are lower-case letters, digits and -, starting with a letter")
	pf.Prefix = r.strRe("prefix", PrefixRe, "prefixes are upper-case letters and digits, starting with a letter, at most 10")
	pf.Name = r.str("name")
	pf.Description = r.str("description")
	pf.Archived, _ = r.boolean("archived")
	pf.MovedToStore = r.str("moved_to_store")
	pf.FormerPrefixes = r.strList("former_prefixes", PrefixRe, "prefixes are upper-case letters and digits, starting with a letter, at most 10")
	if rp := r.get("repos"); rp != nil {
		if rp.Kind != MapNode {
			r.bad("repos", "", "must be a mapping of repo names to {forge, default_branch}")
		} else {
			for _, name := range rp.Keys {
				v := rp.Map[name]
				if !ProjectKeyRe.MatchString(name) {
					r.diags = append(r.diags, diag("invalid_value", p, rp.KeyLines[name], "", "repos: invalid repo name %q", name))
					continue
				}
				if v == nil || v.Kind != MapNode {
					r.diags = append(r.diags, diag("invalid_value", p, rp.KeyLines[name], "", "repos.%s: must be a mapping with forge", name))
					continue
				}
				rr := newFieldReader(p, v)
				repo := Repo{Forge: rr.strRe("forge", urlRe, "forge is the repo's base URL, e.g. https://github.com/org/repo"), DefaultBranch: rr.str("default_branch")}
				rr.unknown([]string{"forge", "default_branch"})
				r.diags = append(r.diags, rr.diags...)
				pf.Repos[name] = repo
			}
		}
	}
	if c := r.get("config"); c != nil {
		if c.Kind != MapNode {
			r.bad("config", "", "must be a mapping (same keys as .ka/config.yml)")
		} else {
			pf.Config = c
		}
	}
	r.unknown([]string{"key", "prefix", "former_prefixes", "name", "description", "archived", "repos", "config", "moved_to_store"})
	if pf.Key == "" && n.Map["key"] == nil {
		r.diags = append(r.diags, diag("missing_field", p, 1, "", "required field \"key\" is missing"))
	}
	if pf.Prefix == "" {
		pf.Prefix = DefaultPrefix(pf.Key)
	}
	return pf, append(diags, r.diags...)
}

// ParseActors parses .ka/actors.yml.
func ParseActors(p string, data []byte) (*Actors, []Diagnostic) {
	n, diags := parseYAMLFile(p, data)
	if n == nil {
		return nil, diags
	}
	a := &Actors{Humans: map[string]Human{}, Agents: map[string]string{}, Roles: map[string][]string{}}
	r := newFieldReader(p, n)
	if h := r.get("humans"); h != nil && h.Kind == MapNode {
		for _, k := range h.Keys {
			if !HumanRe.MatchString(k) {
				r.diags = append(r.diags, diag("invalid_value", p, h.KeyLines[k], "", "humans: invalid handle %q", k))
				continue
			}
			if h.Map[k].Kind != MapNode {
				r.diags = append(r.diags, diag("invalid_value", p, h.KeyLines[k], "", "humans.%s: must be a mapping with name, emails and aliases", k))
				continue
			}
			hr := newFieldReader(p, h.Map[k])
			hu := Human{Name: hr.str("name"), Emails: hr.strList("emails", nil, ""), Aliases: hr.strList("aliases", nil, "")}
			hr.unknown([]string{"name", "emails", "aliases"})
			r.diags = append(r.diags, hr.diags...)
			a.Humans[k] = hu
		}
	} else if h != nil {
		r.bad("humans", "", "must be a mapping of handles")
	}
	if g := r.get("agents"); g != nil && g.Kind == MapNode {
		for _, k := range g.Keys {
			if !AgentRe.MatchString(k) {
				r.diags = append(r.diags, diag("invalid_value", p, g.KeyLines[k], "agent handles are agent/<name>", "agents: invalid handle %q", k))
				continue
			}
			if g.Map[k].Kind != MapNode {
				r.diags = append(r.diags, diag("invalid_value", p, g.KeyLines[k], "", "agents.%s: must be a mapping with name", k))
				continue
			}
			gr := newFieldReader(p, g.Map[k])
			a.Agents[k] = gr.str("name")
			gr.unknown([]string{"name"})
			r.diags = append(r.diags, gr.diags...)
		}
	} else if g != nil {
		r.bad("agents", "", "must be a mapping of handles")
	}
	if ro := r.get("roles"); ro != nil && ro.Kind == MapNode {
		rr := newFieldReader(p, ro)
		for _, k := range ro.Keys {
			a.Roles[k] = rr.strList(k, ActorRe, "")
		}
		r.diags = append(r.diags, rr.diags...)
	} else if ro != nil {
		r.bad("roles", "", "must be a mapping of role names to member lists")
	}
	r.unknown([]string{"humans", "agents", "roles"})
	return a, append(diags, r.diags...)
}

// ParseBoard parses boards/<name>.yml.
func ParseBoard(p string, data []byte) (*Board, []Diagnostic) {
	n, diags := parseYAMLFile(p, data)
	if n == nil {
		return nil, diags
	}
	r := newFieldReader(p, n)
	b := &Board{
		Name: r.str("name"), Description: r.str("description"), Filter: r.str("filter"),
		GroupBy: r.str("group_by"), Sort: r.str("sort"), Swimlanes: r.str("swimlanes"),
		Columns: r.strList("columns", nil, ""), CardFields: r.strList("card_fields", nil, ""),
	}
	if b.Name == "" {
		r.diags = append(r.diags, diag("missing_field", p, 1, "", "required field \"name\" is missing"))
	}
	r.unknown([]string{"name", "description", "filter", "group_by", "sort", "swimlanes", "columns", "card_fields"})
	return b, append(diags, r.diags...)
}

// ---- loading a store from a filesystem --------------------------------------

// Store is a loaded store.
type Store struct {
	Label    string // display name used as the diagnostic path prefix
	FS       fs.FS
	File     *StoreFile
	Config   *Node // .ka/config.yml (nil when absent)
	Actors   *Actors
	Boards   map[string]*Board
	Projects map[string]*Project
	Keys     []string // project keys, sorted
	// Moved maps the key of a tombstoned project (moved_to_store) to the
	// store it moved to. Tombstones are not in Projects or Keys.
	Moved map[string]string
	// History maps history/ file names to their summaries (spec §2).
	History   map[string]*HistorySummary
	Diags     []Diagnostic
	ParentFor func(Parent) (*Store, error) // set by the workspace
}

// Project is a loaded project.
type Project struct {
	Key     string
	Dir     string
	File    *ProjectFile
	Tickets map[string]*TicketEntry
	IDs     []string // sorted
	Config  *Config  // effective config (set by Check)
}

// TicketEntry is a loaded ticket directory.
type TicketEntry struct {
	Dir      string // relative to the store root
	Ticket   *Ticket
	Raw      []byte
	Comments []*CommentEntry
	Files    []FileEntry
}

// CommentEntry is a loaded comment file.
type CommentEntry struct {
	Name    string
	Path    string
	Comment *Comment
	Raw     []byte
}

// FileEntry is an attachment.
type FileEntry struct {
	Path string
	Size int64
}

func (s *Store) p(rel string) string {
	if s.Label == "" {
		return rel
	}
	return s.Label + "/" + rel
}

func (s *Store) add(ds ...Diagnostic) { s.Diags = append(s.Diags, ds...) }

// MaxFileBytes is the largest store file LoadStore reads (spec §2). A bigger
// file is reported as unreadable instead of being loaded.
const MaxFileBytes = 8 << 20

// Files and directories are classified by the type their parent directory
// listing reports, so loading costs no path lookups beyond the listings and
// one open per file; symbolic links are never followed.

// readEntry reads the file e found at rel. It fails when the entry is not a
// regular file, when it is larger than MaxFileBytes, or when reading fails.
func (s *Store) readEntry(rel string, e fs.DirEntry) ([]byte, error) {
	if !e.Type().IsRegular() {
		return nil, notRegular(e.Type())
	}
	f, err := s.FS.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// re-check the opened file: the entry may have been swapped since listing
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, notRegular(info.Mode())
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, fmt.Errorf("file is larger than %d bytes", MaxFileBytes)
	}
	return b, nil
}

// listEntry lists the directory e found at rel. A nil entry is an absent,
// empty directory; an entry that is not a directory (a symbolic link, say) or
// cannot be listed is reported through add.
func (s *Store) listEntry(rel string, e fs.DirEntry, add func(...Diagnostic)) []fs.DirEntry {
	if e == nil {
		return nil
	}
	if !e.IsDir() {
		add(s.unreadable(rel, notRegular(e.Type())))
		return nil
	}
	ents, err := fs.ReadDir(s.FS, rel)
	if err != nil {
		add(s.unreadable(rel, err))
		return nil
	}
	return ents
}

// entry returns the listing entry named name, or nil.
func entry(ents []fs.DirEntry, name string) fs.DirEntry {
	// listings are sorted by name
	if i, ok := slices.BinarySearchFunc(ents, name, func(e fs.DirEntry, n string) int { return strings.Compare(e.Name(), n) }); ok {
		return ents[i]
	}
	return nil
}

func isLink(e fs.DirEntry) bool { return e.Type()&fs.ModeSymlink != 0 }

func notRegular(m fs.FileMode) error {
	switch {
	case m&fs.ModeSymlink != 0:
		return errors.New("is a symbolic link; links are not followed")
	case m.IsDir():
		return errors.New("is a directory, not a file")
	case m.IsRegular():
		return errors.New("is a file, not a directory")
	}
	return errors.New("is not a regular file")
}

func (s *Store) unreadable(rel string, err error) Diagnostic {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err // the path is already in the diagnostic (and may be absolute)
	}
	return diag("unreadable", s.p(rel), 0, "replace it with a regular file of at most 8 MiB, or fix its permissions", "cannot read: %v", err)
}

// LoadStore reads a store rooted at fsys. Label prefixes diagnostic paths
// (use "" for a single store). Parsing diagnostics are collected in Store.Diags;
// cross-file rules run in Check.
func LoadStore(fsys fs.FS, label string) *Store {
	s := &Store{Label: label, FS: fsys, Boards: map[string]*Board{}, Projects: map[string]*Project{}}
	notStore := func() *Store {
		s.add(diag("invalid_store", s.p(".ka/store.yml"), 0, "run `ka init`, or create .ka/store.yml with `spec: 1` and `name:`", "not a KanArtist store: .ka/store.yml is missing"))
		return s
	}
	root, err := fs.ReadDir(fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return notStore()
	} else if err != nil {
		s.add(s.unreadable(".ka/store.yml", err))
		return s
	}
	ka := entry(root, ".ka")
	if ka == nil {
		return notStore()
	}
	if !ka.IsDir() {
		s.add(s.unreadable(".ka", notRegular(ka.Type())))
		return s
	}
	kaEnts, err := fs.ReadDir(fsys, ".ka")
	if err != nil {
		s.add(s.unreadable(".ka", err))
		return s
	}
	se := entry(kaEnts, "store.yml")
	if se == nil {
		return notStore()
	}
	data, err := s.readEntry(".ka/store.yml", se)
	if err != nil {
		s.add(s.unreadable(".ka/store.yml", err))
		return s
	}
	sf, ds := ParseStoreFile(s.p(".ka/store.yml"), data)
	s.add(ds...)
	s.File = sf
	if data, ok := s.readOptional(".ka/config.yml", entry(kaEnts, "config.yml"), s.add); ok {
		n, ds := parseYAMLFile(s.p(".ka/config.yml"), data)
		s.add(ds...)
		s.Config = n
	}
	if data, ok := s.readOptional(".ka/actors.yml", entry(kaEnts, "actors.yml"), s.add); ok {
		a, ds := ParseActors(s.p(".ka/actors.yml"), data)
		s.add(ds...)
		s.Actors = a
	}
	for _, e := range s.listEntry("boards", entry(root, "boards"), s.add) {
		rel := "boards/" + e.Name()
		if e.IsDir() || !boardFileRe.MatchString(e.Name()) {
			s.add(diag("stray_file", s.p(rel), 0, "board files are boards/<name>.yml", "unexpected entry in boards/"))
			continue
		}
		data, err := s.readEntry(rel, e)
		if err != nil {
			s.add(s.unreadable(rel, err))
			continue
		}
		b, ds := ParseBoard(s.p(rel), data)
		s.add(ds...)
		if b != nil {
			s.Boards[strings.TrimSuffix(e.Name(), ".yml")] = b
		}
	}
	s.loadHistory(root)
	for _, e := range s.listEntry("projects", entry(root, "projects"), s.add) {
		rel := "projects/" + e.Name()
		switch {
		case isLink(e):
			s.add(s.unreadable(rel, notRegular(e.Type())))
		case !e.IsDir():
			s.add(diag("stray_file", s.p(rel), 0, "", "unexpected file in projects/"))
		default:
			s.loadProject(e.Name())
		}
	}
	for k := range s.Projects {
		s.Keys = append(s.Keys, k)
	}
	sort.Strings(s.Keys)
	return s
}

// readOptional reads the file e found at rel; a nil entry is an absent file.
// A file that exists but cannot be read is reported through add.
func (s *Store) readOptional(rel string, e fs.DirEntry, add func(...Diagnostic)) ([]byte, bool) {
	if e == nil {
		return nil, false
	}
	data, err := s.readEntry(rel, e)
	if err != nil {
		add(s.unreadable(rel, err))
		return nil, false
	}
	return data, true
}

func (s *Store) loadProject(dirName string) {
	dir := "projects/" + dirName
	pyml := dir + "/project.yml"
	ents, err := fs.ReadDir(s.FS, dir)
	if err != nil {
		s.add(s.unreadable(dir, err))
		return
	}
	pe := entry(ents, "project.yml")
	if pe == nil {
		s.add(diag("project_missing_file", s.p(pyml), 0, "create project.yml with at least `key:`", "project directory has no project.yml"))
		return
	}
	data, ok := s.readOptional(pyml, pe, s.add)
	if !ok {
		return
	}
	pf, ds := ParseProjectFile(s.p(pyml), data)
	s.add(ds...)
	if pf == nil {
		return
	}
	if pf.Key != dirName {
		s.add(diag("project_key_mismatch", s.p(pyml), 1, "make the key equal the directory name", "project key %q does not match its directory %q", pf.Key, dirName))
	}
	tents := s.listEntry(dir+"/tickets", entry(ents, "tickets"), s.add)
	if pf.MovedToStore != "" {
		if s.Moved == nil {
			s.Moved = map[string]string{}
		}
		s.Moved[dirName] = pf.MovedToStore
		if len(tents) > 0 {
			s.add(diag("moved_project_has_tickets", s.p(pyml), 1, "the project lives in "+pf.MovedToStore+"; remove projects/"+dirName+"/tickets here",
				"project %q moved to %s but still has tickets here; they are ignored", dirName, pf.MovedToStore))
		}
		return
	}
	pr := &Project{Key: dirName, Dir: dir, File: pf, Tickets: map[string]*TicketEntry{}}
	s.Projects[dirName] = pr

	for _, e := range ents {
		switch e.Name() {
		case "project.yml", "tickets", "docs":
		default:
			s.add(diag("stray_file", s.p(dir+"/"+e.Name()), 0, "a project holds project.yml, tickets/ and docs/", "unexpected entry in project directory"))
		}
	}
	// Tickets are independent: read and parse them in parallel, then merge in
	// directory order so results and diagnostics stay deterministic.
	type loaded struct {
		te    *TicketEntry
		diags []Diagnostic
	}
	out := make([]loaded, len(tents))
	var wg sync.WaitGroup
	work := make(chan int)
	for range min(runtime.GOMAXPROCS(0), max(len(tents), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				te, ds := s.loadTicket(dir+"/tickets/"+tents[i].Name(), tents[i].Name())
				out[i] = loaded{te, ds}
			}
		}()
	}
	for i, e := range tents {
		rel := dir + "/tickets/" + e.Name()
		switch {
		case isLink(e):
			s.add(s.unreadable(rel, notRegular(e.Type())))
		case !e.IsDir():
			s.add(diag("stray_file", s.p(rel), 0, "tickets live in tickets/<ID>/ticket.md", "unexpected file in tickets/"))
		default:
			work <- i
		}
	}
	close(work)
	wg.Wait()
	for i, l := range out {
		s.add(l.diags...)
		if l.te == nil {
			continue
		}
		pr.Tickets[tents[i].Name()] = l.te
	}
	for id := range pr.Tickets {
		pr.IDs = append(pr.IDs, id)
	}
	sort.Strings(pr.IDs)
}

// LoadTicket reads one ticket directory (relative to the store root).
func (s *Store) LoadTicket(tdir string) (*TicketEntry, []Diagnostic) {
	// no parent listing here: refuse a symlinked ticket directory explicitly
	if info, err := fs.Lstat(s.FS, tdir); err == nil && !info.IsDir() {
		return nil, []Diagnostic{s.unreadable(tdir, notRegular(info.Mode()))}
	}
	return s.loadTicket(tdir, path.Base(tdir))
}

// loadTicket reads a ticket directory that is known to be a directory.
func (s *Store) loadTicket(tdir, name string) (*TicketEntry, []Diagnostic) {
	var diags []Diagnostic
	add := func(ds ...Diagnostic) { diags = append(diags, ds...) }
	tp := tdir + "/ticket.md"
	missing := func() (*TicketEntry, []Diagnostic) {
		add(diag("ticket_missing_file", s.p(tdir), 0, "every ticket directory needs a ticket.md", "ticket directory has no ticket.md"))
		return nil, diags
	}
	ents, err := fs.ReadDir(s.FS, tdir)
	if errors.Is(err, fs.ErrNotExist) {
		return missing()
	} else if err != nil {
		add(s.unreadable(tdir, err))
		return nil, diags
	}
	me := entry(ents, "ticket.md")
	if me == nil {
		return missing()
	}
	data, ok := s.readOptional(tp, me, add)
	if !ok {
		return nil, diags
	}
	t, ds := ParseTicket(s.p(tp), data)
	add(ds...)
	te := &TicketEntry{Dir: tdir, Ticket: t, Raw: data}
	if t != nil && t.ID != "" && t.ID != name {
		add(diag("id_dir_mismatch", s.p(tp), lineOf(t, "id"), "the directory name must equal the ticket id", "ticket id %q does not match its directory %q", t.ID, name))
	}

	for _, e := range ents {
		rel := tdir + "/" + e.Name()
		switch {
		case e.Name() == "ticket.md":
		case (e.Name() == "comments" || e.Name() == "files") && isLink(e):
			add(s.unreadable(rel, notRegular(e.Type())))
		case e.Name() == "comments" && e.IsDir():
			for _, ce := range s.listEntry(rel, e, add) {
				cp := rel + "/" + ce.Name()
				if ce.IsDir() || !strings.HasSuffix(ce.Name(), ".md") {
					add(diag("stray_file", s.p(cp), 0, "comments are <yyyymmddThhmmssZ>-<XXXX>.md files", "unexpected entry in comments/"))
					continue
				}
				data, ok := s.readOptional(cp, ce, add)
				if !ok {
					continue
				}
				if !CommentFileRe.MatchString(ce.Name()) {
					add(diag("comment_filename", s.p(cp), 0, "name comments <yyyymmddThhmmssZ>-<4 base32 chars>.md, e.g. 20261107T130722Z-4F2A.md", "invalid comment filename %q", ce.Name()))
				}
				c, ds := ParseComment(s.p(cp), data)
				add(ds...)
				te.Comments = append(te.Comments, &CommentEntry{Name: ce.Name(), Path: cp, Comment: c, Raw: data})
			}
		case e.Name() == "files" && e.IsDir():
			s.walkFiles(rel, e, te, add)
		default:
			add(diag("stray_file", s.p(rel), 0, "a ticket directory holds ticket.md, comments/ and files/", "unexpected entry in ticket directory"))
		}
	}
	slices.SortFunc(te.Comments, func(a, b *CommentEntry) int { return strings.Compare(a.Name, b.Name) })
	return te, diags
}

// walkFiles records the attachments under the directory e found at rel, in
// lexical order. Attachments are not read; symbolic links are not followed.
func (s *Store) walkFiles(rel string, e fs.DirEntry, te *TicketEntry, add func(...Diagnostic)) {
	for _, f := range s.listEntry(rel, e, add) {
		fp := rel + "/" + f.Name()
		if f.IsDir() {
			s.walkFiles(fp, f, te, add)
			continue
		}
		if info, err := f.Info(); err == nil {
			te.Files = append(te.Files, FileEntry{Path: fp, Size: info.Size()})
		}
	}
}

func lineOf(t *Ticket, key string) int {
	if t == nil || t.Lines == nil {
		return 0
	}
	return t.Lines[key]
}

// Join is path.Join for store-relative paths.
func Join(elem ...string) string { return path.Join(elem...) }
