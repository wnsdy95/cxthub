//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyDerivedPublicationPGStore struct {
	*legacyFindGenerationPGStore
	test         *testing.T
	repo         domain.ContentHash
	hash         domain.ContentHash
	ordinals     []int
	derivedBytes int
	failure      error
}

var _ outbound.LegacyDerivedByteRangeReader = (*legacyDerivedPublicationPGStore)(nil)

func observeLegacyDerivedPublicationPG(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash) *legacyDerivedPublicationPGStore {
	test.Helper()
	observed := &legacyDerivedPublicationPGStore{
		legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture),
		test:                        test,
		repo:                        fixture.repo,
		hash:                        hash,
	}
	fixture.service.blobs = observed
	return observed
}

func (storage *legacyDerivedPublicationPGStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *legacyDerivedPublicationPGStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func (storage *legacyDerivedPublicationPGStore) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func (storage *legacyDerivedPublicationPGStore) ReadLegacyDerivedByteChunk(ctx context.Context, repo, hash domain.ContentHash, binding domain.DocReadGenerationBinding, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
	storage.test.Helper()
	if repo != storage.repo || hash != storage.hash || !outbound.DocReadOnly(ctx) || !storage.InReadOnlyTransaction(ctx) ||
		binding.Validate() != nil || binding != storage.bindings[hash] || descriptor.Bytes <= 0 || descriptor.Bytes > domain.ConversationManifestChunkBytes {
		storage.test.Fatal("derived read lost its selected source, generation binding, snapshot or byte bound", repo, hash, binding, ordinal, descriptor)
	}
	storage.ordinals = append(storage.ordinals, ordinal)
	if ordinal == 1 && storage.failure != nil {
		return nil, storage.failure
	}
	body, err := storage.PostgresStore.ReadLegacyDerivedByteChunk(ctx, repo, hash, binding, ordinal, descriptor)
	if err == nil && (int64(len(body)) != descriptor.Bytes || domain.HashContent(body) != descriptor.Hash) {
		storage.test.Fatal("PostgreSQL returned bytes outside the selected derived descriptor", ordinal, descriptor, len(body))
	}
	storage.derivedBytes += len(body)
	return body, err
}

func (storage *legacyDerivedPublicationPGStore) resetReads() {
	storage.cost = legacyFindGenerationCosts{}
	storage.ordinals = nil
	storage.derivedBytes = 0
}

func assertLegacyDerivedPublicationPGReads(test *testing.T, observed *legacyDerivedPublicationPGStore, ordinals []int) {
	test.Helper()
	cost := observed.cost
	if !reflect.DeepEqual(observed.ordinals, ordinals) || cost.opens != 1 || cost.pages != 1 || cost.chunkLookups != len(ordinals) ||
		cost.directories != 0 || cost.manifests != 0 || cost.manifestBytes != 0 || cost.chunks != 0 || cost.chunkBytes != 0 ||
		cost.indexes != 0 || cost.whole != 0 || cost.boundaries != 0 || cost.writes != 0 ||
		cost.metadataBytes > domain.MaxDocRangeGenerationBytes+domain.MaxDocRangePageBytes+2*domain.MaxDocRangeDirectoryBytes ||
		observed.derivedBytes > len(ordinals)*domain.ConversationManifestChunkBytes {
		test.Fatal("public read bypassed selected derived chunks or loaded archival/index fallback", observed.ordinals, ordinals, observed.derivedBytes, cost)
	}
}

func prepareLegacyDerivedPublicationPG(test *testing.T, fixture docViewPGFixture) (domain.SessionDoc, *legacyDerivedPublicationPGStore) {
	test.Helper()
	fixture.storage.ConfigureLegacyByteRangePublication(true)
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("x", domain.ConversationManifestChunkBytes+128)},
		historyMessage(domain.RoleUser, "first Needle🙂 \"quoted\"\n"+string(fixture.repo)),
		historyMessage(domain.RoleAssistant, "second Needle🙂 <tag> & answer"),
	})
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	if domain.HashContent(canonical) != document.Hash {
		test.Fatal("monolithic fixture does not retain its canonical legacy identity")
	}
	updated, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, canonical)
	if err != nil || updated.RowsAffected() != 1 {
		test.Fatal("could not seed the monolithic original", updated, err)
	}
	sourceImage := legacyByteRangePGSourceImage(test, fixture)
	observed := observeLegacyDerivedPublicationPG(test, fixture, document.Hash)
	page, err := fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, document.Hash, "", 1, 2, "chat")
	if !errors.Is(err, domain.ErrDocRangeIndexPending) || len(page.Events) != 0 {
		test.Fatal("normal monolithic demand did not queue bounded preparation", page, err)
	}
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
	if observed.cost != (legacyFindGenerationCosts{}) || len(observed.ordinals) != 0 {
		test.Fatal("cold public demand read metadata or original bytes instead of queueing", observed.cost, observed.ordinals)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal("normal range worker did not publish monolithic derived bytes", err)
	}
	completed := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	if completed.receipt.Attempts != 1 || !completed.receipt.LeaseUntil.IsZero() || completed.receipt.Reason != "" {
		test.Fatal("derived publication did not complete the existing demand receipt", completed)
	}
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if generation.Version != 5 || generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.Hash != document.Hash ||
		generation.Identity() != domain.DocumentIdentityLegacy || generation.EventCount != int64(len(document.CIR.Events)) ||
		generation.ChunkCount() != 2 || generation.StreamBytes <= domain.ConversationManifestChunkBytes || generation.StreamBytes > 2*domain.ConversationManifestChunkBytes {
		test.Fatal("worker did not publish a bounded v5 generation for the original legacy document", generation)
	}
	if ready, err := fixture.storage.HasDocByteRangeIndex(fixture.ctx, fixture.repo, document.Hash); err != nil || !ready {
		test.Fatal("derived generation was not durably byte-ready", ready, err)
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
		test.Fatal("derived publication rewrote original source bytes, chunks or ownership")
	}
	return document, observed
}

