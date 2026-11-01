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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyValidationPGSource(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) domain.ContentHash {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{SourceProvider: domain.ProviderCodex, SessionOriginID: string(repo)},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 17, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "implicit legacy archive"}}}}}}
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(raw)
	if _, err := storage.PutDoc(ctx, repo, document); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	return document.Hash
}

func legacyValidationPGRejected(test *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) domain.DocRangeIndexJob {
	test.Helper()
	docRangeJobPGEnqueue(test, storage, ctx, repo, hash)
	job := docRangeJobPGClaim(test, storage, ctx, repo, hash)
	job.State, job.Reason, job.LeaseUntil = "rejected", "invalid_document", time.Time{}
	if err := storage.FinishDocRangeJob(ctx, job, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET legacy_validation_revision=0 WHERE repo_id=$1 AND doc_hash=$2`, repo, hash); err != nil {
		test.Fatal(err)
	}
	return job
}

func TestLegacyValidationPGOldRejectionRequeuesOnceUnderConcurrentDemand(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	hash := legacyValidationPGSource(test, storage, ctx, repo)
	prior := legacyValidationPGRejected(test, storage, ctx, repo, hash)
	const callers = 8
	jobs := make([]domain.DocRangeIndexJob, callers)
	failures := make([]error, callers)
	var group sync.WaitGroup
	for position := range jobs {
		group.Add(1)
		go func(position int) {
			defer group.Done()
			jobs[position], failures[position] = storage.EnqueueDocRangeJob(ctx, repo, hash, time.Now().UTC())
		}(position)
	}
	group.Wait()
	for position, job := range jobs {
		if failures[position] != nil || job.State != "waiting" || job.Version != prior.Version+1 || job.Attempts != 0 || job.Reason != "" || !job.LeaseUntil.IsZero() {
			test.Fatal("old validation rejection did not converge on one waiting job", position, job, failures[position])
		}
	}
	claimed := docRangeJobPGClaim(test, storage, ctx, repo, hash)
	var revision int
	if err := storage.pool.QueryRow(ctx, `SELECT legacy_validation_revision FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, repo, hash).Scan(&revision); err != nil || revision != 1 {
		test.Fatal("current worker did not bind its validation revision", revision, err)
	}
	if err := storage.BuildDocRangeJob(ctx, claimed); err != nil {
		test.Fatal("automatic retry failed to publish the valid original legacy archive", err)
	}
	completed := docRangeJobPGGet(test, storage, ctx, repo, hash)
	if completed.State != "completed" || completed.Version != claimed.Version {
		test.Fatal("recovered publication did not complete exactly the claimed job", completed)
	}
	if duplicate, err := storage.EnqueueDocRangeJob(ctx, repo, hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(duplicate, completed) {
		test.Fatal("completed recovery did not retain its durable receipt", duplicate, err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		reader, err := storage.OpenDocRangeGeneration(read, repo, hash)
		if err != nil {
			return err
		}
		if reader.Descriptor().Hash != hash || reader.Descriptor().Identity() != domain.DocumentIdentityLegacy {
			return domain.ErrIntegrity
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestLegacyValidationPGCurrentCorruptionIsNotRetriedForever(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	hash := legacyValidationPGSource(test, storage, ctx, repo)
	legacyValidationPGRejected(test, storage, ctx, repo, hash)
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, []byte(`{"broken":true}`)); err != nil {
		test.Fatal(err)
	}
	if job := docRangeJobPGEnqueue(test, storage, ctx, repo, hash); job.State != "waiting" {
		test.Fatal("old failure was not reevaluated under current validation", job)
	}
	job := docRangeJobPGClaim(test, storage, ctx, repo, hash)
	if err := storage.BuildDocRangeJob(ctx, job); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("current byte corruption was not rejected", err)
	}
	job.State, job.Reason, job.LeaseUntil = "rejected", "invalid_document", time.Time{}
	if err := storage.FinishDocRangeJob(ctx, job, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if duplicate, err := storage.EnqueueDocRangeJob(ctx, repo, hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(duplicate, docRangeJobPGReceipt(job)) {
			test.Fatal("current corruption was automatically retried again", duplicate, err)
		}
	}
	if ready, err := storage.HasDocRangeIndex(ctx, repo, hash); err != nil || ready {
		test.Fatal("corrupt source became range ready", ready, err)
	}
}

func TestLegacyValidationPGRecoveryExcludesOtherReasonsRevisionsAndOwners(tests *testing.T) {
	for _, scenario := range []string{"source_unavailable", "current_revision", "future_revision", "root_source", "ownership_revoked"} {
		tests.Run(scenario, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			var hash domain.ContentHash
			if scenario == "root_source" {
				hash = docRangeJobPGSource(test, storage, ctx, repo, 1).hash
			} else {
				hash = legacyValidationPGSource(test, storage, ctx, repo)
			}
			prior := legacyValidationPGRejected(test, storage, ctx, repo, hash)
			switch scenario {
			case "source_unavailable":
				prior.Reason = "source_unavailable"
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET reason=$3 WHERE repo_id=$1 AND doc_hash=$2`, repo, hash, prior.Reason); err != nil {
					test.Fatal(err)
				}
			case "current_revision", "future_revision":
				revision := 1
				if scenario == "future_revision" {
					revision = 2
				}
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET legacy_validation_revision=$3 WHERE repo_id=$1 AND doc_hash=$2`, repo, hash, revision); err != nil {
					test.Fatal(err)
				}
			case "ownership_revoked":
				if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, repo, hash); err != nil {
					test.Fatal(err)
				}
			}
			duplicate, err := storage.EnqueueDocRangeJob(ctx, repo, hash, time.Now().UTC())
			if scenario == "ownership_revoked" {
				if !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("recovery bypassed source ownership", duplicate, err)
				}
			} else if err != nil || !reflect.DeepEqual(duplicate, docRangeJobPGReceipt(prior)) {
				test.Fatal("unrelated or current failure was retried", duplicate, prior, err)
			}
		})
	}
}

