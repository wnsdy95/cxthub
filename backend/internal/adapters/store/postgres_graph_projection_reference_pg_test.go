//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func graphReferencePagePG(test *testing.T, ctx context.Context, storage *PostgresStore, lease domain.GraphProjectionLease, limit int) (domain.RepositoryViewSourcePage, domain.GraphProjectionReference) {
	test.Helper()
	service := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	view, err := service.GetRepositoryView(ctx, lease.RepoID)
	if err != nil {
		test.Fatal(err)
	}
	expected, err := domain.GraphViewDigest(ctx, lease.RepoID, view)
	if err != nil {
		test.Fatal(err)
	}
	page := graphProjectionPagePG(test, ctx, storage, lease, limit)
	reference := domain.GraphProjectionReference{GraphQualificationKey: domain.GraphQualificationKey{RepoID: lease.RepoID, Dataset: lease.Dataset,
		Applied: domain.RepositoryViewSourceCheckpoint{Version: page.Version, Scope: page.Scope, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through},
		Rules:   domain.GraphViewProjectionRules, ComparisonRules: domain.GraphQualificationRules}, Expected: expected}
	return page, reference
}

func graphReferenceReadPG(test *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) domain.GraphProjectionReference {
	test.Helper()
	reference := domain.GraphProjectionReference{GraphQualificationKey: domain.GraphQualificationKey{RepoID: repo,
		Applied: domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion, Scope: domain.RepositoryGraphSourceScope, RepoID: repo}}}
	err := storage.pool.QueryRow(ctx, `SELECT dataset::text,source_epoch::text,source_seq,rules,comparison_rules,expected_hash
		FROM graph_projection_references WHERE repo_id=$1`, string(repo)).Scan(&reference.Dataset, &reference.Applied.Epoch,
		&reference.Applied.Sequence, &reference.Rules, &reference.ComparisonRules, &reference.Expected)
	if err != nil || reference.Validate() != nil {
		test.Fatalf("invalid durable reference: %+v %v", reference, err)
	}
	return reference
}

func graphReferenceReadyPG(test *testing.T) (*PostgresStore, context.Context, domain.GraphProjectionReference) {
	test.Helper()
	storage, ctx, repo := graphProjectionPG(test)
	lease := graphProjectionClaimPG(test, ctx, storage, repo)
	page, reference := graphReferencePagePG(test, ctx, storage, lease, 1)
	if page.NextCursor == "" {
		test.Fatal("fixture requires a partial first page")
	}
	if err := storage.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); err != nil {
		test.Fatal(err)
	}
	lease = graphProjectionClaimPG(test, ctx, storage, repo)
	lease = graphProjectionCapturePG(test, ctx, storage, lease, 1)
	graphProjectionPublishPG(test, ctx, storage, lease)
	if got := graphReferenceReadPG(test, ctx, storage, repo); got != reference {
		test.Fatal("continuation or publication replaced reference", got, reference)
	}
	result := graphQualificationResultPG(test, ctx, storage, reference.GraphQualificationKey, true)
	if result.Actual != reference.Expected {
		test.Fatal("fixture reference does not match candidate", result, reference)
	}
	if err := storage.ConfigureGraphQualification(ctx, repo, true); err != nil {
		test.Fatal(err)
	}
	return storage, ctx, reference
}

