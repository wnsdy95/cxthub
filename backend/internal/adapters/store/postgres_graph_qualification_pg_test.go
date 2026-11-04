//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphQualificationPG(t *testing.T) (*PostgresStore, context.Context, domain.GraphQualificationKey) {
	t.Helper()
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graphProjectionPublishPG(t, ctx, storage, job)
	if err := storage.ConfigureGraphQualification(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	read, err := storage.ReadGraphProjectionStatus(ctx, repo)
	if err != nil || !read.Ready {
		t.Fatalf("qualification fixture not ready: %+v %v", read, err)
	}
	return storage, ctx, read.QualificationKey()
}

func graphQualificationClaimPG(t *testing.T, ctx context.Context, storage *PostgresStore, key domain.GraphQualificationKey) domain.GraphQualificationLease {
	t.Helper()
	lease, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute)
	if err != nil || lease.Validate() != nil || lease.GraphQualificationKey != key {
		t.Fatalf("qualification claim: %+v %v", lease, err)
	}
	return lease
}

func graphQualificationResultPG(t *testing.T, ctx context.Context, storage *PostgresStore, key domain.GraphQualificationKey, matched bool) domain.GraphQualificationResult {
	t.Helper()
	read, err := storage.ReadGraphProjection(ctx, key.RepoID)
	if err != nil || !read.Complete || read.QualificationKey() != key {
		t.Fatalf("qualification digest fixture unavailable: %+v %v", read, err)
	}
	digest, err := domain.GraphViewProjectionManifestDigest(ctx, key.RepoID, read.Parts)
	if err != nil {
		t.Fatal(err)
	}
	result := domain.GraphQualificationResult{GraphQualificationKey: key, Expected: digest, Actual: digest}
	if !matched {
		result.Expected = domain.HashContent([]byte("different graph digest"))
	}
	return result
}

func graphQualificationSnapshotPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) []byte {
	t.Helper()
	var snapshot []byte
	if err := storage.pool.QueryRow(ctx, `SELECT (SELECT to_jsonb(qualification) || jsonb_build_object('xmin',qualification.xmin::text)
		FROM graph_qualification qualification WHERE repo_id=$1)`, string(repo)).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func graphQualificationDuePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) {
	t.Helper()
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_qualification SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
}

func graphQualificationAdvancePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.GraphQualificationKey {
	t.Helper()
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message=clock_timestamp()::text WHERE repo_id=$1`, string(repo))
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graphProjectionPublishPG(t, ctx, storage, job)
	read, err := storage.ReadGraphProjectionStatus(ctx, repo)
	if err != nil || !read.Ready {
		t.Fatalf("advanced qualification fixture not ready: %+v %v", read, err)
	}
	return read.QualificationKey()
}

func TestPGGraphQualificationConfigureNeverCreatesOrEnablesProjection(t *testing.T) {
	storage, ctx, repo := catalogPG(t)
	if err := storage.ConfigureGraphQualification(ctx, repo, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("configuration without graph work: %v", err)
	}
	if snapshot := graphProjectionSnapshotPG(t, ctx, storage, repo); len(snapshot.work) != 0 || len(graphQualificationSnapshotPG(t, ctx, storage, repo)) != 0 {
		t.Fatal("qualification created projection work")
	}
	if err := storage.ConfigureGraphProjection(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	source := graphProjectionRepairSourcePG(t, ctx, storage, repo)
	if err := storage.ConfigureGraphQualification(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	qualification := graphQualificationSnapshotPG(t, ctx, storage, repo)
	if err := storage.ConfigureGraphQualification(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimGraphQualification(ctx, repo, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disabled graph was qualified: %v", err)
	}
	if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) || !reflect.DeepEqual(source, graphProjectionRepairSourcePG(t, ctx, storage, repo)) ||
		!bytes.Equal(qualification, graphQualificationSnapshotPG(t, ctx, storage, repo)) {
		t.Fatal("qualification configuration changed graph/source or was not idempotent")
	}
}

func TestPGGraphQualificationClaimsCoalesceExpireAndResume(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	replica, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	type outcome struct {
		lease domain.GraphQualificationLease
		err   error
	}
	const attempts = 8
	outcomes := make(chan outcome, attempts)
	for attempt := 0; attempt < attempts; attempt++ {
		worker := []*PostgresStore{storage, replica}[attempt%2]
		go func() {
			lease, err := worker.ClaimGraphQualification(ctx, key.RepoID, time.Minute)
			outcomes <- outcome{lease, err}
		}()
	}
	var lease domain.GraphQualificationLease
	claimed := 0
	for attempt := 0; attempt < attempts; attempt++ {
		result := <-outcomes
		if result.err == nil {
			claimed++
			lease = result.lease
		} else if !errors.Is(result.err, domain.ErrNotFound) || result.lease != (domain.GraphQualificationLease{}) {
			t.Fatalf("concurrent claim failed unexpectedly: %+v", result)
		}
	}
	if claimed != 1 || lease.Validate() != nil || lease.GraphQualificationKey != key {
		t.Fatalf("claim was not exclusive: count=%d lease=%+v", claimed, lease)
	}
	var cooldown bool
	if err := storage.pool.QueryRow(ctx, `SELECT next_attempt>clock_timestamp()+interval '59 seconds' FROM graph_qualification WHERE repo_id=$1`, string(key.RepoID)).Scan(&cooldown); err != nil || !cooldown {
		t.Fatalf("claim lacks 60-second cooldown: %v %v", cooldown, err)
	}
	before := graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)
	if err := replica.ConfigureGraphQualification(ctx, key.RepoID, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)) {
		t.Fatal("replica startup discarded running lease")
	}
	if err := storage.pool.QueryRow(ctx, `UPDATE graph_qualification SET lease_until=clock_timestamp()-interval '1 second'
		WHERE repo_id=$1 RETURNING lease_until`, string(key.RepoID)).Scan(&lease.LeaseUntil); err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, true)); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("expired lease finished: %v", err)
	}
	if _, err := replica.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expiry bypassed cooldown: %v", err)
	}
	graphQualificationDuePG(t, ctx, storage, key.RepoID)
	fresh := graphQualificationClaimPG(t, ctx, replica, key)
	if fresh.Fence <= lease.Fence {
		t.Fatal("expired lease reused its fence")
	}
	if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, true)); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatalf("stale replica finished: %v", err)
	}
	if err := replica.FailGraphQualification(ctx, fresh, "read_error"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed attempt retried without cooldown: %v", err)
	}
}

func TestPGGraphQualificationRetainsExactReceiptsAndNeverRepeatsComparison(t *testing.T) {
	for _, matched := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatch", true: "matched"}[matched], func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			projection := graphProjectionSnapshotPG(t, ctx, storage, key.RepoID)
			source := graphProjectionRepairSourcePG(t, ctx, storage, key.RepoID)
			lease := graphQualificationClaimPG(t, ctx, storage, key)
			result := graphQualificationResultPG(t, ctx, storage, key, matched)
			if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
				t.Fatal(err)
			}
			read, err := storage.ReadGraphQualification(ctx, key)
			if err != nil || read != result || read.Matched() != matched {
				t.Fatalf("incorrect qualification receipt: %+v %v", read, err)
			}
			graphQualificationDuePG(t, ctx, storage, key.RepoID)
			before := graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)
			if _, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("completed exact key compared twice: %v", err)
			}
			if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)) ||
				!reflect.DeepEqual(projection, graphProjectionSnapshotPG(t, ctx, storage, key.RepoID)) ||
				!reflect.DeepEqual(source, graphProjectionRepairSourcePG(t, ctx, storage, key.RepoID)) {
				t.Fatal("qualification or mismatch changed projection/source")
			}
			next := graphQualificationAdvancePG(t, ctx, storage, key.RepoID)
			catalogExecPG(t, ctx, storage.pool, `UPDATE graph_qualification SET next_attempt=clock_timestamp()+interval '60 seconds' WHERE repo_id=$1`, string(key.RepoID))
			if _, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("new generation bypassed cooldown: %v", err)
			}
			for _, reason := range []string{"not_current", "read_error", "resource_limit"} {
				graphQualificationDuePG(t, ctx, storage, key.RepoID)
				fresh := graphQualificationClaimPG(t, ctx, storage, next)
				if read, err := storage.ReadGraphQualification(ctx, key); err != nil || read != result {
					t.Fatalf("new claim discarded prior receipt: %+v %v", read, err)
				}
				if _, err := storage.ReadGraphQualification(ctx, next); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("incomplete key has a receipt: %v", err)
				}
				if err := storage.FailGraphQualification(ctx, fresh, reason); err != nil {
					t.Fatal(err)
				}
				if read, err := storage.ReadGraphQualification(ctx, key); err != nil || read != result {
					t.Fatalf("failure discarded prior receipt: %+v %v", read, err)
				}
				if err := storage.FinishGraphQualification(ctx, fresh, graphQualificationResultPG(t, ctx, storage, next, true)); !errors.Is(err, domain.ErrRefConflict) {
					t.Fatalf("released lease finished: %v", err)
				}
				if reason == "resource_limit" {
					if _, err := storage.ReadGraphQualification(ctx, next); !errors.Is(err, domain.ErrGraphProjectionFailed) {
						t.Fatalf("resource-limited generation remained preparing: %v", err)
					}
					graphQualificationDuePG(t, ctx, storage, key.RepoID)
					if _, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("unchanged resource-limited generation retried: %v", err)
					}
				}
			}
			next = graphQualificationAdvancePG(t, ctx, storage, key.RepoID)
			graphQualificationDuePG(t, ctx, storage, key.RepoID)
			fresh := graphQualificationClaimPG(t, ctx, storage, next)
			if err := storage.FinishGraphQualification(ctx, fresh, graphQualificationResultPG(t, ctx, storage, next, true)); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.ReadGraphQualification(ctx, key); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("bounded ledger retained multiple result generations: %v", err)
			}
			if read, err := storage.ReadGraphQualification(ctx, next); err != nil || !read.Matched() {
				t.Fatalf("next generation never became qualified: %+v %v", read, err)
			}
		})
	}
}

func TestPGGraphQualificationRejectsStaleCompletion(t *testing.T) {
	for _, state := range []string{"disabled", "reenabled", "graph_disabled", "graph_reenabled", "reset", "new_ready", "repair", "terminal"} {
		t.Run(state, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			initial := graphQualificationClaimPG(t, ctx, storage, key)
			if err := storage.FinishGraphQualification(ctx, initial, graphQualificationResultPG(t, ctx, storage, key, true)); err != nil {
				t.Fatal(err)
			}
			next := graphQualificationAdvancePG(t, ctx, storage, key.RepoID)
			graphQualificationDuePG(t, ctx, storage, key.RepoID)
			lease := graphQualificationClaimPG(t, ctx, storage, next)
			result := graphQualificationResultPG(t, ctx, storage, next, true)
			switch state {
			case "disabled", "reenabled":
				if err := storage.ConfigureGraphQualification(ctx, key.RepoID, false); err != nil {
					t.Fatal(err)
				}
				if _, err := storage.ReadGraphQualification(ctx, key); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("disabled qualification exposed receipt: %v", err)
				}
				if state == "reenabled" {
					if err := storage.ConfigureGraphQualification(ctx, key.RepoID, true); err != nil {
						t.Fatal(err)
					}
				}
			case "graph_disabled", "graph_reenabled":
				if err := storage.ConfigureGraphProjection(ctx, key.RepoID, false); err != nil {
					t.Fatal(err)
				}
				if _, err := storage.ReadGraphQualification(ctx, key); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("disabled graph exposed receipt: %v", err)
				}
				if state == "graph_reenabled" {
					if err := storage.ConfigureGraphProjection(ctx, key.RepoID, true); err != nil {
						t.Fatal(err)
					}
				}
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(key.RepoID))
			case "new_ready":
				graphQualificationAdvancePG(t, ctx, storage, key.RepoID)
			case "repair":
				failure := graphProjectionCorruptPG(t, ctx, storage, key.RepoID)
				if err := storage.RequestGraphProjectionRepair(ctx, failure); err != nil {
					t.Fatal(err)
				}
			case "terminal":
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='terminal failure during comparison' WHERE repo_id=$1`, string(key.RepoID))
				job := graphProjectionClaimPG(t, ctx, storage, key.RepoID)
				if err := storage.FailGraphProjection(ctx, job, "invalid_source", false); err != nil {
					t.Fatal(err)
				}
			}
			before := graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)
			if err := storage.FinishGraphQualification(ctx, lease, result); !errors.Is(err, domain.ErrRefConflict) {
				t.Fatalf("stale qualification replaced receipt: %v", err)
			}
			if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, key.RepoID)) {
				t.Fatal("stale result changed qualification state")
			}
		})
	}
}

