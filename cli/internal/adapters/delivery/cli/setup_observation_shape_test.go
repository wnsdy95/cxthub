package cli

import (
	"context"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Root parents arrive as [] from the real backend. The query's defensive copy
// must preserve that exact shape so final admission can compare all metadata.
func TestSetupTrackingPreservesObservedParentShape(t *testing.T) {
	for _, parents := range [][]domain.ContentHash{nil, {}} {
		name := "empty"
		if parents == nil {
			name = "nil"
		}
		t.Run(name, func(t *testing.T) {
			f := setupTrackingFixture(t)
			f.remote.snaps[0].Parents = parents
			ctx := context.Background()
			if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); err != nil {
				t.Fatalf("verified root shape rejected by first attachment: %v", err)
			}
			cached, err := f.store.ReadRemoteObservation(ctx, f.repo, "configured")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, snap := range cached.Snapshots {
				if snap.ID == f.a {
					found = true
					if !reflect.DeepEqual(snap.Parents, parents) {
						t.Fatalf("cached evidence shape changed: nil=%v want nil=%v", snap.Parents == nil, parents == nil)
					}
				}
			}
			if !found {
				t.Fatal("verified root was omitted")
			}
			p, err := f.store.GetWorkingPosition(ctx)
			if err != nil || p.Snapshot != f.a || p.MemoryHash != f.m1 || p.BranchID != f.remote.ref.BranchID || f.remote.pulls != 1 {
				t.Fatalf("exact attachment failed: %+v pulls=%d err=%v", p, f.remote.pulls, err)
			}
		})
	}
}