func TestLegacyValidationPGByteAndBoundaryRejectionsRecoverThroughSameFence(tests *testing.T) {
	for _, boundary := range []bool{false, true} {
		name := "byte_requirement"
		if boundary {
			name = "inherited_boundary"
		}
		tests.Run(name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			storage.ConfigureLegacyByteRangePublication(true)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			hash := legacyValidationPGSource(test, storage, ctx, repo)
			var base domain.ContentHash
			if boundary {
				base = docRangeJobPGSource(test, storage, ctx, repo, 1).hash
			}
			enqueue := func() (domain.DocRangeIndexJob, error) {
				if boundary {
					return storage.EnqueueDocRangeBoundary(ctx, repo, hash, base, time.Now().UTC())
				}
				return storage.EnqueueDocByteRangeJob(ctx, repo, hash, time.Now().UTC())
			}
			if _, err := enqueue(); err != nil {
				test.Fatal(err)
			}
			prior := docRangeJobPGClaim(test, storage, ctx, repo, hash)
			prior.State, prior.Reason, prior.LeaseUntil = "rejected", "invalid_document", time.Time{}
			if err := storage.FinishDocRangeJob(ctx, prior, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET legacy_validation_revision=0 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, repo, hash, base); err != nil {
				test.Fatal(err)
			}
			recovered, err := enqueue()
			if err != nil || recovered.State != "waiting" || recovered.BaseHash != base || recovered.Version != prior.Version+1 {
				test.Fatal("specialized receipt did not use the shared recovery fence", recovered, prior, err)
			}
			claimed := docRangeJobPGClaim(test, storage, ctx, repo, hash)
			if claimed.BaseHash != base {
				test.Fatal("recovery changed its inherited selection", claimed)
			}
			if !boundary {
				if err := storage.BuildDocRangeJob(ctx, claimed); err != nil {
					test.Fatal("byte requirement did not recover to a verified generation", err)
				}
				if completed, err := enqueue(); err != nil || completed.State != "completed" || completed.Version != claimed.Version {
					test.Fatal("byte recovery did not preserve its completed receipt", completed, err)
				}
			} else {
				claimed.State, claimed.Reason, claimed.LeaseUntil = "rejected", "invalid_document", time.Time{}
				if err := storage.FinishDocRangeJob(ctx, claimed, time.Now().UTC()); err != nil {
					test.Fatal(err)
				}
				if duplicate, err := enqueue(); err != nil || !reflect.DeepEqual(duplicate, docRangeJobPGReceipt(claimed)) {
					test.Fatal("current pair rejection did not remain durable", duplicate, err)
				}
			}
		})
	}
}