func TestPGGraphQualificationSourceAdvancePreservesCompletedProof(t *testing.T) {
	for _, phase := range []string{"idle", "capture", "project"} {
		t.Run(phase, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			lease := graphQualificationClaimPG(t, ctx, storage, key)
			catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='advance while shadow compares' WHERE repo_id=$1`, string(key.RepoID))
			if phase != "idle" {
				job := graphProjectionClaimPG(t, ctx, storage, key.RepoID)
				if phase == "project" {
					graphProjectionCapturePG(t, ctx, storage, job, 0)
				}
			}
			before := graphProjectionSnapshotPG(t, ctx, storage, key.RepoID)
			source := graphProjectionRepairSourcePG(t, ctx, storage, key.RepoID)
			result := graphQualificationResultPG(t, ctx, storage, key, true)
			if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
				t.Fatalf("source advance invalidated exact ready proof: %v", err)
			}
			if read, err := storage.ReadGraphQualification(ctx, key); err != nil || read != result {
				t.Fatalf("compatible lagged proof missing: %+v %v", read, err)
			}
			graphQualificationDuePG(t, ctx, storage, key.RepoID)
			if _, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("lagged ready began a new comparison: %v", err)
			}
			if !reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, key.RepoID)) ||
				!reflect.DeepEqual(source, graphProjectionRepairSourcePG(t, ctx, storage, key.RepoID)) {
				t.Fatal("qualification changed source or graph work")
			}
		})
	}
}
