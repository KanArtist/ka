package uisrc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JumpKickOSS/ka/internal/api"
)

func fakeCheckout(t *testing.T, apiHash string) string {
	t.Helper()
	root := t.TempDir()
	ui := filepath.Join(root, "ui")
	os.MkdirAll(ui, 0o755)
	os.WriteFile(filepath.Join(ui, "index.html"), []byte("<!doctype html>"), 0o644)
	os.WriteFile(filepath.Join(ui, "ka-ui.json"), []byte(`{"api":"`+apiHash+`"}`), 0o644)
	return root
}

func TestResolve(t *testing.T) {
	t.Setenv("KA_UI_DIR", "")
	if st := Resolve(Config{}); st.Dir != "" || st.Missing {
		t.Fatalf("no checkout: %+v", st)
	}
	good := fakeCheckout(t, api.APIHash())
	if st := Resolve(Config{Checkout: good}); st.Dir != filepath.Join(good, "ui") {
		t.Fatalf("matching checkout: %+v", st)
	}
	old := fakeCheckout(t, "0000000000000000")
	if st := Resolve(Config{Checkout: old}); st.Dir != "" || st.Missing || st.Reason == "" {
		t.Fatalf("another API: %+v", st)
	}
	if st := Resolve(Config{Checkout: filepath.Join(good, "gone")}); st.Dir != "" || !st.Missing {
		t.Fatalf("deleted checkout: %+v", st)
	}
	t.Setenv("KA_UI_DIR", old)
	if st := Resolve(Config{}); st.Dir != old {
		t.Fatalf("KA_UI_DIR: %+v", st)
	}
}

func TestSaveLoadClear(t *testing.T) {
	t.Setenv("KA_DATA_DIR", t.TempDir())
	if err := Save(Config{Checkout: "/x", URL: DefaultURL}); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.Checkout != "/x" {
		t.Fatalf("load: %+v %v", c, err)
	}
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load(); c.Checkout != "" {
		t.Fatalf("after clear: %+v", c)
	}
}

// This repository is a ka checkout, and its committed ui/ fits this build.
func TestThisRepository(t *testing.T) {
	root, ok := FindCheckout(".")
	if !ok {
		t.Fatal("this repository is not recognized as a ka checkout")
	}
	if err := Check(filepath.Join(root, "ui")); err != nil {
		t.Fatalf("committed ui/: %v (run npm run gen:api && npm run build in web/)", err)
	}
}

func TestRecloneNeedsARelease(t *testing.T) {
	for _, v := range []string{"", "dev", "1.0.4-rc.1"} {
		if _, err := Reclone(t.Context(), Config{}, v); err == nil {
			t.Fatalf("version %q should not clone", v)
		}
	}
}