func TestLegacyValidationPGRecoveryHonorsExistingQueueCap(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	hash := legacyValidationPGSource(test, storage, ctx, repo)
	prior := legacyValidationPGRejected(test, storage, ctx, repo, hash)
	for ordinal := 0; ordinal < domain.MaxPendingDocRangeJobs; ordinal++ {
		source := docRangeJobPGSource(test, storage, ctx, repo, ordinal+1)
		docRangeJobPGEnqueue(test, storage, ctx, repo, source.hash)
	}
	if _, err := storage.EnqueueDocRangeJob(ctx, repo, hash, time.Now().UTC()); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("validation recovery bypassed the queue cap", err)
	}
	if current := docRangeJobPGGet(test, storage, ctx, repo, hash); !reflect.DeepEqual(current, docRangeJobPGReceipt(prior)) {
		test.Fatal("failed recovery altered the rejected receipt", current, prior)
	}
	var revision int
	if err := storage.pool.QueryRow(ctx, `SELECT legacy_validation_revision FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, repo, hash).Scan(&revision); err != nil || revision != 0 {
		test.Fatal("failed admission consumed the validation retry revision", revision, err)
	}
}

func TestLegacyValidationPGByteDemandCannotReopenCurrentMetadataFailure(tests *testing.T) {
	for _, revision := range []int{1, 2} {
		tests.Run(fmt.Sprintf("revision=%d", revision), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			storage.ConfigureLegacyByteRangePublication(true)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			hash := legacyValidationPGSource(test, storage, ctx, repo)
			prior := legacyValidationPGRejected(test, storage, ctx, repo, hash)
			if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET legacy_validation_revision=$3 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, repo, hash, revision); err != nil {
				test.Fatal(err)
			}
			for attempt := 0; attempt < 3; attempt++ {
				job, err := storage.EnqueueDocByteRangeJob(ctx, repo, hash, time.Now().UTC())
				if err != nil || !reflect.DeepEqual(job, docRangeJobPGReceipt(prior)) {
					test.Fatal("byte promotion reopened a current/future invalid source", revision, job, prior, err)
				}
			}
		})
	}
}

func TestLegacyValidationPGClaimCapabilityFencesDocumentAndBase(tests *testing.T) {
	for _, legacyBase := range []bool{false, true} {
		name := "legacy_document"
		if legacyBase {
			name = "legacy_base_only"
		}
		tests.Run(name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			hash := legacyValidationPGSource(test, storage, ctx, repo)
			var base domain.ContentHash
			if legacyBase {
				base, hash = hash, docRangeJobPGSource(test, storage, ctx, repo, 1).hash
				if _, err := storage.EnqueueDocRangeBoundary(ctx, repo, hash, base, time.Now().UTC()); err != nil {
					test.Fatal(err)
				}
			} else {
				docRangeJobPGEnqueue(test, storage, ctx, repo, hash)
			}
			oldClaim := func(capability string) {
				test.Helper()
				transaction, err := storage.pool.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(transaction)
				if _, err := transaction.Exec(ctx, `SET LOCAL cxt.doc_range_worker_protocol='4'`); err != nil {
					test.Fatal(err)
				}
				if capability != "" {
					if _, err := transaction.Exec(ctx, `SELECT set_config('cxt.legacy_range_validation_revision',$1,true)`, capability); err != nil {
						test.Fatal(err)
					}
				}
				_, err = transaction.Exec(ctx, `UPDATE doc_range_jobs_v1 SET state='running',version=version+1,lease_until=clock_timestamp()+interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, repo, hash, base)
				var rejected *pgconn.PgError
				if !errors.As(err, &rejected) || rejected.Code != "0A000" {
					test.Fatal("unpatched or mismatched worker claimed revised legacy work", capability, err)
				}
			}
			oldClaim("")
			oldClaim("2")
			missing := hash
			if legacyBase {
				missing = base
			}
			if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, repo, missing); err != nil {
				test.Fatal(err)
			}
			oldClaim("")
			if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, repo, missing); err != nil {
				test.Fatal(err)
			}
			claimed := docRangeJobPGClaim(test, storage, ctx, repo, hash)
			if claimed.BaseHash != base {
				test.Fatal("claim changed pair identity", claimed)
			}
			if err := storage.RenewDocRangeJob(ctx, claimed, time.Now().UTC(), time.Minute); err != nil {
				test.Fatal("capability fence broke lease renewal", err)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, repo, hash, base); err != nil {
				test.Fatal(err)
			}
			oldClaim("")
			reclaimed := docRangeJobPGClaim(test, storage, ctx, repo, hash)
			if reclaimed.Version != claimed.Version+1 {
				test.Fatal("current worker did not reclaim the expired legacy lease", reclaimed, claimed)
			}
			reclaimed.State, reclaimed.Reason, reclaimed.LeaseUntil = "rejected", "invalid_document", time.Time{}
			if err := storage.FinishDocRangeJob(ctx, reclaimed, time.Now().UTC()); err != nil {
				test.Fatal("capability fence broke terminal finish", err)
			}
		})
	}
}

