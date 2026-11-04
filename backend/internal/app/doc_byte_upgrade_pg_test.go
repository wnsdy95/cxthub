//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docByteUpgradePGJob struct {
	receipt        domain.DocRangeIndexJob
	minimumVersion int
	sourceVersion  *int64
}

func readDocByteUpgradePGJob(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash) docByteUpgradePGJob {
	test.Helper()
	var current docByteUpgradePGJob
	var lease *time.Time
	err := fixture.pool.QueryRow(fixture.ctx, `SELECT repo_id,doc_hash,base_hash,state,reason,attempts,version,created_at,next_attempt,lease_until,minimum_range_version,byte_source_version
 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, hash).Scan(
		&current.receipt.RepoID, &current.receipt.DocHash, &current.receipt.BaseHash, &current.receipt.State, &current.receipt.Reason,
		&current.receipt.Attempts, &current.receipt.Version, &current.receipt.CreatedAt, &current.receipt.NextAttempt, &lease,
		&current.minimumVersion, &current.sourceVersion)
	if err != nil {
		test.Fatal(err)
	}
	if lease != nil {
		current.receipt.LeaseUntil = *lease
	}
	if err := current.receipt.Validate(); err != nil {
		test.Fatal(err)
	}
	var count int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, hash).Scan(&count); err != nil || count != 1 {
		test.Fatal("byte demand forked the existing range job identity", count, err)
	}
	return current
}

func readDocByteUpgradePGGeneration(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash) domain.DocRangeGeneration {
	test.Helper()
	var descriptor []byte
	var generationHash domain.ContentHash
	var layoutVersion int
	var streamBytes *int64
	var sourceVersion, currentVersion int64
	err := fixture.pool.QueryRow(fixture.ctx, `SELECT generation.generation_hash,generation.descriptor,generation.range_layout_version,generation.range_stream_bytes,generation.source_version,source.doc_range_source_version
 FROM doc_range_generations_v2 generation JOIN blobs source ON source.hash=generation.doc_hash WHERE generation.doc_hash=$1`, hash).Scan(
		&generationHash, &descriptor, &layoutVersion, &streamBytes, &sourceVersion, &currentVersion)
	if err != nil {
		test.Fatal(err)
	}
	generation, err := domain.DecodeDocRangeGeneration(hash, generationHash, descriptor)
	if err != nil || layoutVersion != generation.Version || streamBytes == nil || *streamBytes != generation.StreamBytes || sourceVersion != currentVersion {
		test.Fatal("generation hints or publication no longer describe the current original source", generation, layoutVersion, streamBytes, sourceVersion, currentVersion, err)
	}
	return generation
}

func seedDocByteUpgradeV3PG(test *testing.T, fixture docViewPGFixture) (domain.SessionDoc, domain.DocRangeGeneration) {
	test.Helper()
	fixture.storage.ConfigureLegacyByteRangePublication(false)
	defer fixture.storage.ConfigureLegacyByteRangePublication(true)
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		historyMessage(domain.RoleUser, "original byte upgrade source "+string(fixture.repo)),
		historyMessage(domain.RoleAssistant, strings.Repeat("original \"quoted\" bytes 🙂\n", 8)),
	})
	before := legacyByteRangePGSourceImage(test, fixture)
	prepareLegacyFindGenerationPG(test, fixture, document.Hash)
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if generation.Version != 3 || generation.StreamBytes == 0 || generation.Identity() != domain.DocumentIdentityLegacy {
		test.Fatal("old metadata fixture must have nonempty legacy v3 content", generation)
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != before {
		test.Fatal("old metadata preparation rewrote archival bytes or ownership")
	}
	return document, generation
}

func docByteUpgradePGImage(test *testing.T, fixture docViewPGFixture) string {
	test.Helper()
	var objects string
	err := fixture.pool.QueryRow(fixture.ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(objects.doc_hash,objects.hash,encode(objects.bytes,'hex'),objects.xmin::text) ORDER BY objects.doc_hash,objects.hash),'[]'::jsonb)::text
 FROM doc_range_objects_v2 objects WHERE objects.doc_hash IN (SELECT hash FROM repo_blobs WHERE repo_id=$1 AND kind='doc')`, fixture.repo).Scan(&objects)
	if err != nil {
		test.Fatal(err)
	}
	return legacyFindPGImage(test, fixture) + "\n" + objects
}

