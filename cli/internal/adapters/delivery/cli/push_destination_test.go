package cli

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type destinationPushRecorder struct {
	inbound.SyncRepo
	inputs []inbound.SyncInput
}

func (s *destinationPushRecorder) Push(_ context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	s.inputs = append(s.inputs, in)
	return inbound.SyncOutput{BackfillPending: 1}, nil
}

func TestPushExplicitOriginPreservesBackgroundHistoryPolicy(t *testing.T) {
	// Keep command-side replay probes in an empty fixture, never the developer's
	// real replica. No actual provider, network or background process is involved.
	t.Chdir(t.TempDir())
	if err := os.Mkdir(".cxt", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".cxt/config", []byte(`{"remotes":{"origin":"https://origin.invalid/acme/context"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"", "origin", "upstream"} {
		for _, branch := range []string{"", "feature"} {
			if remote == "" && branch != "" {
				continue
			}
			for _, wait := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/wait=%t", remote, branch, wait), func(t *testing.T) {
					syncer := &destinationPushRecorder{}
					resolved, wakes := 0, 0
					c := &Container{
						Sync: syncer,
						ResolveSyncDestination: func(_ context.Context, _ string, name string) (SyncDestination, error) {
							resolved++
							if name != remote {
								t.Fatalf("resolved %q, want %q", name, remote)
							}
							return SyncDestination{Sync: syncer}, nil
						},
						WakeHistoricalSync: func(string) { wakes++ },
					}
					var selectedID string
					if branch != "" {
						_, local, _, _, _, id := manualPublicationFixture(t, branch)
						c.Queries, c.History, c.ResolveRepo = local.Queries, local.History, local.ResolveRepo
						selectedID = id
					}
					args := []string{"cxt", "push"}
					if remote != "" {
						args = append(args, remote)
					}
					if branch != "" {
						args = append(args, branch)
					}
					if wait {
						args = append(args, "--wait-history")
					}
					if err := Run(c, args); err != nil {
						t.Fatal(err)
					}
					wantResolved := 1
					if remote == "" {
						wantResolved = 0
					}
					if resolved != wantResolved || len(syncer.inputs) != 1 {
						t.Fatal("wrong sync destination", resolved, len(syncer.inputs))
					}
					in := syncer.inputs[0]
					if in.Ref != "" || in.ForegroundOnly != ((remote == "" || remote == "origin") && !wait) {
						t.Fatalf("push changed target or wait policy: ref=%q foreground=%t", in.Ref, in.ForegroundOnly)
					}
					if branch == "" && in.Publication != nil || branch != "" && (in.Publication == nil || in.Publication.HistoryOnly || len(in.Publication.Branches) != 1 || in.Publication.Branches[0].Branch != branch || in.Publication.Branches[0].BranchID != selectedID) {
						t.Fatalf("wrong explicit publication scope: %+v", in.Publication)
					}
					wantWakes := 0
					if remote == "" || remote == "origin" {
						wantWakes = 1
					}
					if wakes != wantWakes {
						t.Fatalf("origin worker wakes=%d, want %d", wakes, wantWakes)
					}
					if c.WakeHistoricalSync == nil {
						t.Fatal("destination selection mutated original container")
					}
				})
			}
		}
	}
}