func graphReferenceAdvanceSourcePG(test *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message=message || ' advanced' WHERE repo_id=$1`, string(repo))
}

func TestPGGraphProjectionReferenceAtomicFirstPageAndEmptyDelta(test *testing.T) {
	for _, empty := range []bool{false, true} {
		test.Run(fmt.Sprint(empty), func(test *testing.T) {
			storage, ctx, repo := graphProjectionPG(test)
			lease := graphProjectionClaimPG(test, ctx, storage, repo)
			if empty {
				lease = graphProjectionCapturePG(test, ctx, storage, lease, 0)
				graphProjectionPublishPG(test, ctx, storage, lease)
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_catalog_state SET head_seq=head_seq+1 WHERE repo_id=$1`, string(repo))
				lease = graphProjectionClaimPG(test, ctx, storage, repo)
			}
			page, reference := graphReferencePagePG(test, ctx, storage, lease, 0)
			if (len(page.Entries) == 0) != empty {
				test.Fatal("unexpected page fixture", page)
			}
			before := graphProjectionSnapshotPG(test, ctx, storage, repo)
			name := pgx.Identifier{domain.NewID("graph_reference_save_")}.Sanitize()
			catalogExecPG(test, ctx, storage.pool, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'reference save rollback'; END $$;
				CREATE TRIGGER %s BEFORE UPDATE ON graph_projection_work FOR EACH ROW
				WHEN (NEW.repo_id='%s') EXECUTE FUNCTION %s()`, name, name, string(repo), name))
			test.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := storage.pool.Exec(cleanup, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON graph_projection_work; DROP FUNCTION IF EXISTS %s()`, name, name)); err != nil {
					test.Error(err)
				}
			})
			if err := storage.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); err == nil {
				test.Fatal("forced save failure was ignored")
			}
			if after := graphProjectionSnapshotPG(test, ctx, storage, repo); !reflect.DeepEqual(before, after) {
				test.Fatal("reference save partially changed projection")
			}
			var count int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM graph_projection_references WHERE repo_id=$1`, string(repo)).Scan(&count); err != nil || count != 0 {
				test.Fatal("reference escaped rollback", count, err)
			}
			catalogExecPG(test, ctx, storage.pool, fmt.Sprintf(`DROP TRIGGER %s ON graph_projection_work; DROP FUNCTION %s()`, name, name))
			if err := storage.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); err != nil {
				test.Fatal(err)
			}
			if got := graphReferenceReadPG(test, ctx, storage, repo); got != reference {
				test.Fatal("wrong reference persisted", got, reference)
			}
			if err := storage.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); !errors.Is(err, domain.ErrRefConflict) {
				test.Fatal("released page lease saved again", err)
			}
			if empty {
				read, err := storage.ReadGraphProjectionStatus(ctx, repo)
				if err != nil || read.QualificationKey() != reference.GraphQualificationKey || !read.Complete {
					test.Fatal("empty delta did not bind advanced candidate", read, err)
				}
			}
		})
	}
}

func TestPGGraphProjectionReferenceQualifiesLaggedAndAdvances(test *testing.T) {
	storage, ctx, reference := graphReferenceReadyPG(test)
	repo := reference.RepoID
	for round := range 2 {
		graphReferenceAdvanceSourcePG(test, ctx, storage, repo)
		before := graphProjectionSnapshotPG(test, ctx, storage, repo)
		if _, err := storage.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("unacknowledged candidate was replaced", err)
		}
		if after := graphProjectionSnapshotPG(test, ctx, storage, repo); !reflect.DeepEqual(before, after) {
			test.Fatal("hold changed work or extended retention")
		}
		lease := graphQualificationClaimPG(test, ctx, storage, reference.GraphQualificationKey)
		if lease.ReferenceExpected != reference.Expected {
			test.Fatal("claim lost retained reference", lease, reference)
		}
		result := graphQualificationResultPG(test, ctx, storage, reference.GraphQualificationKey, true)
		if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
			test.Fatal("lagged qualification failed", err)
		}
		serving := graphServingReadPG(test, ctx, storage, repo)
		if serving.Ready || serving.QualificationKey() != reference.GraphQualificationKey {
			test.Fatal("lagged promotion lost identity", serving)
		}
		var due bool
		if err := storage.pool.QueryRow(ctx, `SELECT next_attempt<=clock_timestamp() FROM graph_qualification WHERE repo_id=$1`, string(repo)).Scan(&due); err != nil || !due {
			test.Fatal("successful comparison retained cooldown", due, err)
		}
		if round == 0 {
			capture := graphProjectionClaimPG(test, ctx, storage, repo)
			page, next := graphReferencePagePG(test, ctx, storage, capture, 0)
			if next.Applied.Sequence <= reference.Applied.Sequence {
				test.Fatal("next reference did not advance")
			}
			if err := storage.SaveGraphProjectionPageWithReference(ctx, capture, page, next); err != nil {
				test.Fatal(err)
			}
			project := graphProjectionClaimPG(test, ctx, storage, repo)
			graphProjectionPublishPG(test, ctx, storage, project)
			reference = next
		}
	}
}

func TestPGGraphProjectionReferenceDecisionAndLeaseHold(test *testing.T) {
	for _, state := range []string{"mismatch", "resource_limit", "read_error", "active_exact", "active_other", "expired_exact"} {
		test.Run(state, func(test *testing.T) {
			storage, ctx, reference := graphReferenceReadyPG(test)
			lease := graphQualificationClaimPG(test, ctx, storage, reference.GraphQualificationKey)
			graphReferenceAdvanceSourcePG(test, ctx, storage, reference.RepoID)
			if state == "resource_limit" || state == "read_error" {
				if err := storage.FailGraphQualification(ctx, lease, state); err != nil {
					test.Fatal(err)
				}
			} else {
				result := domain.GraphQualificationResult{GraphQualificationKey: reference.GraphQualificationKey,
					Expected: reference.Expected, Actual: domain.HashContent([]byte("mismatching candidate"))}
				if state == "mismatch" {
					if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
						test.Fatal(err)
					}
				} else {
					catalogExecPG(test, ctx, storage.pool, `UPDATE graph_qualification SET result_dataset=attempt_dataset,result_epoch=attempt_epoch,
						result_seq=attempt_seq,result_rules=attempt_rules,result_comparison_rules=attempt_comparison_rules,
						expected_hash=$2,actual_hash=$3,result_state='mismatch',completed_at=clock_timestamp() WHERE repo_id=$1`,
						string(reference.RepoID), string(result.Expected), string(result.Actual))
					if state == "active_other" {
						catalogExecPG(test, ctx, storage.pool, `UPDATE graph_qualification SET attempt_seq=attempt_seq+1 WHERE repo_id=$1`, string(reference.RepoID))
					}
					if state == "expired_exact" {
						catalogExecPG(test, ctx, storage.pool, `UPDATE graph_qualification SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(reference.RepoID))
					}
				}
			}
			var cooling bool
			if err := storage.pool.QueryRow(ctx, `SELECT next_attempt>clock_timestamp() FROM graph_qualification WHERE repo_id=$1`, string(reference.RepoID)).Scan(&cooling); err != nil || !cooling {
				test.Fatal("failure lost cooldown", cooling, err)
			}
			_, err := storage.ClaimGraphProjection(ctx, time.Minute)
			hold := state == "read_error" || state == "active_exact"
			if hold && !errors.Is(err, domain.ErrNotFound) || !hold && err != nil {
				test.Fatal("wrong acknowledgement hold", hold, err)
			}
		})
	}
}

