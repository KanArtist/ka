package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Browser sign-in. A browser never holds a bearer token: GET /auth trades a
// one-time code (from POST /auth/code, which ka open calls with the local
// token) or, in remote mode, an API token for the ka_token cookie. The
// cookie's value is an HMAC under a key made at daemon start, so it is
// accepted only as a cookie, only by the REST API and the event stream, and
// only until the daemon restarts. Cookies are not scoped by port: every
// http://127.0.0.1:* page the user visits receives it, and must not get a
// token that also works for /mcp, /metrics or stopping the daemon.

const (
	// codeTTL is how long a sign-in code from POST /auth/code stays valid.
	codeTTL = 60 * time.Second
	// maxCodes bounds the codes outstanding; minting another drops the oldest.
	maxCodes = 16
	// cookieLocal is the cookie subject for the local token.
	cookieLocal = "local"
)

// cookieFor returns the cookie value for a subject (cookieLocal or an API
// token's ID); bound is the API token's hash, so a cookie dies with its token.
func (s *Server) cookieFor(subject, bound string) string {
	m := hmac.New(sha256.New, s.cookieKey)
	m.Write([]byte("ka-cookie-v1\x00" + subject + "\x00" + bound))
	return subject + "." + hex.EncodeToString(m.Sum(nil))
}

// cookieOK checks a ka_token cookie and returns its principal ("" for the
// local token).
func (s *Server) cookieOK(v string) (principal string, ok bool) {
	subject, _, found := strings.Cut(v, ".")
	if !found {
		return "", false
	}
	var want string
	if subject == cookieLocal {
		want = s.cookieFor(cookieLocal, "")
	} else if rm := s.opts.Remote; rm != nil {
		t, ok := rm.Tokens.byID(subject)
		if !ok {
			return "", false
		}
		want, principal = s.cookieFor(t.ID, t.Hash), t.Actor
	} else {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(v), []byte(want)) != 1 {
		return "", false
	}
	return principal, true
}

// signinCodes are the outstanding one-time codes, stored as SHA-256 hashes.
type signinCodes struct {
	mu    sync.Mutex
	codes []signinCode
}

type signinCode struct {
	hash    [sha256.Size]byte
	expires time.Time
}

// issue mints a code valid for codeTTL.
func (c *signinCodes) issue(now time.Time) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	code := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	if len(c.codes) >= maxCodes {
		c.codes = c.codes[1:]
	}
	c.codes = append(c.codes, signinCode{sha256.Sum256([]byte(code)), now.Add(codeTTL)})
	return code
}

// take consumes a code: it is valid at most once, and only until it expires.
func (c *signinCodes) take(code string, now time.Time) bool {
	if code == "" {
		return false
	}
	h := sha256.Sum256([]byte(code))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	for i, sc := range c.codes {
		if subtle.ConstantTimeCompare(sc.hash[:], h[:]) == 1 {
			c.codes = append(c.codes[:i], c.codes[i+1:]...)
			return true
		}
	}
	return false
}

func (c *signinCodes) prune(now time.Time) {
	keep := c.codes[:0]
	for _, sc := range c.codes {
		if now.Before(sc.expires) {
			keep = append(keep, sc)
		}
	}
	c.codes = keep
}

// SigninCode is POST /auth/code's response.
type SigninCode struct {
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// authCode mints a one-time sign-in code for GET /auth?code=. Only the local
// token may (ka open on the daemon's machine), sent as a bearer token.
func (s *Server) authCode(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.tokenOK(strings.TrimSpace(tok)) {
		writeProblem(w, newProblem(codeForbidden, "run `ka open` on the daemon's machine", "only the daemon's local token can mint sign-in codes"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(SigninCode{Code: s.codes.issue(time.Now()), ExpiresIn: int(codeTTL / time.Second)})
}

// auth sets the browser cookie and redirects to next (a path on this
// server). It takes a one-time code from POST /auth/code (ka open) or, in
// remote mode, an API token (?token=ka_…). The local token itself is not
// accepted here: it would land in browser history and on command lines.
func (s *Server) auth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var value string
	if s.codes.take(q.Get("code"), time.Now()) {
		value = s.cookieFor(cookieLocal, "")
	} else if rm := s.opts.Remote; rm != nil {
		if t, ok := rm.Tokens.find(q.Get("token")); ok {
			value = s.cookieFor(t.ID, t.Hash)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	if value == "" {
		fix := "run `ka open` again (its link works once, for a minute)"
		if s.opts.Remote != nil {
			fix = "open /auth?token=<API token> (ka token create), or run `ka open` on the daemon's machine"
		}
		writeProblem(w, newProblem(codeUnauthorized, fix, "wrong, used or expired sign-in code or token"))
		return
	}
	next := q.Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/"
	}
	secure := r.TLS != nil || (s.opts.Remote != nil && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: value, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// cookieAllowed reports whether a cookie-authenticated request may reach a
// path: the REST API and its event stream, which is all the web UI uses.
// /mcp, /metrics, minting sign-in codes and stopping the daemon need a
// bearer token.
func cookieAllowed(path string) bool {
	return strings.HasPrefix(path, "/api/") && path != "/api/v1/daemon/shutdown"
}