func assertDocByteUpgradePGNoBodyReads(test *testing.T, costs legacyFindGenerationCosts) {
	test.Helper()
	if costs.pages != 0 || costs.manifests != 0 || costs.manifestBytes != 0 || costs.chunks != 0 || costs.chunkBytes != 0 || costs.chunkLookups != 0 || costs.indexes != 0 || costs.whole != 0 {
		test.Fatal("pending byte demand read event metadata, a whole index, a manifest or original bytes", costs)
	}
}

func assertDocByteUpgradePGRequirement(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash, state string) docByteUpgradePGJob {
	test.Helper()
	current := readDocByteUpgradePGJob(test, fixture, hash)
	var sourceVersion int64
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, hash).Scan(&sourceVersion); err != nil {
		test.Fatal(err)
	}
	if current.minimumVersion != 4 || current.sourceVersion == nil || *current.sourceVersion != sourceVersion || current.receipt.State != state {
		test.Fatal("byte receipt lost its minimum layout or current source fence", current, sourceVersion, state)
	}
	if state == "waiting" && (!current.receipt.LeaseUntil.IsZero() || current.receipt.Attempts != 0 || current.receipt.Reason != "") {
		test.Fatal("promoted byte request retained a weaker lease or retry state", current)
	}
	return current
}

func TestPGDocByteUpgradeReadOnlyOldMetadataDoesNotEnqueue(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, _ := seedDocByteUpgradeV3PG(test, fixture)
	prior := readDocByteUpgradePGJob(test, fixture, document.Hash)
	if prior.receipt.State != "completed" || prior.minimumVersion != 0 || prior.sourceVersion != nil {
		test.Fatal("fixture is not a completed metadata-only receipt", prior)
	}
	if ready, err := fixture.storage.HasDocRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || !ready {
		test.Fatal("byte rollout removed existing metadata readiness", ready, err)
	}
	if ready, err := fixture.storage.HasDocByteRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || ready {
		test.Fatal("old v3 metadata advertised bounded original bytes", ready, err)
	}
	if receipt, err := fixture.storage.EnqueueDocRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(receipt, prior.receipt) {
		test.Fatal("metadata API no longer accepts its own completed receipt", receipt, err)
	}
	observed := newLegacyFindGenerationPGStore(fixture)
	fixture.service.blobs = observed
	before := docByteUpgradePGImage(test, fixture)
	readOnly := outbound.WithDocReadOnly(fixture.ctx)
	for range 2 {
		page, err := fixture.service.ReadDocViewEvents(readOnly, fixture.repo, document.Hash, "", 0, 2, "chat")
		if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(page, domain.DocViewPage{}) {
			test.Fatal("read-only v3 body demand exposed compatibility content", page, err)
		}
		fragment, err := fixture.service.ReadDocViewFragment(readOnly, fixture.repo, document.Hash, 1, 0, 64)
		if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || fragment != (domain.DocViewFragment{}) {
			test.Fatal("read-only v3 fragment demand exposed compatibility content", fragment, err)
		}
	}
	assertDocByteUpgradePGNoBodyReads(test, observed.cost)
	if after := docByteUpgradePGImage(test, fixture); after != before {
		test.Fatal("read-only byte demand changed source, metadata, range objects or the completed queue receipt")
	}
}

