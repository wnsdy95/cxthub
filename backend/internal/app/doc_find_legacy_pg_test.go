//go:build postgres

package app

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func seedLegacyFindPGDocument(test *testing.T, fixture docViewPGFixture, events []domain.CIREvent) domain.SessionDoc {
	test.Helper()
	document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: append([]domain.CIREvent(nil), events...)}}
	for index := range document.CIR.Events {
		document.CIR.Events[index].Seq = index
	}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	if _, err := fixture.storage.PutDoc(fixture.ctx, fixture.repo, document); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{ID: document.Hash, RepoID: fixture.repo, DocHash: document.Hash,
		DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	return document
}

func legacyFindPGImage(test *testing.T, fixture docViewPGFixture) string {
	test.Helper()
	var image string
	err := fixture.pool.QueryRow(fixture.ctx, `WITH owned_documents AS (
 SELECT hash FROM repo_blobs WHERE repo_id=$1 AND kind='doc'
), owned_blocks AS (
 SELECT DISTINCT block_hash FROM doc_read_block_locations_v3 WHERE doc_hash IN (SELECT hash FROM owned_documents)
)
SELECT jsonb_build_object(
 'objects',(SELECT jsonb_agg(jsonb_build_array(owned.kind,owned.hash,encode(blob.bytes,'hex'),owned.xmin::text,blob.xmin::text) ORDER BY owned.kind,owned.hash) FROM repo_blobs owned JOIN blobs blob ON blob.hash=owned.hash WHERE owned.repo_id=$1),
 'snapshots',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) ORDER BY snapshot.id) FROM snapshots snapshot WHERE snapshot.repo_id=$1),
 'legacy_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_read_indexes header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'v2_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_read_indexes_v2 header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'v3_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_read_indexes_v3 header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'locations',(SELECT jsonb_agg(to_jsonb(location) ORDER BY location.doc_hash,location.ordinal) FROM doc_read_event_locations_current location WHERE location.doc_hash IN (SELECT hash FROM owned_documents)),
 'blocks',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(block),block.xmin::text) ORDER BY block.hash) FROM doc_read_blocks_v3 block WHERE block.hash IN (SELECT block_hash FROM owned_blocks)),
 'block_events',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(event),event.xmin::text) ORDER BY event.block_hash,event.ordinal) FROM doc_read_block_events_v3 event WHERE event.block_hash IN (SELECT block_hash FROM owned_blocks)),
 'search',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(event),event.xmin::text) ORDER BY event.hash) FROM doc_search_events_v2 event WHERE event.hash IN (SELECT event_hash FROM doc_read_event_locations_current WHERE doc_hash IN (SELECT hash FROM owned_documents))),
 'range_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_range_indexes_v1 header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'range_generations',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(generation),generation.xmin::text) ORDER BY generation.doc_hash) FROM doc_range_generations_v2 generation WHERE generation.doc_hash IN (SELECT hash FROM owned_documents)),
 'visible_generations',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(generation),generation.xmin::text) ORDER BY generation.doc_hash) FROM doc_visible_generations_v1 generation WHERE generation.doc_hash IN (SELECT hash FROM owned_documents)),
 'boundaries',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(boundary),boundary.xmin::text) ORDER BY boundary.doc_hash,boundary.base_hash) FROM doc_range_boundaries_v1 boundary WHERE boundary.repo_id=$1),
 'range_jobs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY job.doc_hash,job.base_hash) FROM doc_range_jobs_v1 job WHERE job.repo_id=$1),
 'find_jobs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY job.doc_hash) FROM doc_find_fields_jobs_v1 job WHERE job.repo_id=$1),
 'finalization_jobs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY job.id) FROM doc_finalization_jobs job WHERE job.repo_id=$1)
)::text`, fixture.repo).Scan(&image)
	if err != nil {
		test.Fatal(err)
	}
	return image
}

