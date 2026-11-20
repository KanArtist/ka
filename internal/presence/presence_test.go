package presence

import (
	"strings"
	"testing"
)

func TestSessionID(t *testing.T) {
	for in, want := range map[string]string{
		"s1":                     "s1",
		"abc-DEF_1.2:3":          "abc-DEF_1.2:3",
		"s 1/x;yé":               "s1xy",
		strings.Repeat("a", 200): strings.Repeat("a", 128),
	} {
		if got := SessionID(in); got != want {
			t.Errorf("SessionID(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SessionID("///"); len(got) != 12 {
		t.Errorf("SessionID of nothing usable = %q, want a random ID", got)
	}
}
