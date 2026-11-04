//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docByteUpgradeContractPGStore struct {
	*legacyFindGenerationPGStore
	metadataDemands int
	byteDemands     int
}

func (storage *docByteUpgradeContractPGStore) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.metadataDemands++
	return storage.PostgresStore.EnqueueDocRangeJob(ctx, repo, hash, now)
}

func (storage *docByteUpgradeContractPGStore) EnqueueDocByteRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.byteDemands++
	return storage.PostgresStore.EnqueueDocByteRangeJob(ctx, repo, hash, now)
}

func TestPGDocByteUpgradeContractMetadataBasePreflightDoesNotPromote(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, metadata := seedDocByteUpgradeV3PG(test, fixture)
	prior := readDocByteUpgradePGJob(test, fixture, document.Hash)
	if prior.receipt.State != "completed" || prior.minimumVersion != 0 || prior.sourceVersion != nil {
		test.Fatal("metadata base must start with a completed metadata-only receipt", prior)
	}
	observed := &docByteUpgradeContractPGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
	fixture.service.blobs = observed
	before := docByteUpgradePGImage(test, fixture)
	for _, ctx := range []context.Context{fixture.ctx, outbound.WithDocReadOnly(fixture.ctx)} {
		for range 2 {
			if prepared, err := fixture.service.prepareDocViewSource(ctx, fixture.repo, document.Hash, docRangeLegacyMetadata); err != nil || !prepared {
				test.Fatal("view metadata preflight rejected an old v3 base", prepared, err)
			}
			if identity, err := fixture.service.prepareDocVisibleSource(ctx, fixture.repo, document.Hash, docRangeLegacyMetadata); err != nil || identity != domain.DocumentIdentityLegacy {
				test.Fatal("visible metadata preflight rejected an old v3 base", identity, err)
			}
			if observed.metadataDemands != 0 || observed.byteDemands != 0 {
				test.Fatal("metadata-only base preparation enqueued work", observed.metadataDemands, observed.byteDemands)
			}
			if after := docByteUpgradePGImage(test, fixture); after != before {
				test.Fatal("metadata-only base preparation changed physical state")
			}
		}
	}
	if prepared, err := fixture.service.prepareDocViewSource(fixture.ctx, fixture.repo, document.Hash, docRangeLegacyBytes); !errors.Is(err, domain.ErrDocRangeIndexPending) || !prepared {
		test.Fatal("byte demand accepted metadata-only completion", prepared, err)
	}
	promoted := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
	if promoted.receipt.Version != prior.receipt.Version+1 || observed.metadataDemands != 0 || observed.byteDemands != 1 {
		test.Fatal("byte demand did not promote exactly the existing metadata receipt", prior, promoted, observed.metadataDemands, observed.byteDemands)
	}
	if generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash); !reflect.DeepEqual(generation, metadata) {
		test.Fatal("byte preflight published or changed the old metadata generation", generation, metadata)
	}
	assertDocByteUpgradePGNoBodyReads(test, observed.cost)
}

