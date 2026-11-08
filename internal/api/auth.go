package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JumpKickOSS/ka/internal/engine"
)

// Remote mode (PRD §15): a daemon on a non-loopback address authenticates
// every request with a static API token or a trusted reverse proxy's user
// header. Tokens are stored hashed in ka's local config, never in git.

// PrincipalHeader carries the authenticated principal from the HTTP layer to
// handlers and the MCP server. The server always overwrites it, so clients
// cannot set it.
const PrincipalHeader = "X-Ka-Principal"

// Remote configures remote mode.
type Remote struct {
	// Tokens are the API tokens accepted (ka token create).
	Tokens *TokenStore
	// TrustedProxies may assert the user in ProxyUserHeader.
	TrustedProxies []*net.IPNet
	// ProxyUserHeader names the header a trusted proxy sets (default
	// X-Forwarded-User; X-Forwarded-Email and similar work too).
	ProxyUserHeader string
	// Hosts are the host names clients use (from --public-url); empty
	// accepts any Host, with Origin required to match it.
	Hosts []string
}

// Configured reports whether some form of authentication is set up.
func (r *Remote) Configured() bool {
	return r != nil && (r.Tokens.Count() > 0 || len(r.TrustedProxies) > 0)
}

// Token is one API token, as stored (hashed).
type Token struct {
	ID      string `json:"id"`
	Actor   string `json:"actor"`
	Hash    string `json:"hash"` // sha256 of the token, hex
	Note    string `json:"note,omitempty"`
	Created string `json:"created"`
}

// TokenStore reads tokens from a JSON file, reloading it when it changes so
// ka token create takes effect without a restart.
type TokenStore struct {
	Path    string
	mu      sync.Mutex
	mod     time.Time
	size    int64
	tokens  []Token
	checked time.Time
}

// TokensPath is where API tokens live: <ka config dir>/tokens.json.
func TokensPath() string { return filepath.Join(engine.ConfigDir(), "tokens.json") }

// NewTokenStore opens the token file at path (which may not exist yet).
func NewTokenStore(path string) *TokenStore { return &TokenStore{Path: path} }

func (ts *TokenStore) load() {
	if time.Since(ts.checked) < time.Second && ts.checked.After(ts.mod) {
		return
	}
	ts.checked = time.Now()
	st, err := os.Stat(ts.Path)
	if err != nil {
		ts.tokens, ts.mod, ts.size = nil, time.Time{}, 0
		return
	}
	if st.ModTime().Equal(ts.mod) && st.Size() == ts.size {
		return
	}
	data, err := os.ReadFile(ts.Path)
	if err != nil {
		return
	}
	var f struct {
		Tokens []Token `json:"tokens"`
	}
	if json.Unmarshal(data, &f) == nil {
		ts.tokens, ts.mod, ts.size = f.Tokens, st.ModTime(), st.Size()
	}
}

// Count returns how many tokens exist.
func (ts *TokenStore) Count() int {
	if ts == nil {
		return 0
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	return len(ts.tokens)
}

// Lookup returns the actor of a valid token.
func (ts *TokenStore) Lookup(tok string) (string, bool) {
	if ts == nil || !strings.HasPrefix(tok, "ka_") {
		return "", false
	}
	sum := sha256.Sum256([]byte(tok))
	h := hex.EncodeToString(sum[:])
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	for _, t := range ts.tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			return t.Actor, true
		}
	}
	return "", false
}

// List returns the stored tokens (hashes only).
func (ts *TokenStore) List() []Token {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.checked = time.Time{}
	ts.load()
	out := append([]Token(nil), ts.tokens...)
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

// Create makes a token for actor and returns it; only its hash is stored.
func (ts *TokenStore) Create(actor, note string) (string, Token, error) {
	if strings.TrimSpace(actor) == "" {
		return "", Token{}, fmt.Errorf("a token needs an actor (--actor)")
	}
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", Token{}, err
	}
	tok := "ka_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	sum := sha256.Sum256([]byte(tok))
	idb := make([]byte, 4)
	rand.Read(idb)
	t := Token{ID: hex.EncodeToString(idb), Actor: actor, Hash: hex.EncodeToString(sum[:]), Note: note, Created: time.Now().UTC().Format(time.RFC3339)}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.checked = time.Time{}
	ts.load()
	tokens := append(append([]Token(nil), ts.tokens...), t)
	if err := ts.save(tokens); err != nil {
		return "", Token{}, err
	}
	return tok, t, nil
}

// Remove deletes a token by ID.
func (ts *TokenStore) Remove(id string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.checked = time.Time{}
	ts.load()
	var keep []Token
	for _, t := range ts.tokens {
		if t.ID != id {
			keep = append(keep, t)
		}
	}
	if len(keep) == len(ts.tokens) {
		return fmt.Errorf("no token %q (ka token ls)", id)
	}
	return ts.save(keep)
}

func (ts *TokenStore) save(tokens []Token) error {
	if err := os.MkdirAll(filepath.Dir(ts.Path), 0o700); err != nil {
		return err
	}
	if tokens == nil {
		tokens = []Token{}
	}
	data, _ := json.MarshalIndent(struct {
		Tokens []Token `json:"tokens"`
	}{tokens}, "", "  ")
	tmp := ts.Path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, ts.Path); err != nil {
		return err
	}
	ts.tokens, ts.checked = tokens, time.Time{}
	if st, err := os.Stat(ts.Path); err == nil {
		ts.mod, ts.size = st.ModTime(), st.Size()
	}
	return nil
}

// ParseCIDRs parses --trusted-proxy values (CIDRs or single IPs).
func ParseCIDRs(vals []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.Contains(part, "/") {
				if ip := net.ParseIP(part); ip != nil {
					bits := 32
					if ip.To4() == nil {
						bits = 128
					}
					part = fmt.Sprintf("%s/%d", part, bits)
				}
			}
			_, n, err := net.ParseCIDR(part)
			if err != nil {
				return nil, fmt.Errorf("--trusted-proxy %q: %v", part, err)
			}
			out = append(out, n)
		}
	}
	return out, nil
}

// fromTrustedProxy reports whether the request's peer is a trusted proxy.
func (r *Remote) fromTrustedProxy(req *http.Request) bool {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range r.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
