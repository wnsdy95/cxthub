//go:build postgres

package store

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGraphInputsKeepDeterministicOrderAfterRowUpdates(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	var snapshotIDs []domain.ContentHash
	for index := range 4 {
		snapshot := snapshotIdentityMetadataPG(t, ctx, storage, repo, domain.DocumentIdentityLegacy, fmt.Sprintf("snapshot-%d", index))
		snapshotIDs = append(snapshotIDs, snapshot.ID)
	}
	slices.Sort(snapshotIDs)
	createdAt := time.Date(2026, time.October, 15, 0, 0, 0, 0, time.UTC)
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET created_at=$2 WHERE repo_id=$1`, string(repo), createdAt)
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET created_at=$3 WHERE repo_id=$1 AND id=$2`, string(repo), string(snapshotIDs[3]), createdAt.Add(time.Hour))
	wantSnapshots := append([]domain.ContentHash{snapshotIDs[3]}, snapshotIDs[:3]...)
	for _, key := range []struct{ kind, name string }{{"tag", "zeta"}, {"branch", "zeta"}, {"tag", "alpha"}, {"branch", "alpha"}} {
		catalogExecPG(t, ctx, storage.pool, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,$2,$3,$4)`, string(repo), key.kind, key.name, string(snapshotIDs[0]))
	}
	for _, session := range []string{"zeta", "middle", "alpha"} {
		if err := storage.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: session, Target: snapshotIDs[0]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []struct{ user, branch string }{{"zeta", "zeta"}, {"alpha", "zeta"}, {"zeta", "alpha"}, {"alpha", "alpha"}} {
		if err := storage.PutUnsync(ctx, repo, domain.Unsync{RepoID: repo, User: key.user, Branch: key.branch, Target: snapshotIDs[0]}); err != nil {
			t.Fatal(err)
		}
	}
	for iteration := range 3 {
		for _, branch := range []string{"", domain.StashBranchLabel} {
			snapshots, err := storage.ListSnapshots(ctx, repo, branch)
			if err != nil {
				t.Fatal(err)
			}
			var got []domain.ContentHash
			for _, snapshot := range snapshots {
				got = append(got, snapshot.ID)
			}
			if !reflect.DeepEqual(got, wantSnapshots) {
				t.Errorf("iteration %d branch %q snapshot order = %v, want %v", iteration, branch, got, wantSnapshots)
			}
		}
		refs, err := storage.listRefsRaw(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		var refKeys []string
		for _, ref := range refs {
			refKeys = append(refKeys, string(ref.Kind)+"/"+ref.Name)
		}
		if want := []string{"branch/alpha", "branch/zeta", "tag/alpha", "tag/zeta"}; !reflect.DeepEqual(refKeys, want) {
			t.Errorf("iteration %d ref order = %v, want %v", iteration, refKeys, want)
		}
		pendings, err := storage.ListPendings(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		var pendingKeys []string
		for _, pending := range pendings {
			pendingKeys = append(pendingKeys, pending.SessionID)
		}
		if want := []string{"alpha", "middle", "zeta"}; !reflect.DeepEqual(pendingKeys, want) {
			t.Errorf("iteration %d pending order = %v, want %v", iteration, pendingKeys, want)
		}
		unsyncs, err := storage.ListUnsyncs(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		var unsyncKeys []string
		for _, unsync := range unsyncs {
			unsyncKeys = append(unsyncKeys, unsync.User+"/"+unsync.Branch)
		}
		if want := []string{"alpha/alpha", "alpha/zeta", "zeta/alpha", "zeta/zeta"}; !reflect.DeepEqual(unsyncKeys, want) {
			t.Errorf("iteration %d unsync order = %v, want %v", iteration, unsyncKeys, want)
		}
		for _, snapshot := range snapshotIDs {
			catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message=message WHERE repo_id=$1 AND id=$2`, string(repo), string(snapshot))
		}
		catalogExecPG(t, ctx, storage.pool, `UPDATE refs SET target=target WHERE repo_id=$1 AND kind='branch' AND name='alpha'`, string(repo))
		catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=data WHERE repo_id=$1 AND session_id='alpha'`, string(repo))
		catalogExecPG(t, ctx, storage.pool, `UPDATE unsync_contexts SET data=data WHERE repo_id=$1 AND username='alpha'`, string(repo))
	}
}
