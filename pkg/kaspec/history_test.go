package kaspec

import (
	"reflect"
	"strings"
	"testing"
)

func TestHistorySummaryRoundTrip(t *testing.T) {
	h := &HistorySummary{
		CompactedAt: "2026-11-07T12:00:00Z", By: "agent/claude", OnBehalfOf: "bsant",
		From: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), Commits: 12,
		FirstCommit: "2026-01-01T00:00:00Z", LastCommit: "2026-11-07T11:59:59Z",
		Actors:  map[string]int64{"bsant": 4, "agent/claude": 8, "Test User": 1},
		Created: 3, Closed: 2, Previous: "20260101T000000Z-4F2A.md",
		Body: "What happened: the cache rewrite.\n",
	}
	data := FormatHistorySummary(h)
	got, ds := ParseHistorySummary("history/x.md", data)
	if len(ds) > 0 {
		t.Fatalf("diagnostics on canonical output: %v\n%s", ds, data)
	}
	got.Body, h.Body = canonicalBody(got.Body), canonicalBody(h.Body)
	if !reflect.DeepEqual(got, h) {
		t.Fatalf("round trip:\n%+v\n%+v\n%s", got, h, data)
	}
	if !strings.Contains(string(data), "\"Test User\": 1") && !strings.Contains(string(data), "Test User: 1") {
		t.Fatalf("actor keys: %s", data)
	}
	// Without a narrative there is no body; zero counts are omitted.
	h2 := &HistorySummary{CompactedAt: h.CompactedAt, By: "bsant", From: h.From, Tree: h.Tree}
	if s := string(FormatHistorySummary(h2)); !strings.HasSuffix(s, "commits: 0\n---\n") {
		t.Fatalf("minimal summary:\n%s", s)
	}
}
