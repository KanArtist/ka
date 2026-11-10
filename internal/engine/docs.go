package engine

import (
	"bytes"
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
// project is empty), sorted by project and path.
func (s *Service) Docs(project string) (*DocsOut, error) {
	s.ix.catchUp()
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	out := &DocsOut{Docs: []DocEntry{}}
	if project != "" && s.ix.projects[project] == nil {
		return nil, Errf(CodeUnknownProject, "projects: "+strings.Join(sortedKeys(s.ix.projects), ", "), "no project %q", project)
	}
	for _, k := range sortedKeys(s.ix.projects) {
		if project != "" && k != project {
			continue
		}
		pi := s.ix.projects[k]
		root := filepath.Join(pi.Store.Root, filepath.FromSlash(pi.Dir), "docs")
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			info, _ := d.Info()
			e := DocEntry{Project: k, Path: filepath.ToSlash(rel), Title: d.Name()}
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

// Doc returns one project doc and its backlinks.
func (s *Service) Doc(project, rel string) (*DocOut, error) {
	s.ix.catchUp()
	s.ix.mu.RLock()
	defer s.ix.mu.RUnlock()
	pi := s.ix.projects[project]
	if pi == nil {
		return nil, Errf(CodeUnknownProject, "projects: "+strings.Join(sortedKeys(s.ix.projects), ", "), "no project %q", project)
	}
	clean := path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))[1:]
	if clean == "" || clean == "." || strings.HasPrefix(clean, "../") {
		return nil, Errf(CodeInvalid, "pass a path inside the project's docs/ directory", "bad doc path %q", rel)
	}
	root := filepath.Join(pi.Store.Root, filepath.FromSlash(pi.Dir), "docs")
	p := filepath.Join(root, filepath.FromSlash(clean))
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return nil, Errf(CodeNotFound, "list docs with GET /api/v1/docs?project="+project, "no doc %s in project %s", clean, project)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	out := &DocOut{DocEntry: DocEntry{Project: project, Path: clean, Title: path.Base(clean), Size: info.Size()}, Store: pi.Store.Name,
		StorePath: pi.Dir + "/docs/" + clean, Backlinks: []Ref{}}
	if len(data) > maxDoc {
		data, out.Truncated = data[:maxDoc], true
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
