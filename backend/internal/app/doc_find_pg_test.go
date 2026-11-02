//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGDocFindBoundedScanDoesNotReadWholeHistory(test *testing.T) {
	for _, count := range []int{1000, 10000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope}
			for index := 0; index < count; index++ {
				event := historyMessage(domain.RoleUser, "ordinary source text without the requested marker")
				event.Seq = index
				document.Events = append(document.Events, event)
			}
			hash := seedDocViewBoundaryPG(test, fixture, document)
			visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "chat", Offset: 0, Limit: 20})
			observed := &visibleCostStore{docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}}
			fixture.service.blobs = observed
			page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{
				Selection: visible.ReplayCursor, Query: "needle", To: visible.Total})
			if err != nil || len(page.Matches) != 0 || page.Index != docFindScanEvents || page.NextCursor == nil ||
				observed.unitReads != 0 || observed.pages != 1 || observed.chunkReads > 1 || observed.chunkBytes > domain.ConversationManifestChunkBytes || observed.fallbackPages != 0 {
				test.Fatal("find work grew with archive length", page, err, observed)
			}
			test.Logf("events=%d scanned_events=%d range_pages=%d chunk_reads=%d chunk_bytes=%d", count, page.Index, observed.pages, observed.chunkReads, observed.chunkBytes)
		})
	}
}

func TestPGDocFindDecodedAndOversizedFieldsResumeExactly(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
		{Kind: domain.EventToolCall, ToolName: "Bash", Input: map[string]any{"command": "printf \"<NEEDLE>\"\n\ud55c\uae00"}},
		{Kind: domain.EventToolResult, Output: strings.Repeat("needle ", 50)},
		{Kind: domain.EventReasoning, RedactedSummary: "Visible explanation", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: "needle opaque state"}},
		{Kind: domain.EventToolResult, Output: strings.Repeat("\ud55c\uae00", 100_000) + "needle" + strings.Repeat("x", 90_000)},
	}}
	for index := range document.Events {
		document.Events[index].Seq = index
	}
	hash := seedDocViewBoundaryPG(test, fixture, document)
	visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "all", Offset: 0, Limit: 20})
	request := domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle", To: visible.Total}
	var matches []domain.DocFindMatch
	seen := map[string]bool{}
	for attempt := 0; attempt < 20; attempt++ {
		page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
		if err != nil || page.Generation != visible.Generation || len(page.Matches) > domain.DocFindPageMatches {
			test.Fatal(page, err)
		}
		matches = append(matches, page.Matches...)
		for _, match := range page.Matches {
			replayed, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Cursor: match.RevealCursor})
			if err != nil || replayed.Generation != visible.Generation || replayed.View != "all" || replayed.From != match.Index || len(replayed.Units) != 1 ||
				replayed.Units[0].Event == nil || replayed.Units[0].Event.Index != match.Index || match.Preview == "" {
				test.Fatal("find reveal escaped its source generation or event", match, replayed, err)
			}
		}
		if page.NextCursor == nil {
			break
		}
		if seen[*page.NextCursor] {
			test.Fatal("find cursor did not advance")
		}
		seen[*page.NextCursor] = true
		request = domain.DocFindPageRequest{Query: request.Query, Cursor: *page.NextCursor}
	}
	if len(matches) != 52 || matches[0].Index != 0 || matches[0].Raw || matches[len(matches)-1].Index != 3 || !matches[len(matches)-1].Raw {
		test.Fatal("find did not cover decoded and oversized fields", matches)
	}
	for _, match := range matches {
		if match.Index == 2 {
			test.Fatal("small locked state was searched", match)
		}
	}
}

func TestPGDocFindRechecksCurrentGenerationAuthorizationAndOwnership(test *testing.T) {
	for _, changed := range []string{"source", "base"} {
		test.Run(changed, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := fixture.cir
			document.Events[2] = historyMessage(domain.RoleUser, strings.Repeat("needle ", 50))
			document.Events[2].Seq = 2
			hash := seedDocViewBoundaryPG(test, fixture, document)
			previous := document
			previous.Events = previous.Events[:2]
			base := seedDocViewBoundaryPG(test, fixture, previous)
			visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "chat", Base: base, Offset: -1, Limit: 20})
			initial := domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle", From: visible.Start, To: visible.Total}
			first, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, initial)
			if err != nil || first.NextCursor == nil || len(first.Matches) != 20 {
				test.Fatal(first, err)
			}
			requests := []domain.DocFindPageRequest{initial, {Query: "needle", Cursor: *first.NextCursor}}
			for _, request := range requests {
				calls := 0
				denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
					calls++
					return bound, domain.ErrForbidden
				})
				out, err := fixture.service.FindDocPage(denied, fixture.repo, hash, request)
				if !errors.Is(err, domain.ErrForbidden) || calls != 1 || !reflect.DeepEqual(out, domain.DocFindPage{}) {
					test.Fatal("find bypassed current authorization", out, err, calls)
				}
			}
			updated := hash
			if changed == "base" {
				updated = base
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, updated); err != nil {
				test.Fatal(err)
			}
			if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, updated); err != nil {
				test.Fatal(err)
			}
			readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "chat", Base: base, Offset: -1, Limit: 20})
			for _, request := range requests {
				out, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
				if !errors.Is(err, domain.ErrDocVisibleGenerationStale) || !reflect.DeepEqual(out, domain.DocFindPage{}) {
					test.Fatal("find mixed source generations", out, err)
				}
			}
			if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, fixture.repo, hash); err != nil {
				test.Fatal(err)
			}
			out, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, initial)
			if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(out, domain.DocFindPage{}) {
				test.Fatal("find bypassed ownership removal", out, err)
			}
		})
	}
}
