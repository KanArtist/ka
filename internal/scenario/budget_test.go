package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/internal/render"
	"github.com/KanArtist/ka/internal/testkit"
)

var estimateTokens = render.EstimateTokens

// budget is what the model sees for one tool call: the arguments it writes
// plus the text content it reads back.
func budget(args map[string]any, text string) int {
	b, _ := json.Marshal(args)
	return estimateTokens(string(b)) + estimateTokens(text)
}

func realisticTicket(i int, parent string, blockers []string) engine.CreateIn {
	body := strings.Repeat("The incremental compiler keeps a deleted class in the merged output because the processor reads the previous file and merges into it. ", 6)
	return engine.CreateIn{
		Title:    fmt.Sprintf("Incremental compile keeps a deleted class in a merging processor's output (case %d)", i),
		Kind:     "bug",
		Priority: "P1",
		Status:   "ready",
		Labels:   []string{"cache", "compile", "processors"},
		Parent:   parent, BlockedBy: blockers,
		Links: []string{"https://github.com/jkbuild/jk/pull/912"},
		Acceptance: []string{
			"A deleted @Service class is absent from service-registry.json after an incremental compile",
			"The compile cache key covers prior generated resource output",
			"A regression test reproduces the original report",
		},
		Problem:  body,
		Approach: "Mark the processor non-incremental, or snapshot generated resources per compile and diff them.",
	}
}

// TestTokenBudgets keeps the PRD §11.3 targets as regression budgets.
func TestTokenBudgets(t *testing.T) {
	testkit.GitEnv(t)
	r := testkit.NewRemote(t, nil)
	seed := testkit.Service(t, r.Clone())
	epic := seedTickets(t, seed, engine.CreateIn{Title: "Epic: incremental compilation is always correct", Status: "backlog"})[0]
	done := seedTickets(t, seed, engine.CreateIn{Title: "Profile the compile cache", Status: "ready"})[0]
	var ins []engine.CreateIn
	for i := range 25 {
		ins = append(ins, realisticTicket(i, epic, []string{done}))
	}
	seedTickets(t, seed, ins...)
	// Close the blocker so the tickets are ready, and add some discussion.
	c := seed.CallerFor("tester", "")
	if _, err := seed.Transition(ctx, c, engine.TransitionIn{ID: done, Status: "done", Summary: "Profiled."}); err != nil {
		t.Fatal(err)
	}

	a := connect(t, testkit.Service(t, r.Clone()), "claude-code")
	targets := []struct {
		name  string
		limit int
		run   func() (map[string]any, string)
	}{
		{"ka_next with claim and digest", 800, func() (map[string]any, string) {
			args := map[string]any{"project": "jk"}
			text, _ := a.ok("ka_next", args)
			return args, text
		}},
		{"ka_comment (progress)", 100, func() (map[string]any, string) {
			_, sc := a.ok("ka_search", map[string]any{"query": "claimed:agent/claude-code"})
			id := digS(sc["tickets"].([]any)[0].(map[string]any), "id")
			args := map[string]any{"id": id, "type": "progress", "body": "Reproduced the stale registry entry; the merge reads the previous output file."}
			text, _ := a.ok("ka_comment", args)
			return args, text
		}},
		{"ka_transition to done with resolution", 150, func() (map[string]any, string) {
			_, sc := a.ok("ka_search", map[string]any{"query": "claimed:agent/claude-code"})
			id := digS(sc["tickets"].([]any)[0].(map[string]any), "id")
			args := map[string]any{"id": id, "status": "done", "summary": "Snapshot generated resources per compile; regression test added."}
			text, _ := a.ok("ka_transition", args)
			return args, text
		}},
		{"ka_search returning 20 summaries", 1300, func() (map[string]any, string) {
			args := map[string]any{"query": "label:cache", "limit": 20}
			text, _ := a.ok("ka_search", args)
			if n := strings.Count(text, "\n") + 1; n < 20 {
				t.Fatalf("search returned %d lines", n)
			}
			return args, text
		}},
	}
	for _, tg := range targets {
		args, text := tg.run()
		got := budget(args, text)
		t.Logf("%-40s %4d / %4d tokens", tg.name, got, tg.limit)
		if got > tg.limit {
			t.Errorf("%s: %d tokens, budget %d\n%s", tg.name, got, tg.limit, text)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	for s, want := range map[string]int{
		"hello world": 4, // 2 + 2 (letters/4 rounded up)
		"JK-7QK3M":    6,
		"a, b":        3,
		"line\nline":  3,
		"2026-11-07":  6,
	} {
		if got := estimateTokens(s); got != want {
			t.Errorf("%q: got %d, want %d", s, got, want)
		}
	}
}
