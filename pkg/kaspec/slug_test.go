package kaspec

import (
	"strings"
	"testing"
)

func TestDeriveSlug(t *testing.T) {
	for title, want := range map[string]string{
		"Fix the compiler warnings":                           "fix-compiler-warnings",
		"Sync refuses a remote that shares no history":        "sync-refuses-remote",
		"Incremental compile keeps a deleted class":           "incremental-compile", // a third word would pass 24
		"Café crème: the über façade":                         "cafe-creme-uber",
		"Don't push on Ctrl-C":                                "dont-push-ctrl",
		"The":                                                 "the",
		"Internationalizationalisation of everything":         "internationalizationalis",
		"Make ka_next skip tickets blocked by open questions": "make-ka-next",
		"!!!":                       "",
		"日本語のタイトル":                  "",
		"P0: OOM in v1.2.3":         "p0-oom-v1",
		"Epic: IntelliJ just works": "intellij-just-works",
	} {
		got := DeriveSlug(title)
		if got != want {
			t.Errorf("DeriveSlug(%q) = %q, want %q", title, got, want)
		}
		if got != "" {
			if err := CheckSlug(got); err != nil {
				t.Errorf("DeriveSlug(%q) = %q is invalid: %v", title, got, err)
			}
		}
	}
}

func TestNormalizeSlug(t *testing.T) {
	for in, want := range map[string]string{
		"compiler-warnings":         "compiler-warnings",
		"Compiler Warnings":         "compiler-warnings",
		"compiler_warnings":         "compiler-warnings",
		"  --compiler--warnings-- ": "compiler-warnings",
	} {
		if got, err := NormalizeSlug(in); err != nil || got != want {
			t.Errorf("NormalizeSlug(%q) = %q, %v", in, got, err)
		}
	}
	for in, why := range map[string]string{
		"one-two-three-four-five":    "words",
		"abcdefghijklmnopqrstuvwxyz": "characters",
		"---":                        "no letters",
	} {
		if _, err := NormalizeSlug(in); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("NormalizeSlug(%q): %v, want an error about %s", in, err, why)
		}
	}
}

func TestRefID(t *testing.T) {
	for in, want := range map[string]string{
		"JK-7QK3M":                     "JK-7QK3M",
		"jk-7qk3m":                     "JK-7QK3M",
		"JK-7QK3M-compiler-warnings":   "JK-7QK3M",
		" jk-7qk3m-compiler-warnings ": "JK-7QK3M",
		"JK-7QK3M-COMPILER-WARNINGS":   "JK-7QK3M",
		"KAN-7QK3MABC-x":               "KAN-7QK3MABC",
		"JK-7QK3M-":                    "JK-7QK3M",
		"not an id":                    "NOT AN ID",
		"JK-7QK":                       "JK-7QK",
	} {
		if got := RefID(in); got != want {
			t.Errorf("RefID(%q) = %q, want %q", in, got, want)
		}
	}
	if Ref("JK-7QK3M", "") != "JK-7QK3M" || Ref("JK-7QK3M", "a-b") != "JK-7QK3M-a-b" {
		t.Error("Ref")
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		have, want string
		ok         bool
	}{
		{"1.2.0", "1.2.0", true},
		{"1.10.0", "1.2.0", true},
		{"1.1.9", "1.2.0", false},
		{"2.0.0", "1.9.9", true},
		{"v1.2.0", "1.2.0", true},
		{"1.1.0-dev", "9.0.0", true}, // development builds are never refused
		{"0.1.0-dev", "1.2.0", true},
		{"", "1.2.0", true},
		{"1.0.0", "", true},
	} {
		if got := VersionAtLeast(c.have, c.want); got != c.ok {
			t.Errorf("VersionAtLeast(%q, %q) = %v", c.have, c.want, got)
		}
	}
	if VersionAtLeastRelease("", "1.2.0") || !VersionAtLeastRelease("1.2.0", "1.2.0") {
		t.Error("VersionAtLeastRelease")
	}
}