func TestPGDocFindLegacyPreservesIdentityAndRechecksOwnership(test *testing.T) {
	for _, removed := range []string{"source", "base"} {
		test.Run(removed, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			shared := historyMessage(domain.RoleUser, "shared needle")
			base := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{shared})
			document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{shared, historyMessage(domain.RoleAssistant, strings.Repeat("Needle🙂 ", 45))})
			before := legacyFindPGImage(test, fixture)
			initial := domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base.Hash, Total: 2, Inherited: 1, From: 1, To: 2}
			first, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, initial)
			if err != nil || len(first.Matches) != domain.DocFindPageMatches || first.NextCursor == nil {
				test.Fatal("legacy find did not produce a bounded continuation", first, err)
			}
			continuation := domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *first.NextCursor}
			selector, err := domain.DecodeDocLegacyFindSelector(continuation.Cursor)
			if err != nil || selector.Repo != fixture.repo || selector.Hash != document.Hash || selector.Base != base.Hash ||
				selector.Total != 2 || selector.Inherited != 1 || selector.From != 1 || selector.To != 2 || selector.Index != 1 {
				test.Fatal("legacy cursor lost its original source or prefix", selector, err)
			}
			matches := append([]domain.DocFindMatch(nil), first.Matches...)
			request, page := continuation, first
			for attempt := 0; attempt < 4; attempt++ {
				page, err = fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, request)
				if err != nil || page.Projection != domain.DocLegacyFindProjection || page.Hash != document.Hash || page.Generation != "" {
					test.Fatal("legacy continuation changed identity", page, err)
				}
				matches = append(matches, page.Matches...)
				if page.NextCursor == nil {
					break
				}
				if *page.NextCursor == request.Cursor {
					test.Fatal("legacy continuation did not advance")
				}
				request.Cursor = *page.NextCursor
			}
			if len(matches) != 45 || page.NextCursor != nil || first.Projection != domain.DocLegacyFindProjection || first.Hash != document.Hash || first.Generation != "" {
				test.Fatal("legacy find lost matches or invented a root generation", first, page, len(matches))
			}
			for index, match := range matches {
				if match.Index != 1 || match.Offset != index*len("Needle🙂 ") || match.Raw || match.RevealCursor != "" {
					test.Fatal("legacy match escaped its original event coordinates", match)
				}
			}
			current, err := fixture.storage.GetDoc(fixture.ctx, fixture.repo, document.Hash)
			if err != nil || current.DocumentRef() != document.DocumentRef() {
				test.Fatal("legacy find rewrote the archived document", current.DocumentRef(), err)
			}
			canonical, err := domain.CanonicalBytes(current.CIR)
			if err != nil || domain.HashContent(canonical) != document.Hash {
				test.Fatal("legacy find changed canonical archive bytes", err)
			}
			if after := legacyFindPGImage(test, fixture); before != after {
				test.Fatal("legacy find changed persisted documents or projections")
			}
			for _, request := range []domain.DocFindPageRequest{initial, continuation} {
				calls := 0
				denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
					calls++
					return bound, domain.ErrForbidden
				})
				out, err := fixture.service.FindDocPage(denied, fixture.repo, document.Hash, request)
				if !errors.Is(err, domain.ErrForbidden) || calls != 1 || !reflect.DeepEqual(out, domain.DocFindPage{}) {
					test.Fatal("legacy find bypassed fresh authorization", out, err, calls)
				}
			}
			revoked := document.Hash
			if removed == "base" {
				revoked = base.Hash
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, revoked); err != nil {
				test.Fatal(err)
			}
			for _, request := range []domain.DocFindPageRequest{initial, continuation} {
				out, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, request)
				if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(out, domain.DocFindPage{}) {
					test.Fatal("warm legacy find bypassed ownership removal", removed, out, err)
				}
			}
		})
	}
}

