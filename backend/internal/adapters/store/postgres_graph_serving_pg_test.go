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

func graphServingPromotePG(t *testing.T, ctx context.Context, storage *PostgresStore, key domain.GraphQualificationKey) domain.GraphProjectionRead {
	t.Helper()
	graphQualificationDuePG(t, ctx, storage, key.RepoID)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, true)); err != nil {
		t.Fatal(err)
	}
	read := graphServingReadPG(t, ctx, storage, key.RepoID)
	if read.QualificationKey() != key || read.ServingFence <= 0 {
		t.Fatalf("wrong promotion: %+v", read)
	}
	return read
}

func graphServingPG(t *testing.T) (*PostgresStore, context.Context, domain.GraphProjectionRead) {
	t.Helper()
	storage, ctx, key := graphQualificationPG(t)
	return storage, ctx, graphServingPromotePG(t, ctx, storage, key)
}

func graphServingReadPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.GraphProjectionRead {
	t.Helper()
	read, err := storage.ReadQualifiedGraphProjection(ctx, repo)
	if err != nil || !read.Complete || !read.Enabled || read.ServingFence <= 0 || read.Qualification == nil ||
		!read.Qualification.Matched() || read.Qualification.GraphQualificationKey != read.QualificationKey() || len(read.Parts) == 0 {
		t.Fatalf("qualified serving head unavailable: %+v %v", read, err)
	}
	status, err := storage.ReadQualifiedGraphProjectionStatus(ctx, repo)
	metadata := read
	metadata.Parts = nil
	if err != nil || !reflect.DeepEqual(status, metadata) {
		t.Fatalf("serving body/status disagree: %+v %+v %v", read, status, err)
	}
	if _, err := read.Status(); err != nil {
		t.Fatal(err)
	}
	return read
}

func graphServingSnapshotPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) [][]byte {
	t.Helper()
	var result [][]byte
	for _, table := range []string{"graph_serving_heads", "graph_serving_parts", "graph_serving_repairs"} {
		var rows []byte
		if err := storage.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(derived) || jsonb_build_object('xmin',derived.xmin::text)
			ORDER BY to_jsonb(derived)::text),'[]'::jsonb) FROM `+table+` derived WHERE repo_id=$1`, string(repo)).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		result = append(result, rows)
	}
	return result
}

func TestPGGraphServingRetainsQualifiedHeadAcrossPublishPruneAndMismatch(t *testing.T) {
	storage, ctx, initial := graphServingPG(t)
	repo := initial.Source.RepoID
	before := graphServingSnapshotPG(t, ctx, storage, repo)
	next := graphQualificationAdvancePG(t, ctx, storage, repo)
	lagged := graphServingReadPG(t, ctx, storage, repo)
	if lagged.Ready || lagged.QualificationKey() != initial.QualificationKey() || lagged.ServingFence != initial.ServingFence ||
		!reflect.DeepEqual(lagged.Parts, initial.Parts) || lagged.Source.Sequence <= lagged.Applied.Sequence {
		t.Fatal("new candidate replaced independently retained serving head")
	}
	graphQualificationDuePG(t, ctx, storage, repo)
	lease := graphQualificationClaimPG(t, ctx, storage, next)
	if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, next, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReadGraphQualification(ctx, initial.QualificationKey()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("latest receipt unexpectedly retained old key: %v", err)
	}
	restarted, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.ConfigureGraphQualification(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	read := graphServingReadPG(t, ctx, restarted, repo)
	if read.NeedsAttention || read.QualificationKey() != initial.QualificationKey() || !reflect.DeepEqual(before, graphServingSnapshotPG(t, ctx, storage, repo)) {
		t.Fatal("new mismatch or startup destroyed retained proof")
	}
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_parts WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_ready WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE repository_catalog_state SET floor_seq=head_seq WHERE repo_id=$1`, string(repo))
	read = graphServingReadPG(t, ctx, storage, repo)
	if read.QualificationKey() != initial.QualificationKey() || !reflect.DeepEqual(read.Parts, initial.Parts) {
		t.Fatal("retained proof depended on candidate parts, ready, or journal floor")
	}
}

