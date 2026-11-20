package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KanArtist/ka/internal/testkit"
)

// signin asks the daemon for a one-time code with the local token, as ka
// open does.
func (c *client) signinCode() string {
	c.t.Helper()
	r := c.ok("POST", "/auth/code", nil)
	code := str(r.m, "code")
	if code == "" || r.m["expires_in"] != float64(60) || r.hdr.Get("Cache-Control") != "no-store" {
		c.t.Fatalf("auth/code: %s", r.raw)
	}
	return code
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func authCookie(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, ck := range res.Cookies() {
		if ck.Name == CookieName {
			return ck
		}
	}
	return nil
}

// A browser signs in with a one-time code that ka open mints with the local
// token, and gets a cookie that is not a bearer token and reaches only the
// REST API and the event stream.
func TestBrowserSignin(t *testing.T) {
	testkit.GitEnv(t)
	var logs bytes.Buffer
	c := serveWith(t, testkit.Service(t, testkit.NewRemote(t, nil).Clone()),
		Options{Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), MCP: http.NotFoundHandler()})

	// Only the local token, as a bearer token, mints codes.
	if r := c.do("POST", "/auth/code", nil, "Authorization", ""); r.status != 401 {
		t.Fatalf("auth/code without a token: %d", r.status)
	}
	code := c.signinCode()

	// The code sets an HttpOnly, SameSite=Strict cookie and redirects only to
	// local paths.
	res, err := noRedirect.Get(c.base + "/auth?" + url.Values{"code": {code}, "next": {"//evil.example/"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		t.Fatalf("auth redirect: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	cookie := authCookie(t, res)
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Secure {
		t.Fatalf("cookie: %+v", cookie)
	}
	if strings.Contains(cookie.Value, testToken) || strings.Contains(cookie.Value, code) {
		t.Fatalf("cookie carries a secret: %q", cookie.Value)
	}
	// A code works once.
	if res, _ = noRedirect.Get(c.base + "/auth?code=" + code); res.StatusCode != 401 || authCookie(t, res) != nil {
		t.Fatalf("second use of a code: %d", res.StatusCode)
	}
	res.Body.Close()
	for _, q := range []string{"code=wrong", "code=", "token=" + testToken} { // the local token is not taken here
		if res, _ = noRedirect.Get(c.base + "/auth?" + q); res.StatusCode != 401 || authCookie(t, res) != nil {
			t.Fatalf("auth?%s: %d", q, res.StatusCode)
		}
		res.Body.Close()
	}

	withCookie := func(method, path, value string) int {
		t.Helper()
		req, _ := http.NewRequest(method, c.base+path, nil)
		req.AddCookie(&http.Cookie{Name: CookieName, Value: value})
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	// The cookie reaches the REST API and the event stream...
	if st := withCookie("GET", "/api/v1/workspace", cookie.Value); st != 200 {
		t.Fatalf("cookie on the API: %d", st)
	}
	s := c.subscribe("", "Authorization", "", "Cookie", CookieName+"="+cookie.Value)
	s.cancel()
	// ...but not /mcp, /metrics, minting codes or stopping the daemon.
	for _, p := range []string{"GET /mcp", "POST /mcp", "GET /metrics", "POST /auth/code", "POST /api/v1/daemon/shutdown"} {
		method, path, _ := strings.Cut(p, " ")
		if st := withCookie(method, path, cookie.Value); st != 403 {
			t.Fatalf("cookie on %s: %d", p, st)
		}
	}
	// The cookie is not a bearer token, and the local token is not a cookie.
	if r := c.do("GET", "/api/v1/workspace", nil, "Authorization", "Bearer "+cookie.Value); r.status != 401 {
		t.Fatalf("cookie as bearer: %d", r.status)
	}
	if st := withCookie("GET", "/api/v1/workspace", testToken); st != 401 {
		t.Fatalf("local token as cookie: %d", st)
	}
	if st := withCookie("GET", "/api/v1/workspace", "local.00"); st != 401 {
		t.Fatalf("forged cookie: %d", st)
	}
	// A cookie from another daemon (another start) does not work here.
	other := New(nil, Options{Token: testToken})
	if st := withCookie("GET", "/api/v1/workspace", other.cookieFor(cookieLocal, "")); st != 401 {
		t.Fatalf("another daemon's cookie: %d", st)
	}

	// Neither the code nor the token reaches the request log.
	if l := logs.String(); !strings.Contains(l, "path=/auth") || strings.Contains(l, code) || strings.Contains(l, testToken) || strings.Contains(l, cookie.Value) {
		t.Fatalf("request log:\n%s", l)
	}
	if m := c.ok("GET", "/metrics", nil); strings.Contains(string(m.raw), code) {
		t.Fatal("code in metrics")
	}
}

// Codes expire after a minute, and only a bounded number are outstanding.
func TestSigninCodes(t *testing.T) {
	var sc signinCodes
	now := time.Now()
	a := sc.issue(now)
	if sc.take(a, now.Add(codeTTL+time.Second)) {
		t.Fatal("expired code accepted")
	}
	if len(sc.codes) != 0 {
		t.Fatal("expired code kept")
	}
	b := sc.issue(now)
	for range maxCodes {
		sc.issue(now)
	}
	if len(sc.codes) != maxCodes || sc.take(b, now) {
		t.Fatalf("oldest code not dropped: %d outstanding", len(sc.codes))
	}
	c := sc.issue(now)
	if !sc.take(c, now.Add(codeTTL-time.Second)) || sc.take(c, now) {
		t.Fatal("a code works exactly once")
	}
	for _, code := range []string{"", strings.Repeat("0", 64)} {
		if sc.take(code, now) {
			t.Fatalf("code %q accepted", code)
		}
	}
}