func assertLegacyDerivedPublicationPGPublicReads(test *testing.T, fixture docViewPGFixture, document domain.SessionDoc, observed *legacyDerivedPublicationPGStore) {
	test.Helper()
	assertLegacyDerivedPublicationPGPublicReadsAt(test, fixture, document, observed, 1)
}

func assertLegacyDerivedPublicationPGPublicReadsAt(test *testing.T, fixture docViewPGFixture, document domain.SessionDoc, observed *legacyDerivedPublicationPGStore, ordinal int) {
	test.Helper()
	observed.resetReads()
	page, err := fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, document.Hash, "", 1, 2, "chat")
	if err != nil || page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy || page.Total != len(document.CIR.Events) ||
		page.Offset != 1 || page.Inherited != 0 || page.Next != -1 || len(page.Events) != 2 || !reflect.DeepEqual(page.Envelope, document.CIR.Envelope) {
		test.Fatal("derived public body changed archival identity, envelope or selection", page, err)
	}
	for position, event := range page.Events {
		if event.Index != position+1 || event.Deferred || !reflect.DeepEqual(event.Event, document.CIR.Events[position+1]) {
			test.Fatal("derived public body changed event content or order", position, event)
		}
	}
	assertLegacyDerivedPublicationPGReads(test, observed, []int{ordinal})
	if observed.derivedBytes <= 0 {
		test.Fatal("public body did not load selected derived bytes")
	}
	observed.resetReads()
	found, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{
		Legacy: true, Query: "needle🙂", From: 1, To: 3, Total: len(document.CIR.Events),
	})
	if err != nil || found.Projection != domain.DocLegacyFindProjection || found.Hash != document.Hash || found.Generation != "" ||
		found.From != 1 || found.To != 3 || found.NextCursor != nil || len(found.Matches) != 2 {
		test.Fatal("derived public find changed the original legacy selection", found, err)
	}
	assertLegacyDerivedPublicationPGReads(test, observed, []int{ordinal})
	for position, match := range found.Matches {
		raw := docViewLegacyGenerationEventBytes(test, document, position+1)
		if match.Index != position+1 || match.Field != 0 || match.Offset != len([]string{"first ", "second "}[position]) ||
			match.Length != len("Needle🙂") || match.Raw || match.RevealCursor != "" || match.SourceRange == nil ||
			match.SourceRange.From < 0 || match.SourceRange.To > len(raw) || match.SourceRange.To-match.SourceRange.From != len("Needle🙂") {
			test.Fatal("derived find changed event order, text offsets or source coordinates", match)
		}
		if string(raw[match.SourceRange.From:match.SourceRange.To]) != "Needle🙂" {
			test.Fatal("derived match does not identify the original canonical bytes", match)
		}
		observed.resetReads()
		revealed, err := fixture.service.ReadDocFindFragment(fixture.ctx, fixture.repo, document.Hash, domain.DocFindFragmentRequest{
			Legacy: true, Index: match.Index, Total: len(document.CIR.Events), Offset: match.SourceRange.From, Budget: 64,
		})
		end := min(match.SourceRange.From+64, len(raw))
		next := end
		if end == len(raw) {
			next = -1
		}
		if err != nil || revealed.Projection != domain.DocLegacyFindFragmentProjection || revealed.Hash != document.Hash || revealed.Generation != "" ||
			revealed.Index != match.Index || revealed.Offset != match.SourceRange.From || revealed.Total != len(raw) || revealed.Next != next ||
			revealed.Previous != max(0, match.SourceRange.From-64) || revealed.JSON != string(raw[match.SourceRange.From:end]) {
			test.Fatal("derived reveal did not return exact original event bytes and identity", revealed, err)
		}
		assertLegacyDerivedPublicationPGReads(test, observed, []int{ordinal})
	}
}

