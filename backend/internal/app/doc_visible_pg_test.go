//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func visiblePGDocument(fixture docViewPGFixture, hidden, turns int) domain.CIRDocument {
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope}
	add := func(event domain.CIREvent) {
		event.Seq = len(document.Events)
		document.Events = append(document.Events, event)
	}
	add(historyMessage(domain.RoleUser, "[cxt] This session was resumed from a branch context seed. inherited"))
	for turn := 0; turn < turns; turn++ {
		add(historyMessage(domain.RoleUser, "real prompt"))
		for position := 0; position < hidden; position++ {
			if position%2 == 0 {
				add(domain.CIREvent{Kind: domain.EventReasoning})
			} else {
				add(domain.CIREvent{Kind: domain.EventToolResult, ToolName: "test", Output: json.RawMessage(`"tool detail"`)})
			}
		}
		add(historyMessage(domain.RoleAssistant, "answer"))
	}
	add(domain.CIREvent{Kind: domain.EventToolResult, ToolName: "tail", Output: json.RawMessage(`"unanswered"`)})
	return document
}

func readAllVisiblePG(t *testing.T, fixture docViewPGFixture, hash domain.ContentHash, request domain.DocVisiblePageRequest) []domain.DocVisiblePage {
	t.Helper()
	var pages []domain.DocVisiblePage
	for attempt := 0; attempt < 100; attempt++ {
		page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request)
		if err != nil {
			t.Fatal(err)
		}
		if len(pages) > 0 {
			previous := pages[len(pages)-1]
			if page.From != previous.To || page.Generation != previous.Generation || page.Base != previous.Base || page.Start != previous.Start || page.Inherited != previous.Inherited {
				t.Fatal("selection changed", previous, page)
			}
		}
		pages = append(pages, page)
		if page.NextCursor == nil {
			return pages
		}
		request = domain.DocVisiblePageRequest{Cursor: *page.NextCursor}
	}
	t.Fatal("paging never terminated")
	return nil
}

func TestPGDocVisiblePagesAndFoldsReconstructEveryOriginalOrdinal(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	document := visiblePGDocument(fixture, 139, 5)
	hash := seedDocViewBoundaryPG(t, fixture, document)
	for _, mode := range []string{"all", "chat", "prompts"} {
		for _, start := range []int{0, 2, 140, len(document.Events)} {
			limit := 2
			if mode == "all" {
				limit = 100
			}
			pages := readAllVisiblePG(t, fixture, hash, domain.DocVisiblePageRequest{View: mode, Offset: start, Limit: limit})
			seen := make(map[int]bool)
			mark := func(index int) {
				if index < start || index >= len(document.Events) || seen[index] {
					t.Fatal("lost/duplicate ordinal", mode, start, index)
				}
				seen[index] = true
			}
			for _, page := range pages {
				if page.Projection != domain.DocVisibleProjection || page.Rules != domain.DocVisibleAlgorithm || page.View != mode || page.Kind != "units" || page.Fold != nil || len(page.Events) != 0 {
					t.Fatal("wire discriminator changed", page)
				}
				expected := page.From
				for _, unit := range page.Units {
					if unit.From != expected {
						t.Fatal("unit coverage gap", unit, expected)
					}
					expected = unit.To
					if unit.Event != nil {
						mark(unit.Event.Index)
						if !unit.Event.Deferred && !reflect.DeepEqual(unit.Event.Event, document.Events[unit.Event.Index]) {
							t.Fatal("anchor changed", unit.Event.Index)
						}
						if mode == "prompts" && (unit.Event.SeedSummary || unit.Event.Event.Role != domain.RoleUser) {
							t.Fatal("nonprompt anchor", unit)
						}
					}
					if unit.Fold == nil {
						continue
					}
					token, next := unit.Fold.Cursor, unit.Fold.From
					rows := 0
					for attempt := 0; attempt < 10; attempt++ {
						fold, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Expand: token})
						if err != nil {
							t.Fatal(err)
						}
						if fold.Kind != "fold" || fold.Generation != page.Generation || fold.Start != start || fold.From != next || fold.Fold == nil || fold.Fold.From != unit.Fold.From || fold.Fold.To != unit.Fold.To || len(fold.Units) != 0 {
							t.Fatal("fold escaped selection", fold)
						}
						for _, event := range fold.Events {
							if event.Index != next {
								t.Fatal("fold gap", event.Index, next)
							}
							mark(event.Index)
							next++
							if !event.EmptyReasoning {
								rows++
							}
						}
						if fold.To != next {
							t.Fatal("fold endpoint mismatch")
						}
						if fold.NextCursor == nil {
							break
						}
						token = *fold.NextCursor
					}
					if next != unit.Fold.To || rows != unit.Fold.Rows {
						t.Fatal("fold row/range mismatch", mode, start, next, rows, unit.Fold)
					}
				}
				if expected != page.To {
					t.Fatal("page gap")
				}
			}
			if len(seen) != len(document.Events)-start {
				t.Fatal("lost ordinal", mode, start, len(seen))
			}
		}
	}
}