func TestPGDocByteUpgradeContractVisibleAndPreparedFindDemand(test *testing.T) {
	for _, entrypoint := range []string{"visible", "prepared find", "native find"} {
		test.Run(entrypoint, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document, _ := seedDocByteUpgradeV3PG(test, fixture)
			prior := readDocByteUpgradePGJob(test, fixture, document.Hash)
			observed := &docByteUpgradeContractPGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
			fixture.service.blobs = observed
			before := docByteUpgradePGImage(test, fixture)
			var pendingImage string
			var pending docByteUpgradePGJob
			for attempt, readOnly := range []bool{true, true, false, false} {
				ctx := fixture.ctx
				want := domain.ErrDocRangeIndexPending
				if readOnly {
					ctx = outbound.WithDocReadOnly(ctx)
					want = domain.ErrDocRangeIndexUnavailable
				}
				switch entrypoint {
				case "visible":
					identity, err := fixture.service.prepareDocVisibleSource(ctx, fixture.repo, document.Hash, docRangeLegacyBytes)
					if !errors.Is(err, want) || identity != domain.DocumentIdentityLegacy {
						test.Fatal("visible byte preflight bypassed old v3 byte readiness", identity, err)
					}
				case "prepared find":
					page, err := fixture.service.FindDocPage(ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{
						Legacy: true, Representation: domain.DocFindPreparedRepresentation, Query: "original", Total: len(document.CIR.Events), To: len(document.CIR.Events),
					})
					if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
						test.Fatal("prepared find read compatibility bytes before byte preparation", page, err)
					}
				case "native find":
					request := domain.ConversationFindRequest{Query: "original"}
					if readOnly {
						page, err := fixture.service.QueryConversationFind(ctx, fixture.repo, document.Hash, request)
						var notReady *domain.ConversationFindNotReady
						if !errors.Is(err, want) || !errors.As(err, &notReady) || !reflect.DeepEqual(page, domain.ConversationFindPage{}) {
							test.Fatal("native find query bypassed old v3 byte readiness", page, err)
						}
					} else if err := fixture.service.RequestConversationFind(ctx, fixture.repo, document.Hash, request); err != nil {
						test.Fatal("native find demand did not accept pending byte preparation", err)
					}
				}
				assertDocByteUpgradePGNoBodyReads(test, observed.cost)
				if readOnly {
					if observed.metadataDemands != 0 || observed.byteDemands != 0 || !reflect.DeepEqual(readDocByteUpgradePGJob(test, fixture, document.Hash), prior) {
						test.Fatal("read-only preflight enqueued or promoted byte work", observed.metadataDemands, observed.byteDemands)
					}
					if after := docByteUpgradePGImage(test, fixture); after != before {
						test.Fatal("read-only byte preflight changed physical state")
					}
					continue
				}
				current := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
				if current.receipt.Version != prior.receipt.Version+1 || observed.metadataDemands != 0 || observed.byteDemands != attempt-1 {
					test.Fatal("byte preflight failed to retain one promoted range job", prior, current, observed.metadataDemands, observed.byteDemands)
				}
				image := docByteUpgradePGImage(test, fixture)
				if attempt == 2 {
					pending, pendingImage = current, image
				} else if !reflect.DeepEqual(current, pending) || image != pendingImage {
					test.Fatal("duplicate byte preflight rewrote the pending receipt or derived state", pending, current)
				}
			}
		})
	}
}

func TestPGDocByteUpgradeContractMonolithicSourceRequiresBackfill(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, _ := seedDocByteUpgradeV3PG(test, fixture)
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil || domain.HashContent(raw) != document.Hash {
		test.Fatal("monolithic fixture lost its original archival hash", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, raw); err != nil {
		test.Fatal(err)
	}
	fixture.storage.ConfigureLegacyByteRangePublication(false)
	prepareLegacyFindGenerationPG(test, fixture, document.Hash)
	fixture.storage.ConfigureLegacyByteRangePublication(true)
	metadata := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if metadata.Version != 3 || metadata.StreamBytes == 0 || metadata.ChunkPages != (domain.DocRangeDirectoryRoot{}) {
		test.Fatal("monolithic fixture must retain nonempty metadata-only v3", metadata)
	}
	sourceImage := legacyByteRangePGSourceImage(test, fixture)
	observed := &docByteUpgradeContractPGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
	fixture.service.blobs = observed
	if identity, err := fixture.service.prepareDocVisibleSource(fixture.ctx, fixture.repo, document.Hash, docRangeLegacyBytes); !errors.Is(err, domain.ErrDocRangeIndexPending) || identity != domain.DocumentIdentityLegacy {
		test.Fatal("valid monolithic bytes did not queue their minimum-v4 requirement", identity, err)
	}
	queued := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
	started := time.Now().UTC()
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); !errors.Is(err, domain.ErrDocByteRangeBackfillRequired) || errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("valid monolithic source was completed or classified as an invalid document", err)
	}
	retrying := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "retrying")
	if retrying.receipt.Reason != "byte_backfill_required" || retrying.receipt.Attempts != 1 || retrying.receipt.Version != queued.receipt.Version+1 || !retrying.receipt.LeaseUntil.IsZero() || !retrying.receipt.NextAttempt.After(started) {
		test.Fatal("backfill requirement did not persist a fenced retry with backoff", queued, retrying)
	}
	if ready, err := fixture.storage.HasDocRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || !ready {
		test.Fatal("backfill requirement removed valid metadata readiness", ready, err)
	}
	if ready, err := fixture.storage.HasDocByteRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || ready {
		test.Fatal("monolithic backfill requirement advertised bounded byte completion", ready, err)
	}
	if generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash); !reflect.DeepEqual(generation, metadata) {
		test.Fatal("failed byte worker replaced the valid metadata generation", generation, metadata)
	}
	before := docByteUpgradePGImage(test, fixture)
	for range 2 {
		if receipt, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC().Add(time.Hour)); err != nil || !reflect.DeepEqual(receipt, retrying.receipt) {
			test.Fatal("duplicate direct demand reset the durable backfill retry", receipt, retrying, err)
		}
		if prepared, err := fixture.service.prepareDocViewSource(fixture.ctx, fixture.repo, document.Hash, docRangeLegacyBytes); !errors.Is(err, domain.ErrDocByteRangeBackfillRequired) || !prepared {
			test.Fatal("view preflight hid the durable backfill requirement", prepared, err)
		}
		if identity, err := fixture.service.prepareDocVisibleSource(fixture.ctx, fixture.repo, document.Hash, docRangeLegacyBytes); !errors.Is(err, domain.ErrDocByteRangeBackfillRequired) || identity != domain.DocumentIdentityLegacy {
			test.Fatal("visible preflight hid the durable backfill requirement", identity, err)
		}
		if current := readDocByteUpgradePGJob(test, fixture, document.Hash); !reflect.DeepEqual(current, retrying) {
			test.Fatal("duplicate demand reset backoff, attempts or the source fence", retrying, current)
		}
		if after := docByteUpgradePGImage(test, fixture); after != before {
			test.Fatal("duplicate backfill demand changed physical state")
		}
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
		test.Fatal("byte worker or repeated demand rewrote the monolithic archival source")
	}
	assertDocByteUpgradePGNoBodyReads(test, observed.cost)
}

