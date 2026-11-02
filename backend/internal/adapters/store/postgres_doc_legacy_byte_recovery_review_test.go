//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func legacyByteRecoveryReviewUntrustedHeader(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) {
	test.Helper()
	body := bytes.Clone(fixture.envelope)
	if len(body) < 2 || len(body) > 64<<10 {
		test.Fatal("review fixture requires one bounded envelope part")
	}
	body[0] = '['
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()
	for _, statement := range []string{
		`SET LOCAL cxt.legacy_byte_stage_protocol='1'`,
		`SET LOCAL cxt.legacy_byte_header_protocol='1'`,
	} {
		if _, err := transaction.Exec(ctx, statement); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_version=1,header_bytes=$2 WHERE doc_hash=$1`, fixture.hash, len(body)); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes) VALUES($1,0,$2,$3)`, fixture.hash, domain.HashContent(body), body); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_next=1,header_complete=true WHERE doc_hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
}

func TestLegacyByteRecoveryReviewDelayedFailurePreservesAnotherClaimsRestagedCheckpoint(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 4096)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peerRepo, _ := docRangeJobPGRepo(test, storage, ctx)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, peerRepo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: peerRepo, DocHash: fixture.hash,
		DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	peerFixture := fixture
	peerFixture.repo = peerRepo
	peerJob := legacyByteStagePGClaim(test, storage, ctx, peerFixture)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	seed, err := domain.NewLegacyRangeMetadataSeed(ctx, session.proof, session.plan)
	if err != nil {
		test.Fatal(err)
	}
	stage, err := storage.stageLegacyByteSession(ctx, job, session, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || !stage.complete {
		test.Fatal("review fixture did not finish its bounded stream", stage, err)
	}
	legacyByteRecoveryReviewUntrustedHeader(test, storage, ctx, fixture)
	first, err := storage.newLegacyRecoveryWork(ctx, job)
	if err != nil || first == nil {
		test.Fatal("schema-valid untrusted checkpoint was not offered for verification", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			test.Error(err)
		}
	}()
	delayed, err := storage.newLegacyRecoveryWork(ctx, peerJob)
	if err != nil || delayed == nil {
		test.Fatal("second claim did not observe the same checkpoint", err)
	}
	defer func() {
		if err := delayed.Close(); err != nil {
			test.Error(err)
		}
	}()
	extent, err := delayed.source.Size()
	if err != nil {
		test.Fatal(err)
	}
	for copied := 0; copied < extent; copied += domain.ConversationManifestChunkBytes {
		progress, err := delayed.Step(ctx, peerJob)
		if err != nil || progress != outbound.DocRangeWorkPending {
			test.Fatal("delayed claim failed before retaining its unverified canonical spool", progress, err)
		}
	}
	if delayed.input.size != extent || delayed.canonical != nil || delayed.work != nil {
		test.Fatal("delayed claim did not stop before fresh verification")
	}
	var firstFailure error
	for step := 0; step < 64; step++ {
		if _, firstFailure = first.Step(ctx, job); firstFailure != nil {
			break
		}
	}
	if !errors.Is(firstFailure, domain.ErrPreparedDocRangeObject) {
		test.Fatal("first claim did not reject and discard the untrusted checkpoint", firstFailure)
	}
	stage, err = storage.stageLegacyByteSession(ctx, job, session, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || !stage.complete {
		test.Fatal("live claim could not rebuild the original stream", stage, err)
	}
	if stage.incarnation == "" || stage.incarnation == delayed.state.stage.incarnation {
		test.Fatal("recreated checkpoint reused the earlier stage incarnation")
	}
	if complete, err := storage.stageLegacyByteHeader(ctx, job, stage, seed); err != nil || !complete {
		test.Fatal("live claim could not rebuild the original envelope", complete, err)
	}
	if _, ready, err := storage.readLegacyByteRecovery(ctx, job); err != nil || !ready {
		test.Fatal("rebuilt checkpoint was not ready", ready, err)
	}
	checkpoint := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	var delayedFailure error
	for step := 0; step < 64; step++ {
		if _, delayedFailure = delayed.Step(ctx, peerJob); delayedFailure != nil {
			break
		}
	}
	if !errors.Is(delayedFailure, domain.ErrPreparedDocRangeObject) && !errors.Is(delayedFailure, domain.ErrConflict) {
		test.Fatal("delayed claim did not reject its older unverified spool", delayedFailure)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != checkpoint {
		test.Fatal("failure of the older claim deleted or changed another claim's rebuilt checkpoint")
	}
	if _, ready, err := storage.readLegacyByteRecovery(ctx, job); err != nil || !ready {
		test.Fatal("older failure made the rebuilt checkpoint unavailable", ready, err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("review failure changed original source, ownership, snapshots, jobs or publication")
	}
}

func TestLegacyByteRecoveryReviewPublicationDamageRemainsDerivedRetry(test *testing.T) {
	for _, damage := range []string{"hash", "missing"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			storage.ConfigureLegacyDerivedByteRangePublication(true)
			fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			legacyByteRecoveryFencePGStage(test, storage, ctx, job)
			work, err := storage.newLegacyRecoveryWork(ctx, job)
			if err != nil || work == nil {
				test.Fatal("prepare a fresh reverified publication", err)
			}
			defer work.Close()
			progress := outbound.DocRangeWorkPending
			for step := 0; progress == outbound.DocRangeWorkPending && step < 256; step++ {
				progress, err = work.Step(ctx, job)
				if err != nil {
					test.Fatal("prepare original publication before derived damage", err)
				}
			}
			if progress != outbound.DocRangeWorkPublishable || work.work == nil {
				test.Fatal("publication did not complete fresh verification and handoff", progress)
			}
			original := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
			legacyByteRecoveryFencePGDamage(test, storage, ctx, fixture, false, 0, damage)
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("derived publication damage was mislabeled as invalid or absent original source", damage, err)
			}
			if legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture) != original {
				test.Fatal("derived failure changed original conversation, memory or history")
			}
			if current := docRangeJobPGGet(test, storage, ctx, job.RepoID, job.DocHash); current.State != "running" || current.Version != job.Version {
				test.Fatal("derived failure rejected or replaced the original job", current)
			}
			var remaining int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash).Scan(&remaining); err != nil || remaining != 0 {
				test.Fatal("derived publication failure retained its current corrupt checkpoint", remaining, err)
			}
		})
	}
}