func TestPGLegacyDerivedPublicationServesMonolithicViewFindAndReveal(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, observed := prepareLegacyDerivedPublicationPG(test, fixture)
	image := docByteUpgradePGImage(test, fixture)
	assertLegacyDerivedPublicationPGPublicReads(test, fixture, document, observed)
	observed.resetReads()
	raw := docViewLegacyGenerationEventBytes(test, document, 0)
	offset := domain.ConversationManifestChunkBytes - 32
	fragment, err := fixture.service.ReadDocViewFragment(fixture.ctx, fixture.repo, document.Hash, 0, offset, 64)
	if err != nil || fragment.Index != 0 || fragment.Offset != offset || fragment.Next != offset+64 || fragment.Total != len(raw) || fragment.JSON != string(raw[offset:offset+64]) {
		test.Fatal("shared derived chunk reader lost bytes across the private 512KiB boundary", fragment, err)
	}
	assertLegacyDerivedPublicationPGReads(test, observed, []int{0, 1})
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if observed.derivedBytes != int(generation.StreamBytes) {
		test.Fatal("cross-boundary fragment loaded more than the two selected canonical stages", observed.derivedBytes, generation.StreamBytes)
	}
	if after := docByteUpgradePGImage(test, fixture); after != image {
		test.Fatal("ready public view, find or reveal rewrote the archive, generation or queue")
	}
}