func TestPGDocByteUpgradeContractMalformedMetadataFailsClosed(test *testing.T) {
	for _, damage := range []string{"malformed descriptor", "descriptor hash mismatch", "wrong document hash"} {
		test.Run(damage, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document, generation := seedDocByteUpgradeV3PG(test, fixture)
			prior := readDocByteUpgradePGJob(test, fixture, document.Hash)
			raw, err := generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			descriptorHash := domain.HashContent(raw)
			switch damage {
			case "malformed descriptor":
				raw = []byte("malformed legacy v3 descriptor")
				descriptorHash = domain.HashContent(raw)
			case "descriptor hash mismatch":
				descriptorHash = domain.HashContent([]byte("different descriptor " + string(fixture.repo)))
			case "wrong document hash":
				generation.Hash = fixture.hash
				raw, err = json.Marshal(generation)
				if err != nil {
					test.Fatal(err)
				}
				descriptorHash = domain.HashContent(raw)
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2,generation_hash=$3 WHERE doc_hash=$1`, document.Hash, raw, descriptorHash); err != nil {
				test.Fatal(err)
			}
			observed := &docByteUpgradeContractPGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
			fixture.service.blobs = observed
			before := docByteUpgradePGImage(test, fixture)
			if ready, err := fixture.storage.HasDocByteRangeIndex(fixture.ctx, fixture.repo, document.Hash); !errors.Is(err, domain.ErrIntegrity) || ready {
				test.Fatal("malformed metadata was treated as cold or byte-ready", ready, err)
			}
			for _, ctx := range []context.Context{fixture.ctx, outbound.WithDocReadOnly(fixture.ctx)} {
				if prepared, err := fixture.service.prepareDocViewSource(ctx, fixture.repo, document.Hash, docRangeLegacyBytes); !errors.Is(err, domain.ErrIntegrity) || !prepared {
					test.Fatal("view byte preflight did not fail closed on malformed metadata", prepared, err)
				}
				if identity, err := fixture.service.prepareDocVisibleSource(ctx, fixture.repo, document.Hash, docRangeLegacyBytes); !errors.Is(err, domain.ErrIntegrity) || identity != domain.DocumentIdentityLegacy {
					test.Fatal("visible byte preflight did not fail closed on malformed metadata", identity, err)
				}
				page, err := fixture.service.FindDocPage(ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{
					Legacy: true, Representation: domain.DocFindPreparedRepresentation, Query: "original", Total: len(document.CIR.Events), To: len(document.CIR.Events),
				})
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
					test.Fatal("prepared find bypassed malformed metadata", page, err)
				}
			}
			if observed.metadataDemands != 0 || observed.byteDemands != 0 || !reflect.DeepEqual(readDocByteUpgradePGJob(test, fixture, document.Hash), prior) {
				test.Fatal("malformed metadata enqueued or promoted upgrade work", observed.metadataDemands, observed.byteDemands)
			}
			assertDocByteUpgradePGNoBodyReads(test, observed.cost)
			if after := docByteUpgradePGImage(test, fixture); after != before {
				test.Fatal("malformed metadata was repaired or changed during preflight")
			}
		})
	}
}

func TestPGDocByteUpgradeContractEmptyV3CompletesMinimumFourWithoutBody(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document := seedLegacyFindPGDocument(test, fixture, nil)
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil || domain.HashContent(raw) != document.Hash {
		test.Fatal("empty monolithic fixture lost its archival hash", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, raw); err != nil {
		test.Fatal(err)
	}
	fixture.storage.ConfigureLegacyByteRangePublication(false)
	prepareLegacyFindGenerationPG(test, fixture, document.Hash)
	fixture.storage.ConfigureLegacyByteRangePublication(true)
	metadata := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if metadata.Version != 3 || metadata.Identity() != domain.DocumentIdentityLegacy || metadata.EventCount != 0 || metadata.StreamBytes != 0 || metadata.ChunkPages != (domain.DocRangeDirectoryRoot{}) {
		test.Fatal("empty fixture must retain legacy v3 without a byte directory", metadata)
	}
	prior := readDocByteUpgradePGJob(test, fixture, document.Hash)
	if prior.receipt.State != "completed" || prior.minimumVersion != 0 || prior.sourceVersion != nil {
		test.Fatal("empty fixture must start with metadata-only completion", prior)
	}
	sourceImage := legacyByteRangePGSourceImage(test, fixture)
	if _, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	queued := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
	if queued.receipt.Version != prior.receipt.Version+1 {
		test.Fatal("empty byte requirement did not promote its existing receipt", prior, queued)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal("empty monolithic v3 unnecessarily required byte backfill", err)
	}
	completed := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	if completed.receipt.Attempts != 1 || completed.receipt.Version != queued.receipt.Version+1 || completed.receipt.Reason != "" || !completed.receipt.LeaseUntil.IsZero() {
		test.Fatal("empty minimum-v4 worker did not persist successful fenced completion", queued, completed)
	}
	if generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash); !reflect.DeepEqual(generation, metadata) {
		test.Fatal("empty byte completion unnecessarily changed the metadata-only layout", metadata, generation)
	}
	if ready, err := fixture.storage.HasDocByteRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || !ready {
		test.Fatal("zero-stream v3 did not satisfy byte readiness", ready, err)
	}
	observed := &docByteUpgradeContractPGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
	fixture.service.blobs = observed
	before := docByteUpgradePGImage(test, fixture)
	for range 2 {
		if receipt, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(receipt, completed.receipt) {
			test.Fatal("empty minimum-v4 completion was not idempotent", receipt, completed, err)
		}
		ctx := outbound.WithDocReadOnly(fixture.ctx)
		if identity, err := fixture.service.prepareDocVisibleSource(ctx, fixture.repo, document.Hash, docRangeLegacyBytes); err != nil || identity != domain.DocumentIdentityLegacy {
			test.Fatal("empty v3 visible byte preflight required a body directory", identity, err)
		}
		page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, document.Hash, "", 0, 1, "all")
		if err != nil || page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Total != 0 || page.Offset != 0 || page.Next != -1 || len(page.Events) != 0 || !reflect.DeepEqual(page.Envelope, document.CIR.Envelope) {
			test.Fatal("empty byte-ready v3 did not return its original empty selection", page, err)
		}
	}
	if observed.metadataDemands != 0 || observed.byteDemands != 0 {
		test.Fatal("empty read-only selection enqueued preparation", observed.metadataDemands, observed.byteDemands)
	}
	assertDocByteUpgradePGNoBodyReads(test, observed.cost)
	if after := docByteUpgradePGImage(test, fixture); after != before {
		test.Fatal("empty byte completion or read-only selection changed physical state")
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
		test.Fatal("empty minimum-v4 preparation rewrote its archival source")
	}
}
