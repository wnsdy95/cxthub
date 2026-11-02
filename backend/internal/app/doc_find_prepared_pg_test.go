//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docPreparedFindReadProbe struct {
	outbound.DocDisplayReader
	missing         func()
	pages, versions int
}

func (probe *docPreparedFindReadProbe) ReadDocDisplayGeneration(ctx context.Context, target domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	probe.versions++
	generation, err := probe.DocDisplayReader.ReadDocDisplayGeneration(ctx, target)
	if errors.Is(err, domain.ErrNotFound) && probe.missing != nil {
		probe.missing()
	}
	return generation, err
}

func (probe *docPreparedFindReadProbe) ReadDocDisplayPage(ctx context.Context, target domain.DocDisplayTarget, index int) (domain.DocDisplayPage, error) {
	probe.pages++
	return probe.DocDisplayReader.ReadDocDisplayPage(ctx, target, index)
}

func TestPGDocPreparedFindQueuesThenSearchesAcrossPagesAndOldLimit(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "root"
		if legacy {
			name = "legacy"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newDocDisplayPGFixture(test)
			fixture.service.ConfigureDocumentDisplay(fixture.storage)
			output := strings.Repeat("a", domain.DocDisplayPageTextBytes-2) + "NEEDLE🙂" + strings.Repeat("x", 700_000) + "NEEDLE🙂" + string(fixture.repo)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
				historyMessage(domain.RoleUser, "first needle🙂"),
				{Kind: domain.EventToolResult, Seq: 1, Output: output},
				historyMessage(domain.RoleAssistant, "finished"),
			}}
			for index := range document.Events {
				document.Events[index].Seq = index
			}
			var hash domain.ContentHash
			request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Query: "needle🙂", From: 0, To: 3, Legacy: legacy}
			clientSource := map[string]any{"id": name, "representation": domain.DocFindPreparedRepresentation, "from": 0, "to": 3}
			if legacy {
				hash = seedLegacyFindPGDocument(test, fixture, document.Events).Hash
				request.Total = 3
				clientSource["protocol"] = "legacy"
				clientSource["context"] = map[string]any{"hash": hash, "base": "", "view": "all", "start": 0, "total": 3, "inherited": 0}
			} else {
				hash = seedDocViewBoundaryPG(test, fixture, document)
				visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
				request.Selection = visible.ReplayCursor
				clientSource["replay"] = visible.ReplayCursor
				clientSource["context"] = map[string]any{"hash": hash, "base": "", "view": "all", "start": 0, "total": 3, "inherited": 0, "generation": visible.Generation, "rules": 1}
			}
			clientSource["selection"] = map[string]any{"repoId": fixture.repo, "hash": hash, "base": "", "mode": "all", "start": 0}
			var clientPages []domain.DocFindPage
			var matches []domain.DocFindMatch
			queued := false
			pages := 0
			for attempt := 0; attempt < 150; attempt++ {
				page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
				if errors.Is(err, domain.ErrDocRangeIndexPending) {
					if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
						test.Fatal(err)
					}
					continue
				}
				if errors.Is(err, domain.ErrDocDisplayPending) {
					queued = true
					if page.Matches != nil || page.Prepared != nil || page.NextCursor != nil {
						test.Fatal("pending preparation leaked a partial page", page)
					}
					if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
						test.Fatal(err)
					}
					continue
				}
				if err != nil || page.Prepared == nil || len(page.Matches) > domain.DocFindPageMatches {
					test.Fatal(page, err)
				}
				wantProjection := domain.DocFindPreparedProjection
				if legacy {
					wantProjection = domain.DocLegacyFindPreparedProjection
				}
				if page.Projection != wantProjection {
					test.Fatal("prepared representation was silently downgraded", page.Projection)
				}
				pages++
				clientPages = append(clientPages, page)
				for _, match := range page.Matches {
					if match.PreparedPage == nil || match.SourceRange == nil || match.Raw || match.Field != 0 {
						test.Fatal("prepared match lost source semantics", match)
					}
					matches = append(matches, match)
				}
				if page.NextCursor == nil {
					if page.Index != 3 || page.Prepared.Page != 0 || page.Prepared.Generation != "" || page.Byte != 0 {
						test.Fatal("terminal progress was not reset", page)
					}
					break
				}
				request = domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Legacy: legacy, Cursor: *page.NextCursor}
			}
			if !queued || len(matches) != 3 || matches[0].Index != 0 || matches[1].Index != 1 || matches[2].Index != 1 || matches[1].Offset != domain.DocDisplayPageTextBytes-2 || *matches[1].PreparedPage != 1 || matches[2].Offset <= domain.MaxDocFindDisplayBytes {
				test.Fatal("lost/duplicated boundary match or large offsets", queued, pages, matches)
			}
			target := docDisplayTargetPG(test, fixture, hash, 1)
			generation, err := readDocDisplayGenerationPG(fixture, target)
			if err != nil || generation.TextBytes != len(output) {
				test.Fatal(generation, err)
			}
			test.Logf("legacy=%t request_pages=%d prepared_pages=%d output_bytes=%d", legacy, pages, generation.Pages.Count, len(output))
			if directory := os.Getenv("CXT_TEST_PREPARED_FIND_FIXTURES"); directory != "" {
				raw, err := json.Marshal(map[string]any{"source": clientSource, "query": "needle🙂", "pages": clientPages})
				if err != nil {
					test.Fatal(err)
				}
				if err := os.MkdirAll(directory, 0700); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, name+".json"), raw, 0600); err != nil {
					test.Fatal(err)
				}
			}
		})
	}
}