func TestPGGraphProjectionReferenceMissingServingReclaimsImmediately(test *testing.T) {
	storage, ctx, reference := graphReferenceReadyPG(test)
	lease := graphQualificationClaimPG(test, ctx, storage, reference.GraphQualificationKey)
	result := graphQualificationResultPG(test, ctx, storage, reference.GraphQualificationKey, true)
	if err := storage.FinishGraphQualification(ctx, lease, result); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM graph_serving_heads WHERE repo_id=$1`, string(reference.RepoID))
	graphReferenceAdvanceSourcePG(test, ctx, storage, reference.RepoID)
	if _, err := storage.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("matched receipt without serving head released candidate", err)
	}
	recovery := graphQualificationClaimPG(test, ctx, storage, reference.GraphQualificationKey)
	if recovery.Fence <= lease.Fence || recovery.ReferenceExpected != reference.Expected {
		test.Fatal("recovery lost fence or retained reference", recovery, lease)
	}
	if err := storage.FinishGraphQualification(ctx, recovery, result); err != nil {
		test.Fatal(err)
	}
	if serving := graphServingReadPG(test, ctx, storage, reference.RepoID); serving.QualificationKey() != reference.GraphQualificationKey {
		test.Fatal("recovery did not restore exact serving generation", serving)
	}
	graphProjectionClaimPG(test, ctx, storage, reference.RepoID)
}

func TestPGGraphProjectionReferenceSaveRequiresCurrentIdentity(test *testing.T) {
	storage, ctx, repo := graphProjectionPG(test)
	lease := graphProjectionClaimPG(test, ctx, storage, repo)
	page, reference := graphReferencePagePG(test, ctx, storage, lease, 0)
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	before := graphProjectionSnapshotPG(test, ctx, storage, repo)
	if err := storage.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("incompatible caller saved reference", err)
	}
	if after := graphProjectionSnapshotPG(test, ctx, storage, repo); !reflect.DeepEqual(before, after) {
		test.Fatal("incompatible reference save changed projection")
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM graph_projection_references WHERE repo_id=$1`, string(repo)).Scan(&count); err != nil || count != 0 {
		test.Fatal("incompatible reference escaped rollback", count, err)
	}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := storage.SaveGraphProjectionPageWithReference(compatible, lease, page, reference); err != nil {
		test.Fatal("compatible reference save failed", err)
	}
}