func TestPGGraphServingPendingAndFailedWithoutHead(t *testing.T) {
	for _, mode := range []string{"unconfigured", "disabled", "pending", "mismatch", "resource_limit", "new_candidate"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			switch mode {
			case "unconfigured":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_qualification WHERE repo_id=$1`, string(key.RepoID))
			case "disabled":
				if err := storage.ConfigureGraphQualification(ctx, key.RepoID, false); err != nil {
					t.Fatal(err)
				}
			case "mismatch", "new_candidate":
				lease := graphQualificationClaimPG(t, ctx, storage, key)
				if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, false)); err != nil {
					t.Fatal(err)
				}
				if mode == "new_candidate" {
					graphQualificationAdvancePG(t, ctx, storage, key.RepoID)
				}
			case "resource_limit":
				if err := storage.FailGraphQualification(ctx, graphQualificationClaimPG(t, ctx, storage, key), "resource_limit"); err != nil {
					t.Fatal(err)
				}
			}
			for _, read := range []func(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error){storage.ReadQualifiedGraphProjection, storage.ReadQualifiedGraphProjectionStatus} {
				result, err := read(ctx, key.RepoID)
				failed := mode == "mismatch" || mode == "resource_limit"
				if err != nil || result.Complete || result.Ready || !result.Enabled || result.NeedsAttention != failed || result.Qualification != nil || result.ServingFence != 0 {
					t.Fatalf("no-head state incorrect: %+v %v", result, err)
				}
			}
		})
	}
}

func TestPGGraphServingPromotionAndReadsShareOneMVCCGeneration(t *testing.T) {
	storage, ctx, initial := graphServingPG(t)
	repo := initial.Source.RepoID
	next := graphQualificationAdvancePG(t, ctx, storage, repo)
	graphQualificationDuePG(t, ctx, storage, repo)
	lease := graphQualificationClaimPG(t, ctx, storage, next)
	result := graphQualificationResultPG(t, ctx, storage, next, true)
	beforeSource := graphProjectionRepairSourcePG(t, ctx, storage, repo)
	beforeCandidate := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		before := graphServingReadPG(t, bound, storage, repo)
		if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
			return err
		}
		after := graphServingReadPG(t, bound, storage, repo)
		if !reflect.DeepEqual(before, after) || after.QualificationKey() != initial.QualificationKey() {
			t.Fatal("serving head/parts/receipt crossed MVCC promotion boundary")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read := graphServingReadPG(t, ctx, storage, repo)
	if read.QualificationKey() != next || !read.Ready || read.ServingFence <= initial.ServingFence ||
		!reflect.DeepEqual(beforeSource, graphProjectionRepairSourcePG(t, ctx, storage, repo)) ||
		!reflect.DeepEqual(beforeCandidate, graphProjectionSnapshotPG(t, ctx, storage, repo)) {
		t.Fatal("promotion changed source/candidate or did not atomically advance")
	}
}

func TestPGGraphServingPromotionRejectsCandidateDriftAndRollsBack(t *testing.T) {
	for _, mode := range []string{"digest", "header", "missing", "oversize", "body", "fence"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			next := graphQualificationAdvancePG(t, ctx, storage, repo)
			graphQualificationDuePG(t, ctx, storage, repo)
			lease := graphQualificationClaimPG(t, ctx, storage, next)
			result := graphQualificationResultPG(t, ctx, storage, next, true)
			want := domain.ErrIntegrity
			switch mode {
			case "digest":
				result.Expected, result.Actual = domain.HashContent([]byte("fabricated match")), domain.HashContent([]byte("fabricated match"))
			case "header":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_parts SET image='{}' WHERE repo_id=$1 AND kind='header'`, string(repo))
			case "missing":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_parts WHERE repo_id=$1 AND kind='header'`, string(repo))
			case "oversize":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_parts SET image=jsonb_build_object('large',repeat('x',$2)) WHERE repo_id=$1 AND kind='header'`, string(repo), domain.MaxPendingProjectionOutputBytes)
				want = domain.ErrRepositoryViewSourceLimit
			case "body":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_parts SET image='{}' WHERE repo_id=$1 AND kind<>'header'`, string(repo))
			case "fence":
				lease.Fence++
				want = domain.ErrRefConflict
			}
			before := graphServingSnapshotPG(t, ctx, storage, repo)
			receipt := graphQualificationSnapshotPG(t, ctx, storage, repo)
			if err := storage.FinishGraphQualification(ctx, lease, result); !errors.Is(err, want) {
				t.Fatalf("unsafe promotion accepted: %v want %v", err, want)
			}
			if !reflect.DeepEqual(before, graphServingSnapshotPG(t, ctx, storage, repo)) || !bytes.Equal(receipt, graphQualificationSnapshotPG(t, ctx, storage, repo)) {
				t.Fatal("failed promotion partially changed head, parts, or receipt")
			}
			graphServingReadPG(t, ctx, storage, repo)
		})
	}
}

func TestPGGraphServingSourceAdvanceDuringComparisonPromotesLaggedProof(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result := graphQualificationResultPG(t, ctx, storage, key, true)
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='source advanced during comparison' WHERE repo_id=$1`, string(key.RepoID))
	if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
		t.Fatal(err)
	}
	read := graphServingReadPG(t, ctx, storage, key.RepoID)
	if read.Ready || read.Applied.Sequence >= read.Source.Sequence || read.QualificationKey() != key {
		t.Fatal("compatible old proof was not promoted as lagged")
	}
}

