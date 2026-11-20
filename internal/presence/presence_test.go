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

// Presence publishes a derived ID: stable for a session, different between
// sessions, and not the MCP session ID, which works as a credential.
func TestPublicID(t *testing.T) {
	const raw = "N4ZQ3JX7VQYDUTVFKZ2F3W5H4K"
	id := PublicID(raw)
	if len(id) != 12 || id == raw || strings.Contains(raw, id) || PublicID(raw) != id || PublicID(raw+"x") == id {
		t.Fatalf("PublicID(%q) = %q", raw, id)
	}
	if SessionID(id) != id {
		t.Fatalf("PublicID %q changes as a Ka-Session value", id)
	}
	tr := New(nil)
	tr.Seen(raw, Session{Actor: "agent/x"}, Call{Method: "tools/list"})
	tr.Seen(raw, Session{Actor: "agent/x"}, Call{Method: "tools/call", Tool: "ka_next"})
	if l := tr.List(); len(l) != 1 || l[0].ID != id || l[0].Calls != 2 {
		t.Fatalf("list: %+v", l)
	}
	tr.Closed(raw)
	if len(tr.List()) != 0 {
		t.Fatal("closing by the session ID failed")
	}
}