func TestPGGraphProjectionReferenceHoldDoesNotStarveOtherWork(test *testing.T) {
	storage, ctx, reference := graphReferenceReadyPG(test)
	graphReferenceAdvanceSourcePG(test, ctx, storage, reference.RepoID)
	catalogExecPG(test, ctx, storage.pool, `UPDATE graph_projection_work SET next_attempt=clock_timestamp()-interval '1 hour' WHERE repo_id=$1`, string(reference.RepoID))
	peer := domain.HashContent([]byte(test.Name() + time.Now().String()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: peer}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM graph_projection_work WHERE repo_id=$1`, string(peer)); err != nil {
			test.Error(err)
		}
	})
	if err := storage.ConfigureGraphProjection(ctx, peer, true); err != nil {
		test.Fatal(err)
	}
	graphProjectionClaimPG(test, ctx, storage, peer)
	if err := storage.ConfigurePendingProjection(ctx, reference.RepoID, true); err != nil {
		test.Fatal(err)
	}
	if lease, err := storage.ClaimPendingProjection(ctx, time.Minute); err != nil || lease.RepoID != reference.RepoID {
		test.Fatal("graph hold blocked independent pending projection", lease, err)
	}
}

func TestPGGraphProjectionReferenceHoldBypassesRecovery(test *testing.T) {
	for _, state := range []string{"disabled", "missing_reference", "expired", "reset", "rules", "repair", "baseline"} {
		test.Run(state, func(test *testing.T) {
			storage, ctx, reference := graphReferenceReadyPG(test)
			repo := reference.RepoID
			graphReferenceAdvanceSourcePG(test, ctx, storage, repo)
			switch state {
			case "disabled":
				if err := storage.ConfigureGraphQualification(ctx, repo, false); err != nil {
					test.Fatal(err)
				}
			case "missing_reference":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM graph_projection_references WHERE repo_id=$1`, string(repo))
				if _, err := storage.ClaimGraphQualification(ctx, repo, time.Minute); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("lagged generation without reference claimed", err)
				}
			case "expired":
				catalogExecPG(test, ctx, storage.pool, `UPDATE graph_projection_work SET retention_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
			case "reset":
				catalogExecPG(test, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			case "rules":
				catalogExecPG(test, ctx, storage.pool, `UPDATE graph_projection_ready SET rules=rules+1 WHERE repo_id=$1`, string(repo))
			case "repair":
				failure := graphProjectionCorruptPG(test, ctx, storage, repo)
				if err := storage.RequestGraphProjectionRepair(ctx, failure); err != nil {
					test.Fatal(err)
				}
			case "baseline":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_graph_source_state graph SET baseline_seq=catalog.head_seq
					FROM repository_catalog_state catalog WHERE graph.repo_id=catalog.repo_id AND graph.repo_id=$1`, string(repo))
			}
			lease := graphProjectionClaimPG(test, ctx, storage, repo)
			if lease.Phase != "capture" {
				test.Fatal("recovery failed to enter capture", lease)
			}
			if state != "disabled" && state != "missing_reference" && lease.Dataset == reference.Dataset {
				test.Fatal("recovery did not fence old dataset", lease)
			}
		})
	}
}