func TestPGGraphServingTerminalAndIncompatibleHeadsFailClosed(t *testing.T) {
	for _, mode := range []string{"publisher_disabled", "qualifier_disabled", "terminal", "reset", "old_rules", "future_rules", "comparison_rules", "future_sequence"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, initial := graphServingPG(t)
			repo := initial.Source.RepoID
			switch mode {
			case "publisher_disabled":
				if err := storage.ConfigureGraphProjection(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			case "qualifier_disabled":
				if err := storage.ConfigureGraphQualification(ctx, repo, false); err != nil {
					t.Fatal(err)
				}
			case "terminal":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_work SET phase='needs_attention',reason='invalid_source' WHERE repo_id=$1`, string(repo))
			case "reset":
				catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			case "old_rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_heads SET rules=1 WHERE repo_id=$1`, string(repo))
			case "future_rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_heads SET rules=3 WHERE repo_id=$1`, string(repo))
			case "comparison_rules":
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_heads SET comparison_rules=comparison_rules+1 WHERE repo_id=$1`, string(repo))
			case "future_sequence":
				catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_serving_parts WHERE repo_id=$1`, string(repo))
				catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_heads SET source_seq=source_seq+1 WHERE repo_id=$1`, string(repo))
			}
			read, err := storage.ReadQualifiedGraphProjection(ctx, repo)
			if err != nil || read.Complete || read.Ready || read.ServingFence != 0 || read.Qualification != nil || len(read.Parts) != 0 {
				t.Fatalf("unavailable serving proof leaked: %+v %v", read, err)
			}
		})
	}
}

func TestPGGraphServingConcurrentCompletionsPromoteOnce(t *testing.T) {
	storage, ctx, key := graphQualificationPG(t)
	lease := graphQualificationClaimPG(t, ctx, storage, key)
	result := graphQualificationResultPG(t, ctx, storage, key, true)
	const attempts = 6
	outcomes := make(chan error, attempts)
	for attempt := 0; attempt < attempts; attempt++ {
		go func() { outcomes <- storage.FinishGraphQualification(ctx, lease, result) }()
	}
	succeeded := 0
	for attempt := 0; attempt < attempts; attempt++ {
		if err := <-outcomes; err == nil {
			succeeded++
		} else if !errors.Is(err, domain.ErrRefConflict) {
			t.Fatal(err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent promotion count: %d", succeeded)
	}
	graphServingReadPG(t, ctx, storage, key.RepoID)
	graphQualificationDuePG(t, ctx, storage, key.RepoID)
	if _, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("completed serving key qualified again: %v", err)
	}
}

func TestPGGraphServingRequalifiesLegacyMatchingReceiptWithoutHead(t *testing.T) {
	for _, matched := range []bool{true, false} {
		t.Run(map[bool]string{true: "matched", false: "mismatched"}[matched], func(t *testing.T) {
			storage, ctx, key := graphQualificationPG(t)
			lease := graphQualificationClaimPG(t, ctx, storage, key)
			result := graphQualificationResultPG(t, ctx, storage, key, matched)
			if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
				t.Fatal(err)
			}
			catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_serving_heads WHERE repo_id=$1`, string(key.RepoID))
			source := graphProjectionRepairSourcePG(t, ctx, storage, key.RepoID)
			graphQualificationDuePG(t, ctx, storage, key.RepoID)
			lease, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute)
			if !matched {
				if !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("unchanged mismatch retried", err)
				}
				return
			}
			if err != nil || lease.GraphQualificationKey != key {
				t.Fatal("quiet legacy repo could not requalify", err)
			}
			read, err := storage.ReadQualifiedGraphProjectionStatus(ctx, key.RepoID)
			if err != nil || read.Complete {
				t.Fatal("old receipt was treated as serving proof without new comparison", err)
			}
			if err := storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, key, true)); err != nil {
				t.Fatal(err)
			}
			graphServingReadPG(t, ctx, storage, key.RepoID)
			graphQualificationDuePG(t, ctx, storage, key.RepoID)
			if _, err := storage.ClaimGraphQualification(ctx, key.RepoID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("completed legacy upgrade did not coalesce", err)
			}
			if !reflect.DeepEqual(source, graphProjectionRepairSourcePG(t, ctx, storage, key.RepoID)) {
				t.Fatal("legacy serving bootstrap mutated source")
			}
		})
	}
}

func TestPGGraphServingIncompatibleHeadSurfacesCurrentCandidateFailure(t *testing.T) {
	for _, mode := range []string{"epoch", "rules"} {
		for _, failure := range []string{"mismatch", "resource_limit"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				storage, ctx, initial := graphServingPG(t)
				repo := initial.Source.RepoID
				if mode == "epoch" {
					catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
				} else {
					catalogExecPG(t, ctx, storage.pool, `UPDATE graph_serving_heads SET rules=rules+1 WHERE repo_id=$1`, string(repo))
				}
				next := graphQualificationAdvancePG(t, ctx, storage, repo)
				graphQualificationDuePG(t, ctx, storage, repo)
				lease := graphQualificationClaimPG(t, ctx, storage, next)
				var err error
				if failure == "mismatch" {
					err = storage.FinishGraphQualification(ctx, lease, graphQualificationResultPG(t, ctx, storage, next, false))
				} else {
					err = storage.FailGraphQualification(ctx, lease, failure)
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, read := range []func(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error){storage.ReadQualifiedGraphProjection, storage.ReadQualifiedGraphProjectionStatus} {
					result, err := read(ctx, repo)
					if err != nil || !result.NeedsAttention || result.Complete || len(result.Parts) != 0 {
						t.Fatal("unusable retained head concealed failed candidate", result, err)
					}
				}
			})
		}
	}
}