func TestLegacyValidationPGByteRejectionRecoversAfterPhysicalSourceRepair(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyByteRangePublication(true)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	hash := legacyValidationPGSource(test, storage, ctx, repo)
	var original []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, hash).Scan(&original); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, []byte(`{"broken":true}`)); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocByteRangeJob(ctx, repo, hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	prior := docRangeJobPGClaim(test, storage, ctx, repo, hash)
	if err := storage.BuildDocRangeJob(ctx, prior); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("fixture corruption was not rejected", err)
	}
	prior.State, prior.Reason, prior.LeaseUntil = "rejected", "invalid_document", time.Time{}
	if err := storage.FinishDocRangeJob(ctx, prior, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, original); err != nil {
		test.Fatal(err)
	}
	queued, err := storage.EnqueueDocByteRangeJob(ctx, repo, hash, time.Now().UTC())
	if err != nil || queued.State != "waiting" || queued.Version != prior.Version+1 {
		test.Fatal("current validation rejection prevented repaired-source retry", queued, prior, err)
	}
	claimed := docRangeJobPGClaim(test, storage, ctx, repo, hash)
	if err := storage.BuildDocRangeJob(ctx, claimed); err != nil {
		test.Fatal("repaired original failed full source validation", err)
	}
	completed, err := storage.EnqueueDocByteRangeJob(ctx, repo, hash, time.Now().UTC())
	if err != nil || completed.State != "completed" || completed.Version != claimed.Version {
		test.Fatal("repaired source did not retain its completed generation", completed, err)
	}
}

func TestLegacyValidationPGFutureByteRejectionStaysTerminalAfterSourceRepair(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyByteRangePublication(true)
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	hash := legacyValidationPGSource(test, storage, ctx, repo)
	if _, err := storage.EnqueueDocByteRangeJob(ctx, repo, hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	prior := docRangeJobPGClaim(test, storage, ctx, repo, hash)
	prior.State, prior.Reason, prior.LeaseUntil = "rejected", "invalid_document", time.Time{}
	if err := storage.FinishDocRangeJob(ctx, prior, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	var original []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, hash).Scan(&original); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, []byte(`{"broken":true}`)); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, hash, original); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET legacy_validation_revision=2 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, repo, hash); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		current, err := storage.EnqueueDocByteRangeJob(ctx, repo, hash, time.Now().UTC())
		if err != nil || !reflect.DeepEqual(current, docRangeJobPGReceipt(prior)) {
			test.Fatal("physical source repair reinterpreted a future-rule rejection", current, prior, err)
		}
	}
}

func TestLegacyValidationPGFutureRevisionDoesNotStarveCompatibleClaim(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	futureRepo, _ := docRangeJobPGRepo(test, storage, ctx)
	futureHash := legacyValidationPGSource(test, storage, ctx, futureRepo)
	future := docRangeJobPGEnqueue(test, storage, ctx, futureRepo, futureHash)
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET legacy_validation_revision=2 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, futureRepo, futureHash); err != nil {
		test.Fatal(err)
	}
	currentRepo, _ := docRangeJobPGRepo(test, storage, ctx)
	currentHash := legacyValidationPGSource(test, storage, ctx, currentRepo)
	docRangeJobPGEnqueue(test, storage, ctx, currentRepo, currentHash)
	claimed := docRangeJobPGClaim(test, storage, ctx, currentRepo, currentHash)
	if claimed.RepoID != currentRepo || claimed.DocHash != currentHash {
		test.Fatal("future validation revision blocked compatible work", claimed)
	}
	if current := docRangeJobPGGet(test, storage, ctx, futureRepo, futureHash); !reflect.DeepEqual(current, future) {
		test.Fatal("current worker changed future work", current, future)
	}
}