func TestPGDocVisibleContinuationRejectsStaleForgedAndRevokedSources(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	hash := seedDocViewBoundaryPG(t, fixture, visiblePGDocument(fixture, 12, 6))
	page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{View: "chat", Offset: -1, Limit: 2})
	if err != nil || page.NextCursor == nil {
		t.Fatal(page, err)
	}
	selector, err := domain.DecodeDocVisibleSelector(*page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, modify := range []func(*domain.DocVisibleSelector){
		func(value *domain.DocVisibleSelector) { value.Next++ },
		func(value *domain.DocVisibleSelector) { value.Inherited++ },
		func(value *domain.DocVisibleSelector) { value.Total++ },
		func(value *domain.DocVisibleSelector) { value.Repo = h('f') },
		func(value *domain.DocVisibleSelector) { value.Hash = h('e') },
		func(value *domain.DocVisibleSelector) { value.Purpose = "fold" },
	} {
		modified := selector
		modify(&modified)
		encoded, err := modified.Encode()
		if err != nil {
			continue
		}
		out, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: encoded})
		if !errors.Is(err, domain.ErrValidation) || out.Hash != "" {
			t.Fatal("inconsistent selector accepted", modified, out, err)
		}
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, hash); err != nil {
		t.Fatal(err)
	}
	if out, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: *page.NextCursor}); !errors.Is(err, domain.ErrDocRangeIndexPending) || out.Hash != "" {
		t.Fatal("stale generation was not prepared durably", out, err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		t.Fatal(err)
	}
	if out, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: *page.NextCursor}); !errors.Is(err, domain.ErrDocVisibleGenerationStale) || out.Hash != "" {
		t.Fatal("old source incarnation reused", out, err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, fixture.repo, hash); err != nil {
		t.Fatal(err)
	}
	if out, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: *page.NextCursor}); !errors.Is(err, domain.ErrNotFound) || out.Hash != "" {
		t.Fatal("ownership revocation bypassed", out, err)
	}
}

func TestPGDocVisiblePreparationBoundaryAndRestart(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	request := domain.DocVisiblePageRequest{View: "chat", Offset: -1, Limit: 2}
	if _, err := fixture.service.ReadDocVisiblePage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, fixture.hash, request); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		t.Fatal(err)
	}
	if _, err := fixture.storage.ClaimDocRangeJob(fixture.ctx, time.Now().UTC(), docJobLease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("read-only lookup queued work", err)
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, fixture.hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(fixture.ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	restarted := NewService(peer, peer, nil, nil, peer)
	if err := restarted.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, fixture.hash, request); err != nil {
		t.Fatal(err)
	}
	previous := fixture.cir
	previous.Events = previous.Events[:123]
	base := seedDocViewBoundaryPG(t, fixture, previous)
	request.Base = base
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, fixture.hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal("prefix scanned inline", err)
	}
	if err := restarted.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		t.Fatal(err)
	}
	pages := readAllVisiblePG(t, fixture, fixture.hash, request)
	if pages[0].From != 123 || pages[0].Inherited != 123 || pages[0].Base != base {
		t.Fatal("inherited selection lost", pages[0])
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, base); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, base); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, fixture.hash, domain.DocVisiblePageRequest{Cursor: *pages[0].NextCursor}); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal("changed base generation was reused", err)
	}
	if err := restarted.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, fixture.hash, domain.DocVisiblePageRequest{Cursor: *pages[0].NextCursor}); !errors.Is(err, domain.ErrDocVisibleGenerationStale) {
		t.Fatal("base incarnation was not bound", err)
	}
}

func TestPGDocVisibleBodyBudgetAndSelectedChunkIntegrity(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
		historyMessage(domain.RoleUser, strings.Repeat("<>&"+string(fixture.repo), docViewBytes/24)), historyMessage(domain.RoleAssistant, "small anchor "+string(fixture.repo)),
	}}
	hash := seedDocViewBoundaryPG(t, fixture, document)
	page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{View: "chat", Offset: 0})
	if err != nil || len(page.Units) != 2 || !page.Units[0].Event.Deferred || page.Units[1].Event.Deferred {
		t.Fatal(page, err)
	}
	raw, err := json.Marshal(page)
	if err != nil || len(raw) > docViewBytes {
		t.Fatal("wire budget exceeded", len(raw), err)
	}
	manifest, _, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, manifest.Chunks[len(manifest.Chunks)-1].Hash, []byte("corrupt selected chunk")); err != nil {
		t.Fatal(err)
	}
	if page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{View: "chat", Offset: 1}); err == nil || page.Hash != "" {
		t.Fatal("selected bytes not verified", page, err)
	}
	ctx, cancel := context.WithCancel(fixture.ctx)
	cancel()
	if page, err := fixture.service.ReadDocVisiblePage(ctx, fixture.repo, hash, domain.DocVisiblePageRequest{View: "chat", Offset: 0}); !errors.Is(err, context.Canceled) || page.Hash != "" {
		t.Fatal("cancelled read returned data", page, err)
	}
}