func TestPGDocByteUpgradeDemandPromotesCompletedMetadataAndSurvivesRestart(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, metadata := seedDocByteUpgradeV3PG(test, fixture)
	prior := readDocByteUpgradePGJob(test, fixture, document.Hash)
	sourceImage := legacyByteRangePGSourceImage(test, fixture)
	observed := newLegacyFindGenerationPGStore(fixture)
	fixture.service.blobs = observed
	var pendingImage string
	for attempt := 0; attempt < 2; attempt++ {
		page, err := fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, document.Hash, "", 0, 2, "chat")
		if !errors.Is(err, domain.ErrDocRangeIndexPending) || !reflect.DeepEqual(page, domain.DocViewPage{}) {
			test.Fatal("completed metadata receipt was incorrectly accepted as bounded byte completion", page, err)
		}
		current := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
		if current.receipt.Version != prior.receipt.Version+1 {
			test.Fatal("duplicate demand changed the promotion fence", prior, current)
		}
		image := docByteUpgradePGImage(test, fixture)
		if attempt != 0 && image != pendingImage {
			test.Fatal("duplicate byte demand rewrote the pending receipt or derived state")
		}
		pendingImage = image
	}
	assertDocByteUpgradePGNoBodyReads(test, observed.cost)
	peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	peer.ConfigureLegacyByteRangePublication(false)
	if claim, err := peer.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("publication-off writer claimed minimum-v4 work", claim, err)
	}
	if after := docByteUpgradePGImage(test, fixture); after != pendingImage {
		test.Fatal("publication-off claimant modified a byte job it could not fulfill")
	}
	peer.ConfigureLegacyByteRangePublication(true)
	restarted := NewService(peer, peer, nil, nil, peer)
	if err := restarted.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal("restarted range worker did not upgrade old v3 metadata", err)
	}
	completed := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	upgraded := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if upgraded.Version != 4 || upgraded.ChunkPages.Count == 0 {
		test.Fatal("worker completed without publishing an original-byte directory", upgraded)
	}
	upgraded.Version, upgraded.ChunkPages = 3, domain.DocRangeDirectoryRoot{}
	if !reflect.DeepEqual(upgraded, metadata) {
		test.Fatal("v4 upgrade changed archival identity, envelope, event metadata or stream extent", metadata, upgraded)
	}
	if ready, err := peer.HasDocByteRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || !ready {
		test.Fatal("restarted worker did not publish durable byte readiness", ready, err)
	}
	image := docByteUpgradePGImage(test, fixture)
	if receipt, err := peer.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(receipt, completed.receipt) {
		test.Fatal("completed byte receipt was not idempotent after restart", receipt, err)
	}
	observed.cost = legacyFindGenerationCosts{}
	page, err := fixture.service.ReadDocViewEvents(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash, "", 0, 2, "chat")
	if err != nil || page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Total != len(document.CIR.Events) || page.Offset != 0 || page.Next != -1 || len(page.Events) != len(document.CIR.Events) || !reflect.DeepEqual(page.Envelope, document.CIR.Envelope) {
		test.Fatal("upgraded body lost original identity, envelope or selection", page, err)
	}
	for index, event := range page.Events {
		if event.Index != index || !reflect.DeepEqual(event.Event, document.CIR.Events[index]) {
			test.Fatal("byte upgrade changed archival event content or order", index, event)
		}
	}
	if observed.cost.pages != 1 || observed.cost.manifests != 0 || observed.cost.manifestBytes != 0 || observed.cost.indexes != 0 || observed.cost.whole != 0 || observed.cost.chunkLookups < 1 || observed.cost.chunks != observed.cost.chunkLookups {
		test.Fatal("upgraded body fell back to a manifest or whole-index/raw read", observed.cost)
	}
	observed.cost = legacyFindGenerationCosts{}
	original := docViewLegacyGenerationEventBytes(test, document, 1)
	fragment, err := fixture.service.ReadDocViewFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash, 1, 0, len(original))
	if err != nil || fragment.Index != 1 || fragment.Offset != 0 || fragment.Next != -1 || fragment.Total != len(original) || fragment.JSON != string(original) || observed.cost.pages != 1 || observed.cost.manifests != 0 || observed.cost.manifestBytes != 0 || observed.cost.indexes != 0 || observed.cost.whole != 0 || observed.cost.chunkLookups < 1 || observed.cost.chunkLookups > 2 || observed.cost.chunks != observed.cost.chunkLookups {
		test.Fatal("upgraded fragment did not return exact original bytes without a manifest", fragment, observed.cost, err)
	}
	if after := docByteUpgradePGImage(test, fixture); after != image {
		test.Fatal("completed receipt or read-only v4 reads changed persisted state")
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
		test.Fatal("byte upgrade normalized or rewrote original source bytes, chunks or ownership")
	}
}

