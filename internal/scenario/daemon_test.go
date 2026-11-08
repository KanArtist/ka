package scenario

import "testing"

// The MCP scenarios again, with every agent going through the ka mcp proxy
// to a daemon over Streamable HTTP. Behavior must be identical (PRD §8.3).
func TestScenariosViaDaemon(t *testing.T) {
	viaDaemon = true
	defer func() { viaDaemon = false }()
	t.Run("S1", TestS1FindAndClaim)
	t.Run("S4", TestS4FileFollowUps)
	t.Run("S8", TestS8CloseTheLoop)
	t.Run("S9", TestS9PointAHumanAtAnAction)
	t.Run("TokenBudgets", TestTokenBudgets)
}
