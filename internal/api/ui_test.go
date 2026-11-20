package api

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The UI is served from the binary, without the token, with a strict CSP
// whose nonce replaces the marker in index.html.
func TestServesUI(t *testing.T) {
	c := start(t)
	noAuth := []string{"Authorization", ""}
	for _, p := range []string{"/", "/activity", "/ticket/JK-12345"} {
		r := c.do("GET", p, nil, noAuth...)
		csp := r.hdr.Get("Content-Security-Policy")
		if r.status != 200 || !strings.Contains(string(r.raw), "<app-root") || strings.Contains(string(r.raw), nonceMarker) {
			t.Fatalf("%s: %d %.200s", p, r.status, r.raw)
		}
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "'nonce-") || strings.Contains(csp, "unsafe-inline") {
			t.Fatalf("%s: CSP %q", p, csp)
		}
	}
	// Hashed assets are cached forever; unknown files are 404, not index.html.
	ents, _ := fs.ReadDir(EmbeddedUI, ".")
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "main-") {
			r := c.do("GET", "/"+e.Name(), nil, noAuth...)
			if r.status != 200 || !strings.Contains(r.hdr.Get("Cache-Control"), "immutable") {
				t.Fatalf("%s: %d %q", e.Name(), r.status, r.hdr.Get("Cache-Control"))
			}
		}
	}
	if r := c.do("GET", "/nope.js", nil, noAuth...); r.status != 404 {
		t.Fatalf("missing asset: %d", r.status)
	}
	if r := c.do("POST", "/", nil, noAuth...); r.status == 200 {
		t.Fatal("POST / accepted")
	}
	// The API itself still needs the token, and foreign hosts get nothing.
	if r := c.do("GET", "/api/v1/workspace", nil, noAuth...); r.status != 401 {
		t.Fatalf("api without token: %d", r.status)
	}
	req, _ := http.NewRequest("GET", c.base+"/", nil)
	req.Host = "evil.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 403 {
		t.Fatalf("UI on a foreign Host: %v %v", res.StatusCode, err)
	}
	res.Body.Close()
}

func TestIsHashed(t *testing.T) {
	for name, want := range map[string]bool{
		"main-D54TUWJL.js": true, "chunk-Dwv1iin-.js": true, "chunk-B8_c6LW1.js": true, "styles-RIOS7O6L.css": true,
		"media/instrument-sans-latin-wght-normal-Ab12Cd34.woff2": true,
		"favicon.svg": false, "index.html": false, "3rdpartylicenses.txt": false,
	} {
		if got := isHashed(name); got != want {
			t.Errorf("isHashed(%q) = %v", name, got)
		}
	}
}

// Other methods on UI paths are 405, with Allow.
func TestUIMethodNotAllowed(t *testing.T) {
	c := start(t)
	r := c.do("POST", "/board", nil, "Authorization", "")
	if r.status != 405 || r.hdr.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST /board: %d %q", r.status, r.hdr.Get("Allow"))
	}
}

// Assets from a directory carry the file's own time and an ETag that
// changes with it, so a rebuilt UI is not answered with 304; embedded ones
// carry a content ETag.
func TestUIAssetValidators(t *testing.T) {
	c := start(t)
	noAuth := []string{"Authorization", ""}
	dir := t.TempDir()
	file := filepath.Join(dir, "favicon.svg")
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.WriteFile(file, []byte("<svg>old</svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(file, old, old)
	c.srv.SetUI(os.DirFS(dir), dir)
	r := c.do("GET", "/favicon.svg", nil, noAuth...)
	if r.status != 200 || r.hdr.Get("Last-Modified") != old.Format(http.TimeFormat) || r.hdr.Get("ETag") == "" {
		t.Fatalf("dir asset: %d %v", r.status, r.hdr)
	}
	tag := r.hdr.Get("ETag")
	if r := c.do("GET", "/favicon.svg", nil, append(noAuth, "If-None-Match", tag)...); r.status != 304 {
		t.Fatalf("unchanged: %d", r.status)
	}
	if err := os.WriteFile(file, []byte("<svg>new</svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	newer := old.Add(time.Hour)
	os.Chtimes(file, newer, newer)
	r = c.do("GET", "/favicon.svg", nil, append(noAuth, "If-None-Match", tag)...)
	if r.status != 200 || string(r.raw) != "<svg>new</svg>" {
		t.Fatalf("changed: %d %s", r.status, r.raw)
	}
	if r := c.do("GET", "/favicon.svg", nil, append(noAuth, "If-Modified-Since", old.Format(http.TimeFormat))...); r.status != 200 {
		t.Fatalf("changed since: %d", r.status)
	}

	c.srv.SetUI(nil, "")
	ents, _ := fs.ReadDir(EmbeddedUI, ".")
	for _, e := range ents {
		if e.IsDir() || e.Name() == "index.html" {
			continue
		}
		r := c.do("GET", "/"+e.Name(), nil, noAuth...)
		tag := r.hdr.Get("ETag")
		if r.status != 200 || tag == "" {
			t.Fatalf("embedded %s: %d %q", e.Name(), r.status, tag)
		}
		if r := c.do("GET", "/"+e.Name(), nil, append(noAuth, "If-None-Match", tag)...); r.status != 304 {
			t.Fatalf("embedded %s revalidated: %d", e.Name(), r.status)
		}
		break
	}
}
