//go:build postgres

package app

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGDocByteUpgradeDisabledWorkerReleasesExpiredByteLease(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, _ := seedDocByteUpgradeV3PG(test, fixture)
	if _, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	claimed, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || claimed.DocHash != document.Hash {
		test.Fatal("could not simulate interrupted byte preparation", claimed, err)
	}
	if _, err := fixture.storage.EnqueueDocRangeJob(fixture.ctx, fixture.repo, fixture.hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	fixture.storage.ConfigureLegacyByteRangePublication(false)
	if _, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("disabled worker bypassed an unexpired byte lease", err)
	}
	current := readDocByteUpgradePGJob(test, fixture, document.Hash)
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT epoch::text FROM repository_catalog_state WHERE repo_id=$1`, fixture.repo).Scan(&current.receipt.RepositoryEpoch); err != nil {
		test.Fatal(err)
	}
	if claimed.RepositoryEpoch == "" || !reflect.DeepEqual(current.receipt, claimed) {
		test.Fatal("disabled worker changed an unexpired byte lease", current, claimed)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, document.Hash); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("expired incompatible work should consume only one bounded release step", err)
	}
	current = readDocByteUpgradePGJob(test, fixture, document.Hash)
	if current.receipt.State != "waiting" || current.receipt.Version != claimed.Version+1 || !current.receipt.LeaseUntil.IsZero() || current.minimumVersion != 4 {
		test.Fatal("expired byte work was not preserved and fenced", current, claimed)
	}
	if err := fixture.storage.RenewDocRangeJob(fixture.ctx, claimed, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("expired byte holder renewed released work", err)
	}
	if err := fixture.storage.BuildDocRangeJob(fixture.ctx, claimed); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("expired byte holder published released work", err)
	}
	root, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || root.DocHash != fixture.hash {
		test.Fatal("disabled byte publication stranded unrelated Root work", root, err)
	}
	if err := fixture.storage.BuildDocRangeJob(fixture.ctx, root); err != nil {
		test.Fatal("unrelated Root work could not finish with byte publication off", err)
	}
	if _, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("disabled worker claimed deferred byte work", err)
	}
	fixture.storage.ConfigureLegacyByteRangePublication(true)
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal("deferred byte work could not resume after publication was reenabled", err)
	}
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
}

func TestPGDocByteUpgradeVerifiedUnknownHintsRemainReadOnlyReady(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, _ := seedDocByteUpgradeV3PG(test, fixture)
	if _, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	transaction, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(fixture.ctx) }()
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_range_generations_v2 DISABLE TRIGGER doc_range_layout_hint`); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `UPDATE doc_range_generations_v2 SET range_layout_version=0,range_stream_bytes=NULL WHERE doc_hash=$1`, document.Hash); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_range_generations_v2 ENABLE TRIGGER doc_range_layout_hint`); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	before := docByteUpgradePGImage(test, fixture)
	readOnly := outbound.WithDocReadOnly(fixture.ctx)
	if ready, err := fixture.storage.HasDocByteRangeIndex(readOnly, fixture.repo, document.Hash); err != nil || !ready {
		test.Fatal("unknown migration hints overrode a valid verified byte generation", ready, err)
	}
	fragment, err := fixture.service.ReadDocViewFragment(readOnly, fixture.repo, document.Hash, 0, 0, 64)
	original := docViewLegacyGenerationEventBytes(test, document, 0)
	if err != nil || fragment.JSON != string(original[:min(len(original), 64)]) {
		test.Fatal("read-only adoption demanded unnecessary writable preparation", fragment, err)
	}
	if after := docByteUpgradePGImage(test, fixture); after != before {
		test.Fatal("read-only adoption backfilled unknown hints or changed the receipt")
	}
}
