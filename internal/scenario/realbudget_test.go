package scenario

import (
	"os"
	"sort"
	"testing"

	"github.com/JumpKickOSS/ka/internal/engine"
	"github.com/JumpKickOSS/ka/internal/mcpserver"
	"github.com/JumpKickOSS/ka/internal/render"
)

// TestRealStoreNextBudget renders ka_next for every ready ticket of a real
// store (KA_BUDGET_STORE=<store dir>) and checks the 800-token budget.
func TestRealStoreNextBudget(t *testing.T) {
	dir := os.Getenv("KA_BUDGET_STORE")
	if dir == "" {
		t.Skip("set KA_BUDGET_STORE to a store directory")
	}
	ws, err := engine.Open(ctx, engine.OpenOptions{Dir: dir, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := engine.New(ctx, ws)
	out, _ := svc.Search(engine.SearchIn{Query: "is:ready", Limit: 100000})
	var ns []int
	for _, s := range out.Tickets {
		d, _ := svc.Get(engine.GetIn{ID: s.ID})
		txt := render.Next(&engine.NextOut{Ticket: d, Claimed: true, Confirmed: true, Remaining: 9}, mcpserver.NextRenderOptions)
		n := estimateTokens(txt) + estimateTokens(`{"project":"jk"}`)
		ns = append(ns, n)
		if n > 800 {
			t.Errorf("%s: %d tokens", s.ID, n)
		}
	}
	sort.Ints(ns)
	if len(ns) > 0 {
		t.Logf("ka_next over %d ready tickets: p50 %d, p90 %d, max %d tokens", len(ns), ns[len(ns)/2], ns[len(ns)*9/10], ns[len(ns)-1])
	}
}
