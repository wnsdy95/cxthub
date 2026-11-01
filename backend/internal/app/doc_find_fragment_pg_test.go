//go:build postgres

package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docFindFragmentPGStore struct {
	*docVisibleLegacyBasePGStore
	chunkReads int
	chunkBytes int
}

func (storage *docFindFragmentPGStore) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	storage.chunkReads++
	storage.chunkBytes += int(length)
	return storage.PostgresStore.ReadConversationChunk(ctx, repo, hash, length)
}

func (storage *docFindFragmentPGStore) ReadVerifiedDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	storage.test.Error("fragment read materialized a complete verified Root")
	return domain.VerifiedSessionDoc{}, domain.ErrIntegrity
}

func docFindFragmentRevealPG(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash, page domain.DocVisiblePage) string {
	test.Helper()
	found, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{
		Selection: page.ReplayCursor, Query: "needle", From: page.Start, To: page.Total})
	if err != nil || len(found.Matches) == 0 || found.Generation != page.Generation {
		test.Fatal("genuine find reveal unavailable", found, err)
	}
	return found.Matches[0].RevealCursor
}

func TestPGDocFindFragmentRootAndLegacyBaseReadOnlyBoundedRanges(test *testing.T) {
	for _, legacyBase := range []bool{false, true} {
		fixture := newDocViewPGFixture(test)
		document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
			historyMessage(domain.RoleUser, "shared user"), historyMessage(domain.RoleAssistant, "shared assistant"),
			{Kind: domain.EventToolResult, Output: strings.Repeat("한🙂needle ", 80_000)},
		}}
		for index := range document.Events {
			document.Events[index].Seq = index
		}
		var base domain.ContentHash
		if legacyBase {
			base = seedLegacyFindPGDocument(test, fixture, document.Events[:2]).Hash
		}
		hash := seedDocViewBoundaryPG(test, fixture, document)
		visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{Base: base, View: "chat", Offset: -1, Limit: 2})
		reveal := docFindFragmentRevealPG(test, fixture, hash, visible)
		selection, err := domain.DecodeDocVisibleSelector(reveal)
		if err != nil || selection.Start != 2 || selection.Next != 2 || selection.Limit != 1 || selection.View != "all" {
			test.Fatal("unexpected matched reveal shape", selection, err)
		}
		canonical, err := domain.CanonicalBytes(document)
		if err != nil {
			test.Fatal(err)
		}
		verified, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: document})
		if err != nil {
			test.Fatal(err)
		}
		index, err := verified.ReadIndex()
		if err != nil {
			test.Fatal(err)
		}
		entry := index.Events[2]
		want, err := verified.EventStreamRange(fixture.ctx, entry.Offset, entry.Length)
		if err != nil {
			test.Fatal(err)
		}
		observed := &docFindFragmentPGStore{docVisibleLegacyBasePGStore: &docVisibleLegacyBasePGStore{
			docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}, test: test, base: base}}
		fixture.service.blobs = observed
		before := legacyFindPGImage(test, fixture)
		request := domain.DocFindFragmentRequest{Selection: reveal, Budget: 65536}
		var reconstructed []byte
		for {
			pages, chunks := observed.pages, observed.chunkReads
			page, err := fixture.service.ReadDocFindFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
			if err != nil || page.Hash != hash || page.Projection != domain.DocFindFragmentProjection || page.Generation != visible.Generation || page.Index != 2 ||
				page.Offset != request.Offset || page.Total != len(want) || len(page.JSON) == 0 || len(page.JSON) > request.Budget || !utf8.ValidString(page.JSON) {
				test.Fatal("Root fragment lost source coordinates or generation", page, err)
			}
			if observed.pages-pages > 1 || observed.chunkReads-chunks > 4 || observed.fallbackPages != 0 {
				test.Fatal("fragment loaded unrelated range pages or archive bytes", observed)
			}
			if page.Offset == 0 && page.Previous != -1 || page.Offset > 0 && (page.Previous < max(0, page.Offset-request.Budget-3) || page.Previous >= page.Offset || !utf8.RuneStart(want[page.Previous])) {
				test.Fatal("unsafe previous pointer", page)
			}
			reconstructed = append(reconstructed, page.JSON...)
			if page.Next == -1 {
				break
			}
			if page.Next != page.Offset+len(page.JSON) {
				test.Fatal("forward continuation changed byte coordinates", page)
			}
			request.Offset = page.Next
		}
		if !bytes.Equal(reconstructed, want) || before != legacyFindPGImage(test, fixture) {
			test.Fatal("warm fragments rewrote source state or lost canonical bytes")
		}
		if legacyBase && observed.parentOpens == 0 {
			test.Fatal("fragment failed to reopen its actual legacy base generation")
		}
	}
}

