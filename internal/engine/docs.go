package engine

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// DocEntry is one file under a project's docs/ directory (PRD §7.2, §13.2).
type DocEntry struct {
	Project string `json:"project"`
	Path    string `json:"path" doc:"Relative to the project's docs/ directory, slash-separated."`
	Title   string `json:"title" doc:"The first markdown heading, else the file name."`
	Size    int64  `json:"size"`
}

// DocsOut lists project docs.
type DocsOut struct {
	Docs []DocEntry `json:"docs"`
}

// DocOut is one doc with the tickets that mention it.
type DocOut struct {
	DocEntry
	Store     string `json:"store"`
	StorePath string `json:"store_path" doc:"The path tickets use to link it: projects/<key>/docs/<path>."`
	Body      string `json:"body" doc:"The file's text (markdown for .md files)."`
	Text      bool   `json:"text" doc:"False for binary files, whose body is empty."`
	Truncated bool   `json:"truncated,omitempty"`
	Backlinks []Ref  `json:"backlinks" doc:"Tickets whose description, links or comments mention store_path."`
}

const maxDoc = 1 << 20

// Docs lists the files under projects/<key>/docs/ (every project when
// project is empty), sorted by project and path. Links and other files
// that are not regular are left out: ka never follows them.
func (s *Service) Docs(project string) (*DocsOut, error) {
	s.ix.catchUp()
	// Read the project list under the index lock, walk without it.
	type proj struct{ key, root string }
	var projs []proj
	s.ix.mu.RLock()
	if project != "" && s.ix.projects[project] == nil {
		s.ix.mu.RUnlock()
		return nil, s.unknownDocProject(project)
	}
	for _, k := range sortedKeys(s.ix.projects) {
		if project == "" || k == project {
			pi := s.ix.projects[k]
			projs = append(projs, proj{k, filepath.Join(pi.Store.Root, filepath.FromSlash(pi.Dir), "docs")})
		}
	}
	s.ix.mu.RUnlock()
	out := &DocsOut{Docs: []DocEntry{}}
	for _, pr := range projs {
		filepath.WalkDir(pr.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			rel, _ := filepath.Rel(pr.root, p)
			info, _ := d.Info()
			e := DocEntry{Project: pr.key, Path: filepath.ToSlash(rel), Title: d.Name()}
			if info != nil {
				e.Size = info.Size()
			}
			if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				e.Title = docTitle(p, d.Name())
			}
			out.Docs = append(out.Docs, e)
			return nil
		})
	}
	sort.SliceStable(out.Docs, func(i, j int) bool {
		a, b := out.Docs[i], out.Docs[j]
		return a.Project < b.Project || a.Project == b.Project && a.Path < b.Path
	})
	return out, nil
}

func (s *Service) unknownDocProject(project string) error {
	return Errf(CodeUnknownProject, "projects: "+strings.Join(s.Projects(), ", "), "no project %q", project)
}

// Doc returns one project doc and its backlinks. The file is read through
// the docs directory as a root, so a link cannot lead out of it, and it must
// be a regular file; at most maxDoc bytes of it are returned.
func (s *Service) Doc(project, rel string) (*DocOut, error) {
	s.ix.catchUp()
	s.ix.mu.RLock()
	pi := s.ix.projects[project]
	var root, dir, storeName string
	if pi != nil {
		root, dir, storeName = filepath.Join(pi.Store.Root, filepath.FromSlash(pi.Dir), "docs"), pi.Dir, pi.Store.Name
	}
	s.ix.mu.RUnlock()
	if pi == nil {
		return nil, s.unknownDocProject(project)
	}
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))[1:]
	if clean == "" || clean == "." || strings.HasPrefix(clean, "../") {
		return nil, Errf(CodeInvalid, "pass a path inside the project's docs/ directory", "bad doc path %q", rel)
	}
	notFound := Errf(CodeNotFound, "list docs with GET /api/v1/docs?project="+project, "no doc %s in project %s", clean, project)
	data, size, err := readDoc(root, clean)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notFound
	} else if err != nil {
		return nil, err
	}
	out := &DocOut{DocEntry: DocEntry{Project: project, Path: clean, Title: path.Base(clean), Size: size}, Store: storeName,
		StorePath: dir + "/docs/" + clean, Backlinks: []Ref{}}
	if len(data) > maxDoc {
		cut := maxDoc
		for cut > 0 && !utf8.RuneStart(data[cut]) {
			cut-- // never split a character: the text would look binary
		}
		data, out.Truncated = data[:cut], true
	}
	out.Text = utf8.Valid(data) && !bytes.Contains(data, []byte{0})
	if out.Text {
		out.Body = string(data)
		if strings.HasSuffix(strings.ToLower(clean), ".md") {
			if t := firstHeading(out.Body); t != "" {
				out.Title = t
			}
		}
	}
	needle := []byte(out.StorePath)
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	for _, id := range sortedKeys(s.ix.tickets) {
		r := s.ix.tickets[id]
		hit := bytes.Contains(r.Raw, needle)
		for _, c := range r.Comments {
			if hit {
				break
			}
			if c.Comment != nil && strings.Contains(c.Comment.Body, out.StorePath) {
				hit = true
			}
		}
		if hit {
			out.Backlinks = append(out.Backlinks, r.ref())
		}
	}
	return out, nil
}

// readDoc reads at most maxDoc+1 bytes of the regular file rel (slash
// separated) under root, without leaving root. Anything else (a directory,
// a link, a device) reads as not existing.
func readDoc(root, rel string) ([]byte, int64, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	name := filepath.FromSlash(rel)
	if fi, err := r.Lstat(name); err != nil {
		return nil, 0, err
	} else if !fi.Mode().IsRegular() {
		return nil, 0, fs.ErrNotExist
	}
	f, err := r.Open(name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fs.ErrNotExist
	}
	data, err := io.ReadAll(io.LimitReader(f, maxDoc+1))
	return data, fi.Size(), err
}

func docTitle(p, name string) string {
	f, err := os.Open(p)
	if err != nil {
		return name
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	if t := firstHeading(string(buf[:n])); t != "" {
		return t
	}
	return name
}

func firstHeading(md string) string {
	for _, line := range strings.SplitN(md, "\n", 40) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}
