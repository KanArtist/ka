package api

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"
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
