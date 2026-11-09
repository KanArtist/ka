// Package uisrc decides which web UI the daemon serves: the ui/ directory
// of a ka checkout, when one is set up and was built against this binary's
// API, or the UI embedded in the binary.
//
// `ka setup` (or `ka ui use`) records the checkout in ui.json in ka's data
// directory. If that checkout is deleted, the daemon serves the embedded UI
// and makes a shallow clone of this release's tag to serve from next.
package uisrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/KanArtist/ka/internal/api"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/gitx"
)

// DefaultURL is where ka's repository lives.
const DefaultURL = "https://github.com/KanArtist/ka.git"

// Config is ui.json.
type Config struct {
	// Checkout is a ka repository whose ui/ the daemon serves.
	Checkout string `json:"checkout,omitempty"`
	// URL is cloned again when Checkout is gone.
	URL string `json:"url,omitempty"`
}

func configPath() string { return filepath.Join(engine.DataDir(), "ui.json") }

// cloneDir is where ka clones its repository when the checkout is gone.
func cloneDir() string { return filepath.Join(engine.DataDir(), "ka-repo") }

// Load reads ui.json; a missing file is an empty Config.
func Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// Save writes ui.json.
func Save(c Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), append(b, '\n'), 0o644)
}

// Clear forgets the checkout, so the embedded UI is served.
func Clear() error {
	err := os.Remove(configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Status is the outcome of Resolve.
type Status struct {
	// Dir is the ui/ directory to serve; empty means the embedded UI.
	Dir string
	// Reason says why the embedded UI is served, when it is.
	Reason string
	// Missing is true when the recorded checkout no longer exists.
	Missing bool
}

// Resolve picks the UI for this binary. KA_UI_DIR overrides everything
// (for UI development; no API check).
func Resolve(c Config) Status {
	if d := os.Getenv("KA_UI_DIR"); d != "" {
		return Status{Dir: d}
	}
	if c.Checkout == "" {
		return Status{Reason: "no checkout is set up"}
	}
	dir := filepath.Join(c.Checkout, "ui")
	if _, err := os.Stat(c.Checkout); errors.Is(err, fs.ErrNotExist) {
		return Status{Reason: c.Checkout + " no longer exists", Missing: true}
	}
	if err := Check(dir); err != nil {
		return Status{Reason: err.Error()}
	}
	return Status{Dir: dir}
}

// Check reports whether dir holds a UI built against this binary's API.
func Check(dir string) error {
	fsys := os.DirFS(dir)
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		return fmt.Errorf("%s has no built UI (index.html)", dir)
	}
	m, err := api.ReadUIManifest(fsys)
	if err != nil {
		return fmt.Errorf("%s has no ka-ui.json", dir)
	}
	if m.API != api.APIHash() {
		return fmt.Errorf("%s was built for another version of the API (check out the tag that matches `ka version`)", dir)
	}
	return nil
}

// FindCheckout returns the ka checkout containing dir, if there is one.
func FindCheckout(dir string) (string, bool) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for d := dir; ; {
		if fileExists(filepath.Join(d, "embed.go")) && fileExists(filepath.Join(d, "ui", "ka-ui.json")) && fileExists(filepath.Join(d, "go.mod")) {
			if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/KanArtist/ka\n") {
				return d, true
			}
		}
		p := filepath.Dir(d)
		if p == d {
			return "", false
		}
		d = p
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Reclone makes a shallow clone of tag (v1.2.3) into ka's data directory,
// records it as the checkout and returns its ui/. version is the binary's
// version; development builds have no tag to clone.
func Reclone(ctx context.Context, c Config, version string) (string, error) {
	if version == "" || version == "dev" || strings.Contains(version, "-") {
		return "", fmt.Errorf("version %q has no release tag to clone", version)
	}
	url := c.URL
	if url == "" {
		url = DefaultURL
	}
	dest := cloneDir()
	tmp := dest + ".new"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	g := &gitx.Repo{Dir: filepath.Dir(dest)}
	if _, err := g.Run(ctx, "clone", "-q", "--depth", "1", "--branch", "v"+version, url, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	c.Checkout, c.URL = dest, url
	if err := Save(c); err != nil {
		return "", err
	}
	dir := filepath.Join(dest, "ui")
	return dir, Check(dir)
}
