package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"

	ka "github.com/KanArtist/ka"
)

// EmbeddedUI is the web UI built into the binary (ui/ at the repository
// root), so the daemon serves it offline (PRD §13.1).
var EmbeddedUI, _ = fs.Sub(ka.UI, "ui")

// UIManifest is ui/ka-ui.json, written by the UI build.
type UIManifest struct {
	// API is the APIHash of the API the UI was generated against.
	API string `json:"api"`
}

// APIHash identifies the REST API's shape: a hash of the OpenAPI document
// without the version number, so a release that leaves the API alone keeps
// it, and a UI built against it still fits.
var APIHash = sync.OnceValue(func() string {
	doc, _ := json.Marshal(New(nil, Options{Version: "-"}).OpenAPI())
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:8])
})

// ReadUIManifest reads ka-ui.json from a built UI.
func ReadUIManifest(fsys fs.FS) (UIManifest, error) {
	var m UIManifest
	b, err := fs.ReadFile(fsys, "ka-ui.json")
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// uiSource is the UI the server serves and where it came from.
type uiSource struct {
	fsys   fs.FS
	origin string
}

// SetUI serves the UI from fsys from now on (origin is for logs and
// /api/v1/daemon). A nil fsys goes back to the embedded UI.
func (s *Server) SetUI(fsys fs.FS, origin string) {
	if fsys == nil {
		fsys, origin = EmbeddedUI, "embedded"
	}
	s.ui.Store(&uiSource{fsys, origin})
}

func (s *Server) uiFS() fs.FS {
	if u := s.ui.Load(); u != nil {
		return u.fsys
	}
	return EmbeddedUI
}

// UIOrigin says where the served UI comes from: "embedded" or a directory.
func (s *Server) UIOrigin() string {
	if u := s.ui.Load(); u != nil {
		return u.origin
	}
	return "embedded"
}

// nonceMarker is replaced in index.html by a per-response CSP nonce, which
// Angular uses for the component styles it injects (ngCspNonce).
const nonceMarker = "KA_CSP_NONCE"

// isUIPath reports whether a request is for the UI rather than the API.
func isUIPath(p string) bool {
	return !strings.HasPrefix(p, "/api/") && p != "/mcp" && p != "/healthz" && p != "/auth" && p != "/metrics"
}

// serveUI serves static files, and index.html for every client-side route.
// The UI's files hold no workspace data, so they need no token: without the
// cookie, the app shows how to open it with `ka open`.
func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeProblem(w, newProblem(codeForbidden, "", "method not allowed"))
		return
	}
	uiFS := s.uiFS()
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if data, err := fs.ReadFile(uiFS, name); err == nil {
			h := w.Header()
			if isHashed(name) {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				h.Set("Cache-Control", "no-cache")
			}
			http.ServeContent(w, r, name, s.started, bytes.NewReader(data))
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
	}
	index, err := fs.ReadFile(uiFS, "index.html")
	if err != nil {
		http.Error(w, "the web UI is not built into this binary (cd web && npm run build)", http.StatusNotFound)
		return
	}
	nonce := newNonce()
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-"+nonce+"'; "+
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
	h.Set("X-Frame-Options", "DENY")
	w.Write(bytes.ReplaceAll(index, []byte(nonceMarker), []byte(nonce)))
}

// isHashed reports whether a file name carries a content hash
// (main-6RWXTBIF.js), so it can be cached forever.
// hashedName matches build outputs named by content (main-D54TUWJL.js,
// chunk-Dwv1iin-.js, fonts): a name, a dash, an 8-character hash.
var hashedName = regexp.MustCompile(`^.+-[A-Za-z0-9_-]{8}\.[a-z0-9]+$`)

func isHashed(name string) bool {
	return hashedName.MatchString(path.Base(name))
}

func newNonce() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawStdEncoding.EncodeToString(b)
}