func TestPGDocPreparedFindReauthorizesBeforeQueueAndReadOnlyNeverWrites(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: string(fixture.repo) + strings.Repeat("value ", 40_000)}}})
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Selection: visible.ReplayCursor, Query: "value", To: 1}
	count := func() int {
		var jobs int
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&jobs); err != nil {
			test.Fatal(err)
		}
		return jobs
	}
	if _, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); !errors.Is(err, domain.ErrDocFindDisplayUnavailable) || count() != 0 {
		test.Fatal("read-only request created work", err)
	}
	checks, missing := 0, false
	fixture.service.ConfigureDocumentDisplayReads(&docPreparedFindReadProbe{DocDisplayReader: fixture.storage, missing: func() { missing = true }})
	authorize := func(read context.Context, _ domain.ContentHash) (context.Context, error) {
		checks++
		if missing {
			return read, domain.ErrForbidden
		}
		return read, nil
	}
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, authorize)
	ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, authorize)
	if _, err := fixture.service.FindDocPage(ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrForbidden) || count() != 0 || checks < 2 || !missing {
		test.Fatal("queue used stale read authority", err, checks)
	}
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocDisplayPending) || count() != 1 {
		test.Fatal("authorized request was not durably queued", err)
	}
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocDisplayPending) || count() != 1 {
		test.Fatal("repeated demand duplicated work", err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
			test.Fatal(err)
		}
		if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); err == nil {
			if page, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request); err != nil || page.Prepared == nil || len(page.Matches) == 0 || count() != 1 {
				test.Fatal("prepared read-only caller could not reuse ready pages", page, err)
			}
			return
		} else if !errors.Is(err, domain.ErrDocDisplayPending) {
			test.Fatal(err)
		}
	}
	test.Fatal("demand did not complete")
}

func TestPGDocPreparedFindWarmCostIndependentOfOutputLength(test *testing.T) {
	for _, size := range []int{1 << 20, 10 << 20} {
		fixture := newDocDisplayPGFixture(test)
		fixture.service.ConfigureDocumentDisplay(fixture.storage)
		hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: string(fixture.repo) + strings.Repeat("x", size)}}})
		visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
		target := docDisplayTargetPG(test, fixture, hash, 0)
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
			test.Fatal(err)
		}
		ready := false
		for attempt := 0; attempt < 100; attempt++ {
			if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
				test.Fatal(err)
			}
			if _, err := readDocDisplayGenerationPG(fixture, target); err == nil {
				ready = true
				break
			} else if !errors.Is(err, domain.ErrNotFound) {
				test.Fatal(err)
			}
		}
		if !ready {
			test.Fatal("fixture did not complete preparation")
		}
		observed := &visibleCostStore{docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}}
		fixture.service.blobs = observed
		probe := &docPreparedFindReadProbe{DocDisplayReader: fixture.storage}
		fixture.service.ConfigureDocumentDisplayReads(probe)
		page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Selection: visible.ReplayCursor, Query: "absent", To: 1})
		if err != nil || len(page.Matches) != 0 || page.NextCursor == nil || page.Prepared == nil || page.Prepared.Page <= 0 || page.Prepared.Page > docFindPreparedWindows || probe.pages > 2*(docFindPreparedWindows+1) || probe.versions > docFindPreparedWindows+1 || observed.chunkReads != 0 {
			test.Fatal("warm search replayed whole source or lost bounded progress", size, page, err, probe, observed)
		}
		test.Logf("source_text_bytes=%d selected_projection_pages=%d descriptors=%d original_chunk_reads=%d next_page=%d", size, probe.pages, probe.versions, observed.chunkReads, page.Prepared.Page)
	}
}

