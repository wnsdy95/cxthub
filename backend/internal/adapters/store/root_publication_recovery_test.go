package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRootFSPublicationInterruptedAfterManifest(t *testing.T) {
	for _, corruptRetry := range []bool{false, true} {
		name := "resume"
		if corruptRetry {
			name = "corrupt-current-dependency"
		}
		t.Run(name, func(t *testing.T) {
			ctx := rootPublicationContext(context.Background())
			st := NewFSStore(t.TempDir())
			fixture := rootFixture(t, "interrupted root publication")
			claim, proof := rootPublicationClaimFS(t, st, fixture)
			// A non-directory index parent deterministically fails after the root
			// file is installed, before the completed receipt can be persisted.
			blocked := filepath.Dir(st.readIndexPath(claim.RepoID, fixture.hash))
			if err := os.WriteFile(blocked, []byte("injected index failure"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := st.CompleteDocJob(ctx, claim, proof, time.Now()); err == nil {
				t.Fatal("injected failure acknowledged completion")
			}
			if _, err := st.ReadVerifiedDoc(ctx, claim.RepoID, fixture.hash); err != nil {
				t.Fatal("root was not installed before injected failure", err)
			}
			if err := os.Remove(blocked); err != nil {
				t.Fatal(err)
			}
			fsDocQueues.Delete(st.dataDir) // Simulate loss of the process scheduler.
			reopened := NewFSStore(st.dataDir)
			pending, err := reopened.GetDocJob(ctx, claim.RepoID, claim.ID)
			if err != nil || pending.State != "running" || pending.Version != claim.Version {
				t.Fatal("failed completion lost its durable claim", pending, err)
			}
			if snaps, err := reopened.ListSnapshots(ctx, claim.RepoID, ""); err != nil || len(snaps) != 0 {
				t.Fatal("preparation fabricated a committed snapshot", snaps, err)
			}
			for hash := range fixture.bodies {
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(reopened.chunkPath(claim.RepoID, hash), old, old); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := reopened.RepackDocs(); err != nil {
				t.Fatal(err)
			}
			next, err := reopened.ClaimDocJob(ctx, claim.RepoID, claim.LeaseUntil.Add(time.Second), time.Minute)
			if err != nil || next.Version <= claim.Version {
				t.Fatal("restart failed to reclaim pending root", next, err)
			}
			if err := reopened.CompleteDocJob(ctx, claim, proof, time.Now()); !errors.Is(err, domain.ErrConflict) {
				t.Fatal("stale process acknowledged the reclaimed job", err)
			}
			if corruptRetry {
				path := reopened.chunkPath(claim.RepoID, fixture.manifest.Chunks[0].Hash)
				original, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("corrupt after restart"), 0600); err != nil {
					t.Fatal(err)
				}
				before := rootFSImage(t, reopened.dataDir)
				if err := reopened.CompleteDocJob(ctx, next, proof, time.Now()); err == nil {
					t.Fatal("old proof hid changed bytes after restart")
				}
				if after := rootFSImage(t, reopened.dataDir); !reflect.DeepEqual(before, after) {
					t.Fatal("failed replay repaired bytes or changed durable state")
				}
				pending, err := reopened.GetDocJob(ctx, claim.RepoID, claim.ID)
				if err != nil || pending.State != "running" || pending.Version != next.Version {
					t.Fatal("failed replay discarded recovery evidence", pending, err)
				}
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := reopened.CompleteDocJob(ctx, next, proof, time.Now()); err != nil {
				t.Fatal("verified retry did not recover", err)
			}
			done, err := reopened.GetDocJob(ctx, claim.RepoID, claim.ID)
			if err != nil || done.State != "completed" || done.DocumentRef() != proof.DocumentRef() {
				t.Fatal("bad recovered receipt", done, err)
			}
			current, err := reopened.ReadVerifiedDoc(ctx, claim.RepoID, fixture.hash)
			if err != nil {
				t.Fatal(err)
			}
			assertRootProof(t, current, fixture)
		})
	}
}
