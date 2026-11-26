package kaservice_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/KanArtist/ka/internal/testkit"
	"github.com/KanArtist/ka/pkg/kaservice"
)

// An embedding program opens a store through its own workspace file, writes as an
// authority, and another clone sees the read-only claim after syncing.
func TestEmbedOpenWriteAndAuthorityClaim(t *testing.T) {
	testkit.GitEnv(t)
	ctx := context.Background()
	remote := testkit.NewRemote(t, nil)
	replica := remote.Clone()

	wsFile := filepath.Join(t.TempDir(), "workspace.yml")
	if err := os.WriteFile(wsFile, []byte("name: syre\nstores:\n  - name: test\n    path: "+replica+"\nidentity:\n  actor: agent/syre\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := kaservice.Open(ctx, kaservice.OpenOptions{Workspace: wsFile})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := kaservice.New(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	syre := kaservice.Caller{Actor: "agent/syre", Session: "lease-1", Authority: "syre"}
	out, err := svc.Create(ctx, syre, []kaservice.CreateIn{{Title: "From Syre", Status: "ready", Priority: "P1"}})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Created[0].ID
	if _, err := svc.Claim(ctx, syre, id, false); err != nil {
		t.Fatal(err)
	}

	other := testkit.Service(t, remote.Clone())
	d, err := other.Get(kaservice.GetIn{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if d.Claim == nil || d.Claim.Authority != "syre" {
		t.Fatalf("other clone sees claim %+v, want authority syre", d.Claim)
	}
	if _, err := other.Claim(ctx, kaservice.Caller{Actor: "agent/claude"}, id, true); !kaservice.IsCode(err, kaservice.CodeClaimLost) {
		t.Fatalf("other client stealing an authority claim: %v", err)
	}
}
