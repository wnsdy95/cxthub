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

func graphServingCorruptPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.GraphProjectionCorruption {
	t.Helper()
	before, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || !before.Complete {
		t.Fatalf("serving corruption fixture unavailable: %+v %v", before, err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_parts SET image='{}' WHERE repo_id=$1 AND kind='header'`, string(repo))
	read, err := storage.ReadQualifiedGraphProjection(ctx, repo)
	var failure *domain.GraphProjectionCorruption
	if !errors.As(err, &failure) || failure.Validate() != nil || !failure.Serving || failure.ServingFence != before.ServingFence ||
		failure.Dataset != before.Dataset || failure.Applied != before.Applied || !reflect.DeepEqual(read, before) {
		t.Fatalf("serving corruption lost fenced identity: %+v %+v %v", read, before, err)
	}
	return *failure
}

func TestPGGraphServingRepairCoalescesAndFencesSameKeyReplacement(t *testing.T) {
	storage, ctx, initial := graphServingPG(t)
	repo := initial.Source.RepoID
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	source := graphProjectionRepairSourcePG(t, ctx, storage, repo)
	failure := graphServingCorruptPG(t, ctx, storage, repo)
	const attempts = 8
	outcomes := make(chan error, attempts)
	for attempt := 0; attempt < attempts; attempt++ {
		go func() { outcomes <- storage.RequestGraphServingRepair(ctx, failure) }()
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if err := <-outcomes; err != nil {
			t.Fatal(err)
		}
	}
	var requests, rebuilds, fence int64
	if err := storage.pool.QueryRow(ctx, `SELECT request_count,cardinality(rebuilds),serving_fence FROM graph_serving_repairs WHERE repo_id=$1`, string(repo)).Scan(&requests, &rebuilds, &fence); err != nil {
		t.Fatal(err)
	}
	if requests != attempts || rebuilds != 1 || fence != initial.ServingFence {
		t.Fatalf("duplicate repair not coalesced: requests=%d rebuilds=%d fence=%d", requests, rebuilds, fence)
	}
	if _, err := storage.ReadGraphQualification(ctx, initial.QualificationKey()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("exact matched receipt was not released: %v", err)
	}
	status, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || status.Complete || status.Ready || status.NeedsAttention || !status.Enabled {
		t.Fatalf("invalidated serving output remained complete: %+v %v", status, err)
	}
	fresh := graphServingPromotePG(t, ctx, storage, initial.QualificationKey())
	if fresh.ServingFence <= initial.ServingFence || !reflect.DeepEqual(fresh.Parts, initial.Parts) {
		t.Fatal("same candidate was not recopied under a fresh serving fence")
	}
	oldStatus, _ := initial.Status()
	newStatus, _ := fresh.Status()
	if oldStatus.Generation == newStatus.Generation {
		t.Fatal("same-key repair did not advance external generation")
	}
	serving := graphServingSnapshotPG(t, ctx, storage, repo)
	receipt := graphQualificationSnapshotPG(t, ctx, storage, repo)
	if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serving, graphServingSnapshotPG(t, ctx, storage, repo)) || !bytes.Equal(receipt, graphQualificationSnapshotPG(t, ctx, storage, repo)) ||
		!reflect.DeepEqual(before, graphProjectionSnapshotPG(t, ctx, storage, repo)) || !reflect.DeepEqual(source, graphProjectionRepairSourcePG(t, ctx, storage, repo)) {
		t.Fatal("stale fenced receipt changed replacement, diagnostic budget, candidate, or source")
	}
}

func TestPGGraphServingRepairPreservesNewerCandidateReceiptAndLease(t *testing.T) {
	for _, mode := range []string{"candidate", "running", "mismatch", "matched"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			failure := graphServingCorruptPG(t, ctx, storage, repo)
			next := graphQualificationAdvancePG(t, ctx, storage, repo)
			var lease domain.GraphQualificationLease
			if mode != "candidate" {
				graphQualificationDuePG(t, ctx, storage, repo)
				lease = graphQualificationClaimPG(t, ctx, storage, next)
			}
			if mode == "mismatch" || mode == "matched" {
				if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, next, mode == "matched")); err != nil {
					t.Fatal(err)
				}
			}
			candidate := graphProjectionSnapshotPG(t, ctx, storage, repo)
			receipt := graphQualificationSnapshotPG(t, ctx, storage, repo)
			source := graphProjectionRepairSourcePG(t, ctx, storage, repo)
			if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(candidate, graphProjectionSnapshotPG(t, ctx, storage, repo)) || !reflect.DeepEqual(source, graphProjectionRepairSourcePG(t, ctx, storage, repo)) {
				t.Fatal("serving repair changed newer candidate or source")
			}
			if mode == "mismatch" || mode == "matched" {
				if !bytes.Equal(receipt, graphQualificationSnapshotPG(t, ctx, storage, repo)) {
					t.Fatal("serving repair cleared unrelated completed qualification")
				}
			}
			if mode == "running" {
				if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, next, true)); err != nil {
					t.Fatalf("serving repair fenced unrelated qualification lease: %v", err)
				}
			}
			if mode == "running" || mode == "matched" {
				if read := graphServingReadPG(t, ctx, storage, repo); read.QualificationKey() != next {
					t.Fatal("new generation failed to survive stale serving repair")
				}
			}
		})
	}
}

func TestPGGraphServingRepairQuarantineSurvivesRestartAndBlocksInflightPromotion(t *testing.T) {
	storage, ctx, initial := graphServingPG(t)
	repo := initial.Source.RepoID
	var blocked domain.GraphQualificationLease
	var result domain.GraphQualificationResult
	for repair := 1; repair <= 4; repair++ {
		failure := graphServingCorruptPG(t, ctx, storage, repo)
		if repair == 4 {
			next := graphQualificationAdvancePG(t, ctx, storage, repo)
			graphQualificationDuePG(t, ctx, storage, repo)
			blocked = graphQualificationClaimPG(t, ctx, storage, next)
			result = graphQualificationResultPG(t, ctx, storage, next, true)
		}
		if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
			t.Fatal(err)
		}
		var quarantined bool
		var count int
		if err := storage.pool.QueryRow(ctx, `SELECT quarantined,cardinality(rebuilds) FROM graph_serving_repairs WHERE repo_id=$1`, string(repo)).Scan(&quarantined, &count); err != nil {
			t.Fatal(err)
		}
		if quarantined != (repair == 4) || count != min(repair, 3) {
			t.Fatalf("unbounded serving repair window: repair=%d quarantine=%v count=%d", repair, quarantined, count)
		}
		if repair < 4 {
			graphServingPromotePG(t, ctx, storage, initial.QualificationKey())
		}
	}
	before := graphQualificationSnapshotPG(t, ctx, storage, repo)
	if err := storage.FinishGraphQualification(ctx, blocked, result); !errors.Is(err, domain.ErrGraphProjectionFailed) {
		t.Fatalf("inflight worker bypassed quarantine: %v", err)
	}
	if !bytes.Equal(before, graphQualificationSnapshotPG(t, ctx, storage, repo)) {
		t.Fatal("quarantined promotion replaced receipt")
	}
	if err := storage.FailGraphQualification(ctx, blocked, "read_error"); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.ConfigureGraphProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ConfigureGraphQualification(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	graphQualificationDuePG(t, ctx, storage, repo)
	if _, err := restarted.ClaimGraphQualification(ctx, repo, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("quarantined comparison was retried: %v", err)
	}
	read, err := restarted.ReadQualifiedGraphProjectionStatus(ctx, repo)
	if err != nil || !read.NeedsAttention || read.Complete {
		t.Fatalf("quarantine lost across restart: %+v %v", read, err)
	}
}

func TestPGGraphServingRepairDoesNotReviveTerminalDisabledOrResetWork(t *testing.T) {
	for _, mode := range []string{"terminal", "publisher_disabled", "qualifier_disabled", "reset"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			failure := graphServingCorruptPG(t, ctx, storage, repo)
			switch mode {
			case "terminal":
				catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='terminal source failure' WHERE repo_id=$1`, string(repo))
				if err := storage.FailGraphProjection(ctx, graphProjectionClaimPG(t, ctx, storage, repo), "invalid_source", false); err != nil {
					t.Fatal(err)
				}
			case "publisher_disabled":
				if err := storage.ConfigureGraphProjection(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			case "qualifier_disabled":
				if err := storage.ConfigureGraphQualification(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			}
			before := graphServingSnapshotPG(t, ctx, storage, repo)
			receipt := graphQualificationSnapshotPG(t, ctx, storage, repo)
			candidate := graphProjectionSnapshotPG(t, ctx, storage, repo)
			if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, graphServingSnapshotPG(t, ctx, storage, repo)) || !bytes.Equal(receipt, graphQualificationSnapshotPG(t, ctx, storage, repo)) ||
				!reflect.DeepEqual(candidate, graphProjectionSnapshotPG(t, ctx, storage, repo)) {
				t.Fatal("delayed receipt revived incompatible/terminal projection")
			}
		})
	}
}

func TestPGGraphServingFenceSurvivesQualificationRowReconstruction(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "head_retained", true: "head_invalidated"}[repair], func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			failure := graphServingCorruptPG(t, ctx, storage, repo)
			if repair {
				if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
					t.Fatal(err)
				}
			}
			catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_qualification WHERE repo_id=$1`, string(repo))
			if err := storage.ConfigureGraphQualification(ctx, repo, true); err != nil {
				t.Fatal(err)
			}
			fresh := graphServingPromotePG(t, ctx, storage, initial.QualificationKey())
			if fresh.ServingFence <= initial.ServingFence {
				t.Fatal("qualification reconstruction reset serving fence")
			}
			before := graphServingSnapshotPG(t, ctx, storage, repo)
			if err := storage.RequestGraphServingRepair(ctx, failure); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, graphServingSnapshotPG(t, ctx, storage, repo)) {
				t.Fatal("reconstructed qualification allowed serving repair ABA")
			}
		})
	}
}