func TestPGGraphProjectionReferenceFinishRejectsChangedReference(test *testing.T) {
	for _, change := range []string{"missing", "hash", "key", "forged_result", "forged_lease", "cleared_lease"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx, reference := graphReferenceReadyPG(test)
			lease := graphQualificationClaimPG(test, ctx, storage, reference.GraphQualificationKey)
			result := graphQualificationResultPG(test, ctx, storage, reference.GraphQualificationKey, true)
			want := domain.ErrRefConflict
			switch change {
			case "missing":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM graph_projection_references WHERE repo_id=$1`, string(reference.RepoID))
			case "hash":
				catalogExecPG(test, ctx, storage.pool, `UPDATE graph_projection_references SET expected_hash=$2 WHERE repo_id=$1`, string(reference.RepoID), string(domain.HashContent([]byte("changed reference"))))
			case "key":
				catalogExecPG(test, ctx, storage.pool, `UPDATE graph_projection_references SET source_seq=source_seq+1 WHERE repo_id=$1`, string(reference.RepoID))
			case "forged_result":
				result.Expected = domain.HashContent([]byte("fabricated expected"))
				want = domain.ErrIntegrity
			case "forged_lease":
				lease.ReferenceExpected = domain.HashContent([]byte("fabricated lease"))
			case "cleared_lease":
				lease.ReferenceExpected = ""
			}
			before := graphQualificationSnapshotPG(test, ctx, storage, reference.RepoID)
			if err := storage.FinishGraphQualification(ctx, lease, result); !errors.Is(err, want) {
				test.Fatal("reference drift accepted", err, want)
			}
			if after := graphQualificationSnapshotPG(test, ctx, storage, reference.RepoID); !reflect.DeepEqual(before, after) {
				test.Fatal("failed finish changed qualification state")
			}
			if change == "forged_lease" || change == "cleared_lease" {
				if err := storage.FailGraphQualification(ctx, lease, "read_error"); !errors.Is(err, domain.ErrRefConflict) {
					test.Fatal("failure bypassed reference lease fence", err)
				}
			}
		})
	}
}

func TestPGGraphProjectionReferenceClaimRechecksHoldAfterLock(test *testing.T) {
	storage, ctx, reference := graphReferenceReadyPG(test)
	if err := storage.ConfigureGraphQualification(ctx, reference.RepoID, false); err != nil {
		test.Fatal(err)
	}
	graphReferenceAdvanceSourcePG(test, ctx, storage, reference.RepoID)
	pause := &graphServingSizePause{prefix: "SELECT " + projectionWorkColumns,
		reached: make(chan struct{}), release: make(chan struct{})}
	peer := graphServingPausedPeerPG(test, ctx, storage, pause)
	var release sync.Once
	unblock := func() { release.Do(func() { close(pause.release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		_, err := peer.ClaimGraphProjection(ctx, time.Minute)
		done <- err
	}()
	select {
	case <-pause.reached:
	case err := <-done:
		test.Fatal("claim skipped selection barrier", err)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE graph_qualification SET enabled=true WHERE repo_id=$1`, string(reference.RepoID))
	unblock()
	if err := <-done; !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("claim ignored newly enabled hold", err)
	}
}