func TestPGDocFindFragmentRechecksSelectedGenerationAuthorityAndOwnership(test *testing.T) {
	for _, changed := range []string{"root", "root_base", "legacy_base"} {
		test.Run(changed, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
				historyMessage(domain.RoleUser, "shared"),
				{Kind: domain.EventToolResult, Seq: 1, Output: strings.Repeat("needle 한🙂 ", 100)},
			}}
			var base domain.ContentHash
			if changed == "legacy_base" {
				base = seedLegacyFindPGDocument(test, fixture, document.Events[:1]).Hash
			} else if changed == "root_base" {
				previous := document
				previous.Events = previous.Events[:1]
				base = seedDocViewBoundaryPG(test, fixture, previous)
			}
			hash := seedDocViewBoundaryPG(test, fixture, document)
			visibleRequest := domain.DocVisiblePageRequest{Base: base, View: "chat", Offset: -1, Limit: 2}
			visible := readVisibleReplayReadyPG(test, fixture, hash, visibleRequest)
			reveal := docFindFragmentRevealPG(test, fixture, hash, visible)
			request := domain.DocFindFragmentRequest{Selection: reveal, Budget: 64}
			first, err := fixture.service.ReadDocFindFragment(fixture.ctx, fixture.repo, hash, request)
			if err != nil || first.Next < 1 {
				test.Fatal("first fragment unavailable", first, err)
			}
			requests := []domain.DocFindFragmentRequest{request, {Selection: reveal, Budget: 64, Offset: first.Next}}
			for _, deniedRequest := range requests {
				calls := 0
				denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
					calls++
					return bound, domain.ErrForbidden
				})
				page, err := fixture.service.ReadDocFindFragment(denied, fixture.repo, hash, deniedRequest)
				if !errors.Is(err, domain.ErrForbidden) || calls != 1 || page != (domain.DocFindFragmentPage{}) {
					test.Fatal("fragment reused stale permission", page, err, calls)
				}
			}
			updated := hash
			if base != "" {
				updated = base
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, updated); err != nil {
				test.Fatal(err)
			}
			if changed == "legacy_base" {
				readDocVisibleLegacyBaseReadyPG(test, fixture, hash, visibleRequest)
			} else {
				if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, updated); err != nil {
					test.Fatal(err)
				}
				readVisibleReplayReadyPG(test, fixture, hash, visibleRequest)
			}
			for _, staleRequest := range requests {
				page, err := fixture.service.ReadDocFindFragment(fixture.ctx, fixture.repo, hash, staleRequest)
				if !errors.Is(err, domain.ErrDocVisibleGenerationStale) || page != (domain.DocFindFragmentPage{}) {
					test.Fatal("fragment mixed current bytes with a stale selected generation", page, err)
				}
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, fixture.repo, updated); err != nil {
				test.Fatal(err)
			}
			page, err := fixture.service.ReadDocFindFragment(fixture.ctx, fixture.repo, hash, request)
			if !errors.Is(err, domain.ErrNotFound) || page != (domain.DocFindFragmentPage{}) {
				test.Fatal("fragment ignored source/base ownership removal", page, err)
			}
		})
	}
}

type docFindLegacyFragmentPGStore struct {
	*store.PostgresStore
	wholeReads int
	chunkReads int
	chunkBytes int
	chunks     []domain.ContentHash
}

func (storage *docFindLegacyFragmentPGStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.wholeReads++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *docFindLegacyFragmentPGStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	storage.chunkReads++
	storage.chunks = append(storage.chunks, hash)
	raw, err := storage.PostgresStore.GetChunk(ctx, repo, hash)
	storage.chunkBytes += len(raw)
	return raw, err
}

