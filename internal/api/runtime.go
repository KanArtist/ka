package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Runtime is the daemon's runtime file (PRD §8.3). Clients find a running
// daemon for their workspace through it; the token authenticates them.
type Runtime struct {
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	Addr          string `json:"addr"` // host:port
	URL           string `json:"url"`
	Workspace     string `json:"workspace"` // the workspace source (file or directory)
	WorkspaceHash string `json:"workspace_hash"`
	Token         string `json:"token"`
	Started       string `json:"started"`
}

// RuntimeDir is where runtime files live: $KA_RUNTIME_DIR, else
// $XDG_RUNTIME_DIR/ka, else the user cache directory's ka/run.
func RuntimeDir() string {
	if d := os.Getenv("KA_RUNTIME_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		return filepath.Join(d, "ka")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "ka", "run")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("ka-%d", os.Getuid()))
}

// WorkspaceHash identifies a workspace by its source path.
func WorkspaceHash(source string) string {
	if abs, err := filepath.Abs(source); err == nil {
		source = abs
	}
	if ev, err := filepath.EvalSymlinks(source); err == nil {
		source = ev
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		// Case-insensitive file systems by default.
		source = lowerASCII(source)
	}
	sum := sha256.Sum256([]byte(filepath.ToSlash(source)))
	return hex.EncodeToString(sum[:6])
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// RuntimePath is the runtime file for a workspace hash. There is one per
// workspace, so daemons for different workspaces can run side by side.
func RuntimePath(hash string) string {
	return filepath.Join(RuntimeDir(), "daemon-"+hash+".json")
}

// NewToken returns a random local token.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// WriteRuntime writes the runtime file atomically, readable only by the user.
func WriteRuntime(r *Runtime) error {
	dir := RuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	tmp, err := os.CreateTemp(dir, ".daemon-*.json")
	if err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), RuntimePath(r.WorkspaceHash))
}

// ReadRuntime reads a workspace's runtime file.
func ReadRuntime(hash string) (*Runtime, error) {
	data, err := os.ReadFile(RuntimePath(hash))
	if err != nil {
		return nil, err
	}
	var r Runtime
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %v", RuntimePath(hash), err)
	}
	return &r, nil
}

// RemoveRuntime removes the runtime file if it still belongs to this daemon.
func RemoveRuntime(r *Runtime) {
	cur, err := ReadRuntime(r.WorkspaceHash)
	if err == nil && cur.PID == r.PID && cur.Token == r.Token {
		os.Remove(RuntimePath(r.WorkspaceHash))
	}
}

// Health is GET /healthz.
type Health struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	WorkspaceHash string `json:"workspace_hash"`
}

// Probe reports whether the daemon described by r answers on its address
// for the same workspace.
func Probe(ctx context.Context, r *Runtime) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.Addr+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var h Health
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&h) != nil {
		return false
	}
	return h.WorkspaceHash == r.WorkspaceHash && h.PID == r.PID
}

// Discover returns the running daemon for a workspace, or nil.
func Discover(ctx context.Context, source string) *Runtime {
	r, err := ReadRuntime(WorkspaceHash(source))
	if err != nil || !Probe(ctx, r) {
		return nil
	}
	return r
}