func TestPGLegacyDerivedPublicationReaderOnlyPeerPreservesV5WithoutRequeue(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document, _ := prepareLegacyDerivedPublicationPG(test, fixture)
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	completed := readDocByteUpgradePGJob(test, fixture, document.Hash)
	image := docByteUpgradePGImage(test, fixture)
	peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	peerFixture := fixture
	peerFixture.storage = peer
	peerFixture.service = NewService(peer, peer, nil, nil, peer)
	observed := observeLegacyDerivedPublicationPG(test, peerFixture, document.Hash)
	for _, mode := range []struct {
		name    string
		bytes   bool
		derived bool
	}{
		{name: "both producers disabled"},
		{name: "derived producer disabled", bytes: true},
		{name: "byte producer disabled", derived: true},
	} {
		test.Run(mode.name, func(test *testing.T) {
			observed.test = test
			peer.ConfigureLegacyByteRangePublication(mode.bytes)
			peer.ConfigureLegacyDerivedByteRangePublication(mode.derived)
			assertLegacyDerivedPublicationPGPublicReads(test, peerFixture, document, observed)
			if err := peerFixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
				test.Fatal("reader-only peer found unexpected pending work", err)
			}
			if current := readDocByteUpgradePGGeneration(test, fixture, document.Hash); current.Version != 5 || !reflect.DeepEqual(current, generation) {
				test.Fatal("disabled producer replaced the existing derived generation", current, generation)
			}
			if current := readDocByteUpgradePGJob(test, fixture, document.Hash); !reflect.DeepEqual(current, completed) {
				test.Fatal("reader-only peer requeued or rewrote completed v5 demand", current, completed)
			}
			if after := docByteUpgradePGImage(test, fixture); after != image {
				test.Fatal("reader-only peer changed source, generation identity or durable receipt")
			}
		})
	}
}

func TestPGLegacyDerivedPublicationSelectedReadFailuresExposeNoBytes(test *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{name: "missing selected stage", err: domain.ErrNotFound},
		{name: "invalid selected stage", err: domain.ErrIntegrity},
		{name: "stale selected binding", err: domain.ErrConflict},
	} {
		test.Run(failure.name, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document, observed := prepareLegacyDerivedPublicationPG(test, fixture)
			image := docByteUpgradePGImage(test, fixture)
			observed.failure = failure.err
			observed.resetReads()
			offset := domain.ConversationManifestChunkBytes - 32
			fragment, err := fixture.service.ReadDocViewFragment(fixture.ctx, fixture.repo, document.Hash, 0, offset, 64)
			if !errors.Is(err, failure.err) || fragment != (domain.DocViewFragment{}) || observed.derivedBytes != domain.ConversationManifestChunkBytes {
				test.Fatal("second selected stage failure leaked the first stage's bytes", fragment, observed.derivedBytes, err)
			}
			assertLegacyDerivedPublicationPGReads(test, observed, []int{0, 1})
			observed.resetReads()
			page, err := fixture.service.ReadDocViewEvents(fixture.ctx, fixture.repo, document.Hash, "", 1, 2, "chat")
			if !errors.Is(err, failure.err) || len(page.Events) != 0 || observed.derivedBytes != 0 {
				test.Fatal("failed selected body returned archival content", page, err)
			}
			assertLegacyDerivedPublicationPGReads(test, observed, []int{1})
			observed.resetReads()
			found, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{
				Legacy: true, Query: "needle🙂", From: 1, To: 3, Total: len(document.CIR.Events),
			})
			if !errors.Is(err, failure.err) || !reflect.DeepEqual(found, domain.DocFindPage{}) || observed.derivedBytes != 0 {
				test.Fatal("failed selected find returned matches or fallback bytes", found, err)
			}
			assertLegacyDerivedPublicationPGReads(test, observed, []int{1})
			observed.resetReads()
			revealed, err := fixture.service.ReadDocFindFragment(fixture.ctx, fixture.repo, document.Hash, domain.DocFindFragmentRequest{
				Legacy: true, Index: 1, Total: len(document.CIR.Events), Budget: 64,
			})
			if !errors.Is(err, failure.err) || revealed != (domain.DocFindFragmentPage{}) || observed.derivedBytes != 0 {
				test.Fatal("failed selected reveal returned a fragment or fallback bytes", revealed, err)
			}
			assertLegacyDerivedPublicationPGReads(test, observed, []int{1})
			if after := docByteUpgradePGImage(test, fixture); after != image {
				test.Fatal("selected read failure changed the original, generation or completed receipt")
			}
		})
	}
}
