package render

import (
	"strings"
	"testing"

	"github.com/KanArtist/ka/internal/engine"
)

// The budgeted Detail measures only the description per search step; it
// must pick the same cut as measuring the whole rendering each time.
func TestDetailBudgetMatchesFullMeasure(t *testing.T) {
	d := &engine.Detail{
		Summary: engine.Summary{ID: "JK-7QK3M", Ref: "JK-7QK3M-cache", Priority: "P1", Kind: "bug", Status: "ready", Title: "Fix the cache"},
		Body:    strings.Repeat("The cache evicts entries 12345 too early; see log.\n", 200) + "ünïcode tail",
		Recent:  []engine.CommentOut{{Name: "c1", Type: "note", Author: "bob", Created: "2026-01-01", Body: strings.Repeat("word ", 100)}},
	}
	for _, budget := range []int{50, 120, 400, 1000, 3000, 100000} {
		o := DetailOptions{MaxTokens: budget, MinBody: 40}
		got := Detail(d, o)
		// The search as it was: render and measure everything per step.
		o.clip = 240
		want := detail(d, o)
		if EstimateTokens(want) > budget {
			lo, hi := min(o.MinBody, len(d.Body)), len(d.Body)
			for lo < hi {
				mid := (lo + hi + 1) / 2
				o.BodyLimit = mid
				if EstimateTokens(detail(d, o)) <= budget {
					lo = mid
				} else {
					hi = mid - 1
				}
			}
			o.BodyLimit = max(lo, 1)
			want = detail(d, o)
		}
		if got != want {
			t.Fatalf("budget %d: renderings differ (%d vs %d bytes)", budget, len(got), len(want))
		}
	}
}

func TestSyncRenamedSorted(t *testing.T) {
	s := Sync(&engine.SyncOut{Renamed: map[string]string{"JK-C": "JK-Z", "JK-A": "JK-X", "JK-B": "JK-Y"}})
	if want := "renamed JK-A → JK-X (ID collision)\nrenamed JK-B → JK-Y (ID collision)\nrenamed JK-C → JK-Z (ID collision)\n"; s != want {
		t.Fatalf("got %q", s)
	}
}
