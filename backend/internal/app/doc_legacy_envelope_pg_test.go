//go:build postgres

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyEnvelopePGStore struct {
	*legacyFindGenerationPGStore
}

func (storage *legacyEnvelopePGStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *legacyEnvelopePGStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func assertLegacyEnvelopePGGeneration(test *testing.T, document domain.SessionDoc, generation domain.DocRangeGeneration, version int, byteStorage domain.DocRangeByteStorage) {
	test.Helper()
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	var original struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(canonical, &original); err != nil {
		test.Fatal(err)
	}
	if document.CIR.Envelope.CIRVersion != "" || !bytes.Contains(original.Envelope, []byte(`"cir_version":""`)) ||
		domain.HashContent(canonical) != document.Hash || generation.Hash != document.Hash || generation.Version != version ||
		generation.Identity() != domain.DocumentIdentityLegacy || generation.ByteStorage() != byteStorage ||
		generation.EventCount != int64(len(document.CIR.Events)) || !bytes.Equal(generation.Envelope, original.Envelope) ||
		(len(document.CIR.Events) == 0) != (generation.StreamBytes == 0) {
		test.Fatal("worker changed the canonical implicit-v1 envelope, source hash or publication layout", generation, string(original.Envelope))
	}
	normalized := document.CIR
	normalized.Envelope.CIRVersion = "1"
	explicit, err := domain.CanonicalBytes(normalized)
	if err != nil || domain.HashContent(explicit) == document.Hash {
		test.Fatal("implicit-v1 source was normalized to a different envelope before hashing", err)
	}
}

func assertLegacyEnvelopePGSelectedReads(test *testing.T, observed *legacyEnvelopePGStore, count int) {
	test.Helper()
	selected := min(count, 1)
	cost := observed.cost
	if cost.opens != 1 || cost.pages != selected || cost.chunkLookups != selected || cost.chunks != selected ||
		cost.directories != 0 || cost.manifests != 0 || cost.manifestBytes != 0 || cost.indexes != 0 || cost.whole != 0 ||
		cost.boundaries != 0 || cost.writes != 0 || cost.chunkBytes > domain.ChunkTarget ||
		cost.metadataBytes > domain.MaxDocRangeGenerationBytes+domain.MaxDocRangePageBytes+2*domain.MaxDocRangeDirectoryBytes ||
		(count == 0) != (cost.chunkBytes == 0) {
		test.Fatal("implicit-v1 public read used archival/index fallback instead of selected generation bytes", cost)
	}
}

func assertLegacyEnvelopePGPublicReads(test *testing.T, fixture docViewPGFixture, document domain.SessionDoc) {
	test.Helper()
	observed := &legacyEnvelopePGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
	fixture.service.blobs = observed
	count := len(document.CIR.Events)
	for _, ctx := range []context.Context{fixture.ctx, outbound.WithDocReadOnly(fixture.ctx)} {
		observed.cost = legacyFindGenerationCosts{}
		page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, document.Hash, "", 0, 20, "chat")
		if err != nil || page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Total != count ||
			page.Offset != 0 || page.Inherited != 0 || page.Next != -1 || len(page.Events) != count ||
			!reflect.DeepEqual(page.Envelope, document.CIR.Envelope) {
			test.Fatal("implicit-v1 public view changed the original identity, envelope or selection", page, err)
		}
		for position, event := range page.Events {
			if event.Index != position || event.Deferred || !reflect.DeepEqual(event.Event, document.CIR.Events[position]) {
				test.Fatal("implicit-v1 public view changed event content or order", position, event)
			}
		}
		assertLegacyEnvelopePGSelectedReads(test, observed, count)
		observed.cost = legacyFindGenerationCosts{}
		found, err := fixture.service.FindDocPage(ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{
			Legacy: true, Query: "needle🙂", From: 0, To: count, Total: count,
		})
		if err != nil || found.Projection != domain.DocLegacyFindProjection || found.Hash != document.Hash || found.Generation != "" ||
			found.From != 0 || found.To != count || found.NextCursor != nil || len(found.Matches) != count {
			test.Fatal("implicit-v1 public find changed the original legacy selection", found, err)
		}
		for position, match := range found.Matches {
			raw := docViewLegacyGenerationEventBytes(test, document, position)
			if match.Index != position || match.Field != 0 || match.Offset != len([]string{"first ", "second "}[position]) ||
				match.Length != len("Needle🙂") || match.Raw || match.RevealCursor != "" || match.SourceRange == nil ||
				match.SourceRange.From < 0 || match.SourceRange.To > len(raw) || match.SourceRange.To-match.SourceRange.From != len("Needle🙂") {
				test.Fatal("implicit-v1 find changed event order, text offsets or canonical source coordinates", match)
			}
			if string(raw[match.SourceRange.From:match.SourceRange.To]) != "Needle🙂" {
				test.Fatal("implicit-v1 find did not identify the original event bytes", match)
			}
		}
		assertLegacyEnvelopePGSelectedReads(test, observed, count)
	}
}

