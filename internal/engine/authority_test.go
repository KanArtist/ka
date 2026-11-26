package engine

import (
	"testing"
	"time"
)

// A claim taken through an external authority (Syre) is read-only to other ka clients:
// next skips it, claim refuses it even when stale, and release needs force.
func TestExternalAuthorityClaimIsReadOnly(t *testing.T) {
	e := newEnv(t, nil)
	a, _ := e.clone()
	ids := must[*CreateOut](t)(a.Create(ctx, agent("lead"), []CreateIn{
		{Title: "managed by syre", Priority: "P0", Status: "ready"},
		{Title: "free", Priority: "P1", Status: "ready"},
	}))
	managed, free := ids.Created[0].ID, ids.Created[1].ID

	syre := Caller{Actor: "agent/syre", Session: "lease-1", Authority: "syre"}
	must[*ClaimOut](t)(a.Claim(ctx, syre, managed, false))

	b, _ := e.clone()
	if d := must[*Detail](t)(b.Get(GetIn{ID: managed})); d.Claim == nil || d.Claim.Authority != "syre" || d.Claim.By != "agent/syre" {
		t.Fatalf("claim as seen by another clone = %+v", d.Claim)
	}
	if n := must[*NextOut](t)(b.Next(ctx, agent("b"), NextIn{})); n.Ticket == nil || n.Ticket.ID != free {
		t.Fatalf("next must skip the authority-held ticket, got %+v", n.Ticket)
	}

	// Three days later the claim would be stale for a normal holder; an authority claim never is.
	b.Now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	_, err := b.Claim(ctx, agent("c"), managed, true)
	if !IsCode(err, CodeClaimLost) || AsError(err).Data["authority"] != "syre" {
		t.Fatalf("stealing an authority claim: %v", err)
	}
	if _, err := b.Release(ctx, agent("c"), managed, "", false); !IsCode(err, CodeNotYourClaim) {
		t.Fatalf("releasing without force: %v", err)
	}

	// The authority itself can refresh and release its own claim.
	must[*ClaimOut](t)(a.Claim(ctx, Caller{Actor: "agent/syre", Session: "lease-2", Authority: "syre"}, managed, false))
	if _, err := a.Release(ctx, syre, managed, "", false); err != nil {
		t.Fatalf("authority releasing its claim: %v", err)
	}
}