func TestPGDocFindMixedIdentitiesPreservePrefixAndProjectionFence(test *testing.T) {
	for _, sourceIdentity := range []string{"legacy", "root"} {
		test.Run(sourceIdentity, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
				historyMessage(domain.RoleUser, "shared needle"),
				historyMessage(domain.RoleAssistant, "shared answer"),
				historyMessage(domain.RoleUser, "new needle"),
				historyMessage(domain.RoleAssistant, "later shared needle"),
			}}
			for index := range document.Events {
				document.Events[index].Seq = index
			}
			previous := domain.CIRDocument{Envelope: document.Envelope, Events: append([]domain.CIREvent(nil), document.Events...)}
			previous.Events[2] = historyMessage(domain.RoleUser, "different base needle")
			previous.Events[2].Seq = 2
			var hash, base domain.ContentHash
			if sourceIdentity == "legacy" {
				hash = seedLegacyFindPGDocument(test, fixture, document.Events).Hash
				base = seedDocViewBoundaryPG(test, fixture, previous)
			} else {
				hash = seedDocViewBoundaryPG(test, fixture, document)
				base = seedLegacyFindPGDocument(test, fixture, previous.Events).Hash
			}
			boundary := readPreparedDocViewReadyPG(test, fixture, hash, base)
			identity := domain.DocumentIdentityLegacy
			if sourceIdentity == "root" {
				identity = domain.DocumentIdentityRootV1
			}
			if boundary.DocIdentity != identity || boundary.Inherited != 2 || boundary.Offset != 2 || boundary.Total != 4 || len(boundary.Events) != 2 {
				test.Fatal("mixed identities did not use the exact contiguous prefix", boundary)
			}
			legacyRequest := domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base, Total: 4, Inherited: 2, From: 2, To: 4}
			if sourceIdentity == "legacy" {
				page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, legacyRequest)
				if err != nil || page.Projection != domain.DocLegacyFindProjection || page.Hash != hash || page.Generation != "" ||
					page.NextCursor != nil || len(page.Matches) != 2 || page.Matches[0].Index != 2 || page.Matches[1].Index != 3 {
					test.Fatal("legacy find did not preserve the root-base prefix", page, err)
				}
				legacyRequest.Inherited = 3
				page, err = fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, legacyRequest)
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
					test.Fatal("legacy find trusted a declared mixed prefix", page, err)
				}
				return
			}
			visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "chat", Base: base, Offset: -1, Limit: 20})
			page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{
				Selection: visible.ReplayCursor, Query: "needle", From: boundary.Inherited, To: boundary.Total})
			if err != nil || page.Projection != domain.DocFindProjection || page.Hash != hash || page.Generation != visible.Generation ||
				page.NextCursor != nil || len(page.Matches) != 2 || page.Matches[0].Index != 2 || page.Matches[1].Index != 3 {
				test.Fatal("root find lost the event range selected by its legacy boundary", page, err)
			}
			page, err = fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, legacyRequest)
			if !errors.Is(err, domain.ErrDocVisibleProjectionUnsupported) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
				test.Fatal("legacy find downgraded a root identity", page, err)
			}
			selection, err := domain.DecodeDocVisibleSelector(visible.ReplayCursor)
			if err != nil {
				test.Fatal(err)
			}
			selection.Base = ""
			selection.Inherited = 0
			mixedSelection, err := selection.Encode()
			if err != nil {
				test.Fatal(err)
			}
			page, err = fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{
				Selection: mixedSelection, Query: "needle", From: 2, To: 4})
			if !errors.Is(err, domain.ErrDocVisibleGenerationStale) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
				test.Fatal("root find accepted a legacy-base generation with its base removed", page, err)
			}
		})
	}
}