func TestPGDocFindFragmentLegacyBoundedReadOnlyAndFreshSourceChecks(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	doc := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("unselected prefix ", 40_000)},
		{Kind: domain.EventToolResult, Output: strings.Repeat("한🙂needle ", 80_000) + string(fixture.repo)},
		{Kind: domain.EventToolResult, Output: strings.Repeat("unselected suffix ", 40_000)},
	})
	index, err := fixture.storage.DocReadIndex(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash)
	if err != nil {
		test.Fatal(err)
	}
	entry := index.Events[1]
	verified, err := domain.VerifySessionDoc(doc)
	if err != nil {
		test.Fatal(err)
	}
	want, err := verified.EventStreamRange(fixture.ctx, entry.Offset, entry.Length)
	if err != nil {
		test.Fatal(err)
	}
	manifest, err := fixture.storage.GetDocManifest(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash)
	if err != nil || manifest.Format != domain.ChunkFormatV2 || len(manifest.Chunks) < 4 {
		test.Fatal("legacy fragment fixture requires a multi-chunk owned source", manifest, err)
	}
	observed := &docFindLegacyFragmentPGStore{PostgresStore: fixture.storage}
	fixture.service.blobs = observed
	allowed, calls := true, 0
	authorized := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
		calls++
		if repo != fixture.repo || !allowed {
			return bound, domain.ErrForbidden
		}
		return inbound.WithDocumentSnapshotReadAuthorizer(bound, func(pinned context.Context, requested domain.ContentHash) (context.Context, error) {
			if !fixture.storage.InReadOnlyTransaction(pinned) {
				test.Fatal("fragment authority escaped the read-only snapshot")
			}
			if requested != fixture.repo || !allowed {
				return pinned, domain.ErrForbidden
			}
			return pinned, nil
		}), nil
	})
	request := domain.DocFindFragmentRequest{Legacy: true, Index: 1, Total: 3, Budget: 65536}
	before := legacyFindPGImage(test, fixture)
	for _, offset := range []int{0, 70_000} {
		for !utf8.RuneStart(want[offset]) {
			offset++
		}
		request.Offset = offset
		observed.chunkReads, observed.chunkBytes, observed.chunks = 0, 0, nil
		previousCalls := calls
		page, err := fixture.service.ReadDocFindFragment(authorized, fixture.repo, doc.Hash, request)
		if err != nil || page.Projection != domain.DocLegacyFindFragmentProjection || page.Generation != "" || page.Hash != doc.Hash || page.Index != 1 ||
			page.Offset != offset || page.Total != len(want) || len(page.JSON) == 0 || len(page.JSON) > request.Budget || !utf8.ValidString(page.JSON) ||
			!bytes.Equal([]byte(page.JSON), want[offset:offset+len(page.JSON)]) || page.Next != offset+len(page.JSON) || calls != previousCalls+1 {
			test.Fatal("legacy fragment changed canonical bytes, identity, or fresh permission", page, err, calls)
		}
		if offset == 0 && page.Previous != -1 || offset > 0 && (page.Previous < max(0, offset-request.Budget-3) || page.Previous >= offset || !utf8.RuneStart(want[page.Previous])) {
			test.Fatal("legacy fragment returned an unsafe previous pointer", page)
		}
		if observed.wholeReads != 0 || observed.chunkReads < 1 || observed.chunkReads > 4 || observed.chunkBytes > 4*domain.ChunkTarget {
			test.Fatal("legacy fragment materialized its oversized event or archive", observed)
		}
		selectedChunks := map[domain.ContentHash]bool{}
		for position := entry.Offset / domain.ChunkTarget; position <= (entry.Offset+entry.Length-1)/domain.ChunkTarget; position++ {
			selectedChunks[manifest.Chunks[position]] = true
		}
		for _, hash := range observed.chunks {
			if !selectedChunks[hash] {
				test.Fatal("legacy fragment fetched a chunk outside the selected event", hash)
			}
		}
	}
	if before != legacyFindPGImage(test, fixture) {
		test.Fatal("legacy fragment published projections or rewrote its source")
	}
	allowed = false
	previousCalls, previousChunks := calls, observed.chunkReads
	page, err := fixture.service.ReadDocFindFragment(authorized, fixture.repo, doc.Hash, request)
	if !errors.Is(err, domain.ErrForbidden) || page != (domain.DocFindFragmentPage{}) || calls != previousCalls+1 || observed.chunkReads != previousChunks {
		test.Fatal("warm legacy fragment reused revoked permission", page, err, calls)
	}
	allowed = true
	invalid := request
	invalid.Total++
	page, err = fixture.service.ReadDocFindFragment(authorized, fixture.repo, doc.Hash, invalid)
	if !errors.Is(err, domain.ErrIntegrity) || page != (domain.DocFindFragmentPage{}) || observed.chunkReads != previousChunks {
		test.Fatal("legacy fragment trusted the declared total", page, err)
	}
	if before != legacyFindPGImage(test, fixture) {
		test.Fatal("failed legacy authorization or count check wrote state")
	}
	chunkPosition := (entry.Offset + request.Offset) / domain.ChunkTarget
	chunkHash := manifest.Chunks[chunkPosition]
	chunk, err := fixture.storage.GetChunk(fixture.ctx, fixture.repo, chunkHash)
	if err != nil {
		test.Fatal(err)
	}
	damaged := bytes.Clone(chunk)
	damaged[entry.Offset+request.Offset-chunkPosition*domain.ChunkTarget] ^= 1
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunkHash, damaged); err != nil {
		test.Fatal(err)
	}
	corruptImage := legacyFindPGImage(test, fixture)
	page, err = fixture.service.ReadDocFindFragment(authorized, fixture.repo, doc.Hash, request)
	if !errors.Is(err, domain.ErrIntegrity) || page != (domain.DocFindFragmentPage{}) || observed.wholeReads != 0 {
		test.Fatal("warm legacy fragment exposed corrupt selected chunk bytes", page, err)
	}
	if corruptImage != legacyFindPGImage(test, fixture) {
		test.Fatal("legacy corruption rejection rewrote source state")
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunkHash, chunk); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, fixture.repo, doc.Hash); err != nil {
		test.Fatal(err)
	}
	revokedImage := legacyFindPGImage(test, fixture)
	page, err = fixture.service.ReadDocFindFragment(authorized, fixture.repo, doc.Hash, request)
	if !errors.Is(err, domain.ErrNotFound) || page != (domain.DocFindFragmentPage{}) || observed.wholeReads != 0 {
		test.Fatal("warm legacy fragment ignored ownership removal", page, err)
	}
	if revokedImage != legacyFindPGImage(test, fixture) {
		test.Fatal("legacy ownership rejection wrote state")
	}
}