func assertLegacyEnvelopePGMetadataOnlyReads(test *testing.T, fixture docViewPGFixture, document domain.SessionDoc) {
	test.Helper()
	fixture.storage.ConfigureLegacyByteRangePublication(true)
	observed := &legacyEnvelopePGStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
	fixture.service.blobs = observed
	ctx := outbound.WithDocReadOnly(fixture.ctx)
	page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, document.Hash, "", 0, 20, "chat")
	if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(page, domain.DocViewPage{}) {
		test.Fatal("nonempty v3 view exposed archival compatibility content instead of requiring byte preparation", page, err)
	}
	fragment, err := fixture.service.ReadDocViewFragment(ctx, fixture.repo, document.Hash, 0, 0, 64)
	if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || fragment != (domain.DocViewFragment{}) {
		test.Fatal("nonempty v3 fragment exposed archival compatibility bytes", fragment, err)
	}
	found, err := fixture.service.FindDocPage(ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{
		Legacy: true, Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", To: len(document.CIR.Events), Total: len(document.CIR.Events),
	})
	if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(found, domain.DocFindPage{}) {
		test.Fatal("nonempty v3 prepared find exposed archival compatibility matches", found, err)
	}
	if observed.cost != (legacyFindGenerationCosts{}) {
		test.Fatal("read-only v3 byte demand loaded metadata or archival fallback", observed.cost)
	}
}

func TestPGLegacyEnvelopeWorkerPublicationPreservesImplicitV1(test *testing.T) {
	for _, publishBytes := range []bool{false, true} {
		for _, count := range []int{0, 2} {
			test.Run(fmt.Sprintf("byte_publication=%t/events=%d", publishBytes, count), func(test *testing.T) {
				fixture := newDocViewPGFixture(test)
				fixture.cir.Envelope.CIRVersion = ""
				fixture.storage.ConfigureLegacyByteRangePublication(publishBytes)
				fixture.storage.ConfigureLegacyDerivedByteRangePublication(false)
				events := []domain.CIREvent{
					historyMessage(domain.RoleUser, "first Needle🙂 \"quoted\"\n"+string(fixture.repo)),
					historyMessage(domain.RoleAssistant, "second Needle🙂 <tag> & answer"),
				}
				document := seedLegacyFindPGDocument(test, fixture, events[:count])
				sourceImage := legacyByteRangePGSourceImage(test, fixture)
				prepareLegacyFindGenerationPG(test, fixture, document.Hash)
				version, byteStorage := 3, domain.DocRangeByteStorageNone
				if publishBytes && count > 0 {
					version, byteStorage = 4, domain.DocRangeByteStorageOriginal
				}
				generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
				assertLegacyEnvelopePGGeneration(test, document, generation, version, byteStorage)
				completed := readDocByteUpgradePGJob(test, fixture, document.Hash)
				if completed.receipt.State != "completed" || completed.receipt.Attempts != 1 || completed.receipt.Reason != "" ||
					!completed.receipt.LeaseUntil.IsZero() || completed.minimumVersion != 0 || completed.sourceVersion != nil {
					test.Fatal("worker did not complete the original metadata demand", completed)
				}
				if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
					test.Fatal("implicit-v1 publication rewrote original source, chunks or ownership")
				}
				image := docByteUpgradePGImage(test, fixture)
				if version == 3 && count > 0 {
					assertLegacyEnvelopePGMetadataOnlyReads(test, fixture, document)
				} else {
					assertLegacyEnvelopePGPublicReads(test, fixture, document)
					peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
					if err != nil {
						test.Fatal(err)
					}
					defer peer.Close()
					peer.ConfigureLegacyByteRangePublication(false)
					peer.ConfigureLegacyDerivedByteRangePublication(false)
					peerFixture := fixture
					peerFixture.storage = peer
					peerFixture.service = NewService(peer, peer, nil, nil, peer)
					assertLegacyEnvelopePGPublicReads(test, peerFixture, document)
				}
				if current := readDocByteUpgradePGGeneration(test, fixture, document.Hash); !reflect.DeepEqual(current, generation) {
					test.Fatal("public read changed the original implicit-v1 generation", current, generation)
				}
				if after := docByteUpgradePGImage(test, fixture); after != image {
					test.Fatal("implicit-v1 view/find demand wrote source, indexes, range objects or durable receipts")
				}
				if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
					test.Fatal("implicit-v1 public read changed original data or source versions")
				}
			})
		}
	}
}

func TestPGLegacyEnvelopeMonolithicDerivedPublicationPreservesImplicitV1(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	fixture.cir.Envelope.CIRVersion = ""
	document, observed := prepareLegacyDerivedPublicationPG(test, fixture)
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	assertLegacyEnvelopePGGeneration(test, document, generation, 5, domain.DocRangeByteStorageDerived)
	sourceImage := legacyByteRangePGSourceImage(test, fixture)
	image := docByteUpgradePGImage(test, fixture)
	assertLegacyDerivedPublicationPGPublicReads(test, fixture, document, observed)
	readOnlyFixture := fixture
	readOnlyFixture.ctx = outbound.WithDocReadOnly(fixture.ctx)
	assertLegacyDerivedPublicationPGPublicReads(test, readOnlyFixture, document, observed)
	peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	peer.ConfigureLegacyByteRangePublication(false)
	peer.ConfigureLegacyDerivedByteRangePublication(false)
	peerFixture := fixture
	peerFixture.storage = peer
	peerFixture.service = NewService(peer, peer, nil, nil, peer)
	peerObserved := observeLegacyDerivedPublicationPG(test, peerFixture, document.Hash)
	assertLegacyDerivedPublicationPGPublicReads(test, peerFixture, document, peerObserved)
	peerFixture.ctx = outbound.WithDocReadOnly(peerFixture.ctx)
	assertLegacyDerivedPublicationPGPublicReads(test, peerFixture, document, peerObserved)
	if current := readDocByteUpgradePGGeneration(test, fixture, document.Hash); !reflect.DeepEqual(current, generation) {
		test.Fatal("reader-only peer replaced the implicit-v1 derived generation", current, generation)
	}
	if after := docByteUpgradePGImage(test, fixture); after != image {
		test.Fatal("implicit-v1 derived reads changed source, publication objects or completed demand")
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
		test.Fatal("implicit-v1 derived reads changed the original monolithic data image")
	}
}