func TestPGDocByteUpgradeDirectEnqueueReopensChangedSource(test *testing.T) {
	for _, state := range []string{"waiting", "running", "completed"} {
		test.Run(state, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document, _ := seedDocByteUpgradeV3PG(test, fixture)
			if _, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			var claimed domain.DocRangeIndexJob
			if state != "waiting" {
				var err error
				claimed, err = fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute)
				if err != nil || claimed.RepoID != fixture.repo || claimed.DocHash != document.Hash || claimed.State != "running" {
					test.Fatal("could not claim byte request", claimed, err)
				}
				if state == "completed" {
					if err := fixture.storage.BuildDocRangeJob(fixture.ctx, claimed); err != nil {
						test.Fatal(err)
					}
				}
			}
			prior := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, state)
			before := docByteUpgradePGImage(test, fixture)
			for range 2 {
				receipt, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC())
				if err != nil || !reflect.DeepEqual(receipt, prior.receipt) {
					test.Fatal("duplicate current-source request changed its durable receipt", receipt, prior, err)
				}
			}
			if after := docByteUpgradePGImage(test, fixture); after != before {
				test.Fatal("duplicate direct enqueue rewrote the source or current request")
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, document.Hash); err != nil {
				test.Fatal(err)
			}
			if ready, err := fixture.storage.HasDocByteRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || ready {
				test.Fatal("changed source retained old byte readiness", ready, err)
			}
			receipt, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC())
			current := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
			if err != nil || !reflect.DeepEqual(receipt, current.receipt) || current.receipt.Version != prior.receipt.Version+1 || *current.sourceVersion <= *prior.sourceVersion {
				test.Fatal("source-version change reused a stale receipt instead of reopening one request", receipt, prior, current, err)
			}
			image := docByteUpgradePGImage(test, fixture)
			if state == "running" {
				if err := fixture.storage.RenewDocRangeJob(fixture.ctx, claimed, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("old source token renewed the reopened request", err)
				}
				if err := fixture.storage.BuildDocRangeJob(fixture.ctx, claimed); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("old source token published into the reopened request", err)
				}
			}
			if duplicate, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(duplicate, current.receipt) {
				test.Fatal("reopened byte request was not idempotent", duplicate, err)
			}
			if after := docByteUpgradePGImage(test, fixture); after != image {
				test.Fatal("stale token or duplicate enqueue changed the reopened request")
			}
			sourceImage := legacyByteRangePGSourceImage(test, fixture)
			if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
				test.Fatal("reopened source did not finish bounded byte preparation", err)
			}
			assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
			if generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash); generation.Version != 4 {
				test.Fatal("reopened job completed with weaker metadata", generation)
			}
			if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
				test.Fatal("reopened worker rewrote original source bytes")
			}
		})
	}
}