func TestPGDocPreparedFindPREOnlyUnsupportedDoesNotRetryForever(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	fixture.service.ConfigureDocumentDisplayReads(&docPreparedFindReadProbe{DocDisplayReader: fixture.storage})
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: map[string]any{"opaque": string(fixture.repo) + strings.Repeat("value", 40_000)}}}})
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Selection: visible.ReplayCursor, Query: "value", To: 1}
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocDisplayPending) {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
		test.Fatal(err)
	}
	if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
		test.Fatal("unsupported representation remained retryable", err)
	}
	job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
	if !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("unsupported demand was requeued", job, err)
	}
}

func TestPGDocPreparedFindRejectsForgedContinuationAndCorruptPage(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: string(fixture.repo) + strings.Repeat("x", 1<<20)}}})
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
			test.Fatal(err)
		}
		if _, err := readDocDisplayGenerationPG(fixture, target); err == nil {
			break
		}
	}
	request := domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Selection: visible.ReplayCursor, Query: "absent", To: 1}
	readOnly := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
	readOnly.ConfigureDocumentDisplayReads(fixture.storage)
	page, err := readOnly.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
	if err != nil || page.NextCursor == nil {
		test.Fatal("reader-only composition could not serve prepared result", page, err)
	}
	selector, err := domain.DecodeDocFindSelector(*page.NextCursor)
	if err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*domain.DocFindSelector){
		func(selector *domain.DocFindSelector) {
			selector.Prepared.Generation = domain.HashContent([]byte("foreign descriptor"))
		},
		func(selector *domain.DocFindSelector) { selector.Prepared.Page = domain.MaxDocRangePages - 1 },
		func(selector *domain.DocFindSelector) { selector.Byte = domain.MaxConversationDocumentBytes },
	} {
		changed := selector
		progress := *selector.Prepared
		changed.Prepared = &progress
		mutate(&changed)
		cursor, err := changed.Encode()
		if err != nil {
			test.Fatal(err)
		}
		result, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Cursor: cursor})
		if err == nil || errors.Is(err, domain.ErrDocDisplayPending) || result.Matches != nil || result.NextCursor != nil {
			test.Fatal("forged current descriptor/position was accepted or queued", result, err)
		}
	}
	generation, err := readDocDisplayGenerationPG(fixture, target)
	if err != nil {
		test.Fatal(err)
	}
	sourceHash, err := generation.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	pageHash, err := domain.ResolveDocRangeLeaf(fixture.ctx, generation.Pages, selector.Prepared.Page, func(read context.Context, hash domain.ContentHash) ([]byte, error) {
		var raw []byte
		err := fixture.pool.QueryRow(read, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, sourceHash, hash).Scan(&raw)
		return raw, err
	})
	if err != nil {
		test.Fatal(err)
	}
	var original []byte
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, sourceHash, pageHash).Scan(&original); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := fixture.pool.Exec(context.Background(), `WITH replaced AS (DELETE FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2 RETURNING source_hash,hash)
 INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) SELECT source_hash,hash,$3 FROM replaced`, sourceHash, pageHash, original); err != nil {
			test.Error(err)
		}
	})
	if _, err := fixture.pool.Exec(fixture.ctx, `WITH replaced AS (DELETE FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2 RETURNING source_hash,hash)
 INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) SELECT source_hash,hash,$3 FROM replaced`, sourceHash, pageHash, []byte("corrupt")); err != nil {
		test.Fatal(err)
	}
	result, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{Representation: request.Representation, Query: request.Query, Cursor: *page.NextCursor})
	if !errors.Is(err, domain.ErrIntegrity) || result.Matches != nil || result.NextCursor != nil {
		test.Fatal("corrupt prepared page used source fallback or partial success", result, err)
	}
}