func TestPGDocFindLegacyMissingIndexReadOnlyCompatibilityDoesNotWrite(test *testing.T) {
	for _, representation := range []string{"chunked", "monolithic"} {
		test.Run(representation, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			shared := historyMessage(domain.RoleUser, "shared needle")
			base := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{shared})
			document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
				shared,
				historyMessage(domain.RoleUser, strings.Repeat("needle ", 45)),
				{Kind: domain.EventReasoning, RedactedSummary: "visible needle", Locked: &domain.LockedBlob{
					Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: "private needle"}},
			})
			initial := domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base.Hash, Total: 3, Inherited: 1, From: 1, To: 3}
			first, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, initial)
			if err != nil || first.NextCursor == nil || len(first.Matches) != domain.DocFindPageMatches {
				test.Fatal("indexed compatibility baseline failed", first, err)
			}
			continuation := domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *first.NextCursor}
			second, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, continuation)
			if err != nil || second.NextCursor == nil {
				test.Fatal("indexed compatibility continuation failed", second, err)
			}
			terminal := domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *second.NextCursor}
			third, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, terminal)
			if err != nil || third.NextCursor != nil || len(third.Matches) != 6 || third.Matches[5].Index != 2 {
				test.Fatal("indexed compatibility terminal page searched hidden state or lost public text", third, err)
			}
			if representation == "monolithic" {
				for _, archived := range []domain.SessionDoc{document, base} {
					canonical, err := domain.CanonicalBytes(archived.CIR)
					if err != nil {
						test.Fatal(err)
					}
					if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, archived.Hash, canonical); err != nil {
						test.Fatal(err)
					}
				}
			}
			hashes := []domain.ContentHash{document.Hash, base.Hash}
			if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM doc_read_indexes_v3 WHERE hash=ANY($1::text[])`, hashes); err != nil {
				test.Fatal(err)
			}
			var remaining int
			if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_read_index_current WHERE hash=ANY($1::text[])`, hashes).Scan(&remaining); err != nil || remaining != 0 {
				test.Fatal("fixture retained a readable legacy index", remaining, err)
			}
			before := legacyFindPGImage(test, fixture)
			read := func(read context.Context) error {
				for index, request := range []domain.DocFindPageRequest{initial, continuation, terminal} {
					page, err := fixture.service.FindDocPage(read, fixture.repo, document.Hash, request)
					want := []domain.DocFindPage{first, second, third}[index]
					if err != nil || !reflect.DeepEqual(page, want) {
						test.Fatal("missing-index compatibility changed a legacy find page", representation, page, want, err)
					}
				}
				return nil
			}
			if err := read(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if err := read(outbound.WithDocReadOnly(fixture.ctx)); err != nil {
				test.Fatal(err)
			}
			if err := fixture.storage.WithinReadSnapshot(outbound.WithDocReadOnly(fixture.ctx), read); err != nil {
				test.Fatal("compatibility find required a writable transaction", err)
			}
			if after := legacyFindPGImage(test, fixture); before != after {
				test.Fatal("missing-index legacy find published rows or rewrote its archive")
			}
		})
	}
}

func TestPGDocFindLegacySelectedByteCorruptionFailsClosed(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{historyMessage(domain.RoleUser, strings.Repeat("needle ", 45)+string(fixture.repo))})
	initial := domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1}
	first, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, initial)
	if err != nil || first.NextCursor == nil || len(first.Matches) != domain.DocFindPageMatches {
		test.Fatal("corruption fixture did not warm indexed find", first, err)
	}
	manifest, err := fixture.storage.GetDocManifest(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash)
	if err != nil || manifest.Format != domain.ChunkFormatV2 || len(manifest.Chunks) != 1 {
		test.Fatal("corruption fixture requires a selected legacy chunk", manifest, err)
	}
	chunk, err := fixture.storage.GetChunk(fixture.ctx, fixture.repo, manifest.Chunks[0])
	if err != nil {
		test.Fatal(err)
	}
	damaged := bytes.Replace(chunk, []byte("needle"), []byte("broken"), 1)
	if bytes.Equal(chunk, damaged) || len(chunk) != len(damaged) {
		test.Fatal("corruption fixture did not change selected bytes in place")
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, manifest.Chunks[0], damaged); err != nil {
		test.Fatal(err)
	}
	for _, request := range []domain.DocFindPageRequest{initial, {Legacy: true, Query: "needle", Cursor: *first.NextCursor},
		{Legacy: true, Query: "absent", Total: 1, To: 1}} {
		page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, document.Hash, request)
		if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
			test.Fatal("warm legacy find exposed corrupt selected bytes", page, err)
		}
	}
}