func TestPGDocByteUpgradePromotesWaitingAndRunningMetadata(test *testing.T) {
	for _, state := range []string{"waiting", "running"} {
		test.Run(state, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{historyMessage(domain.RoleUser, "weaker metadata claim "+string(fixture.repo))})
			if _, err := fixture.storage.EnqueueDocRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
			if err != nil {
				test.Fatal(err)
			}
			defer peer.Close()
			peer.ConfigureLegacyByteRangePublication(false)
			var weaker domain.DocRangeIndexJob
			if state == "running" {
				weaker, err = peer.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute)
				if err != nil || weaker.RepoID != fixture.repo || weaker.DocHash != document.Hash || weaker.State != "running" || weaker.LeaseUntil.IsZero() {
					test.Fatal("publication-off writer could not claim metadata-only work", weaker, err)
				}
			}
			prior := readDocByteUpgradePGJob(test, fixture, document.Hash)
			if prior.minimumVersion != 0 || prior.sourceVersion != nil || prior.receipt.State != state {
				test.Fatal("fixture no longer represents a weaker metadata request", prior)
			}
			receipt, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC())
			current := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
			if err != nil || !reflect.DeepEqual(receipt, current.receipt) || current.receipt.Version <= prior.receipt.Version {
				test.Fatal("byte promotion did not fence the weaker metadata request", receipt, prior, current, err)
			}
			image := docByteUpgradePGImage(test, fixture)
			if state == "running" {
				if err := peer.RenewDocRangeJob(fixture.ctx, weaker, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("weaker metadata lease renewed after byte promotion", err)
				}
				if err := peer.BuildDocRangeJob(fixture.ctx, weaker); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("old metadata builder completed a promoted byte request", err)
				}
			}
			if metadata, err := peer.EnqueueDocRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(metadata, current.receipt) {
				test.Fatal("metadata demand downgraded or forked a promoted byte request", metadata, err)
			}
			if after := docByteUpgradePGImage(test, fixture); after != image {
				test.Fatal("weaker builder, renewal or metadata enqueue changed the promoted request")
			}
			if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
				test.Fatal("promoted metadata work did not complete through the existing worker", err)
			}
			assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
			if generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash); generation.Version != 4 {
				test.Fatal("promoted metadata work published a weaker layout", generation)
			}
		})
	}
}

func assertDocByteUpgradePGProtocolRejected(test *testing.T, fixture docViewPGFixture, job domain.DocRangeIndexJob, protocol, code, statement string) {
	test.Helper()
	before := docByteUpgradePGImage(test, fixture)
	transaction, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(fixture.ctx)) }()
	if _, err := transaction.Exec(fixture.ctx, `SELECT set_config('cxt.doc_range_worker_protocol',$1,true)`, protocol); err != nil {
		test.Fatal(err)
	}
	_, err = transaction.Exec(fixture.ctx, statement, job.RepoID, job.DocHash, job.Version)
	var rejected *pgconn.PgError
	if !errors.As(err, &rejected) || rejected.Code != code {
		test.Fatal("database did not reject the incompatible byte transition", protocol, code, err)
	}
	if err := transaction.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if after := docByteUpgradePGImage(test, fixture); after != before {
		test.Fatal("rejected worker protocol transition changed persisted state")
	}
}

func TestPGDocByteUpgradeDatabaseRejectsProtocolThreeAndMetadataCompletion(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, _ := seedDocByteUpgradeV3PG(test, fixture)
	queued, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
	assertDocByteUpgradePGProtocolRejected(test, fixture, queued, "3", "0A000", `UPDATE doc_range_jobs_v1
 SET state='running',attempts=attempts+1,version=version+1,lease_until=clock_timestamp()+interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND version=$3 AND state='waiting'`)
	claimed, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || claimed.RepoID != fixture.repo || claimed.DocHash != document.Hash || claimed.State != "running" {
		test.Fatal("publication-enabled worker could not claim minimum-v4 work", claimed, err)
	}
	completion := `UPDATE doc_range_jobs_v1 SET state='completed',reason='',lease_until=NULL
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND version=$3 AND state='running'`
	assertDocByteUpgradePGProtocolRejected(test, fixture, claimed, "3", "0A000", completion)
	assertDocByteUpgradePGProtocolRejected(test, fixture, claimed, "4", "23514", completion)
	if err := fixture.storage.BuildDocRangeJob(fixture.ctx, claimed); err != nil {
		test.Fatal("current byte worker could not complete after rejected protocol probes", err)
	}
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
}
