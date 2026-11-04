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
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGDocDisplayFindRootAndLegacyBasePinnedSource(test *testing.T) {
	for _, legacyBase := range []bool{false, true} {
		fixture := newDocViewPGFixture(test)
		document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{
			historyMessage(domain.RoleUser, "shared baseline"),
			historyMessage(domain.RoleAssistant, strings.Repeat("**Needle🙂** ", 45)+"[label](https://hidden.example)"),
		}}
		var base domain.ContentHash
		if legacyBase {
			base = seedLegacyFindPGDocument(test, fixture, document.Events[:1]).Hash
		} else {
			previous := document
			previous.Events = previous.Events[:1]
			base = seedDocViewBoundaryPG(test, fixture, previous)
		}
		hash := seedDocViewBoundaryPG(test, fixture, document)
		visibleRequest := domain.DocVisiblePageRequest{Base: base, View: "chat", Offset: -1, Limit: 2}
		var visible domain.DocVisiblePage
		if legacyBase {
			visible = readDocVisibleLegacyBaseReadyPG(test, fixture, hash, visibleRequest)
		} else {
			visible = readVisibleReplayReadyPG(test, fixture, hash, visibleRequest)
		}
		before := legacyFindPGImage(test, fixture)
		request := domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle🙂", From: 1, To: 2, Representation: domain.DocFindDisplayRepresentation}
		var firstCursor string
		var matches []domain.DocFindMatch
		for iteration := 0; iteration < 5; iteration++ {
			page, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, request)
			if err != nil || page.Projection != domain.DocFindDisplayProjection || page.Generation != visible.Generation || page.Hash != hash {
				test.Fatal(page, err)
			}
			matches = append(matches, page.Matches...)
			if page.NextCursor == nil {
				break
			}
			if firstCursor == "" {
				firstCursor = *page.NextCursor
			}
			request = domain.DocFindPageRequest{Query: request.Query, Cursor: *page.NextCursor, Representation: domain.DocFindDisplayRepresentation}
		}
		if len(matches) != 45 || firstCursor == "" {
			test.Fatal("missing display continuation", len(matches))
		}
		for _, match := range matches {
			if match.SourceRange == nil || match.Field != 0 || match.Raw {
				test.Fatal(match)
			}
			fragment, err := fixture.service.ReadDocFindFragment(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, hash, domain.DocFindFragmentRequest{Selection: match.RevealCursor, Offset: match.SourceRange.From, Budget: 64})
			if err != nil || fragment.Generation != visible.Generation || !strings.HasPrefix(fragment.JSON, "Needle🙂") {
				test.Fatal(fragment, err, match)
			}
		}
		if before != legacyFindPGImage(test, fixture) {
			test.Fatal("display scan rewrote source or indexes")
		}
		denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
			return bound, domain.ErrForbidden
		})
		page, err := fixture.service.FindDocPage(denied, fixture.repo, hash, request)
		if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
			test.Fatal("stale authority", page, err)
		}
		wrong := domain.DocFindPageRequest{Query: "needle🙂", Cursor: firstCursor}
		if _, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, wrong); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("source continuation accepted display cursor", err)
		}
		if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, base); err != nil {
			test.Fatal(err)
		}
		if legacyBase {
			readDocVisibleLegacyBaseReadyPG(test, fixture, hash, visibleRequest)
		} else {
			if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, base); err != nil {
				test.Fatal(err)
			}
			readVisibleReplayReadyPG(test, fixture, hash, visibleRequest)
		}
		request = domain.DocFindPageRequest{Query: "needle🙂", Cursor: firstCursor, Representation: domain.DocFindDisplayRepresentation}
		page, err = fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, request)
		if !errors.Is(err, domain.ErrDocVisibleGenerationStale) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
			test.Fatal("old generation accepted", page, err)
		}
	}
}

func TestPGDocDisplayFindLegacyReadOnlyOriginalIdentity(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	doc := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{historyMessage(domain.RoleUser, strings.Repeat("**needle** ", 25))})
	before := legacyFindPGImage(test, fixture)
	request := domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1, Representation: domain.DocFindDisplayRepresentation}
	first, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash, request)
	if err != nil || len(first.Matches) != 20 || first.NextCursor == nil || first.Projection != domain.DocLegacyFindDisplayProjection || first.Generation != "" {
		test.Fatal(first, err)
	}
	second, err := fixture.service.FindDocPage(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *first.NextCursor, Representation: domain.DocFindDisplayRepresentation})
	if err != nil || len(second.Matches) != 5 || second.NextCursor != nil {
		test.Fatal(second, err)
	}
	if before != legacyFindPGImage(test, fixture) {
		test.Fatal("legacy display mutated database")
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, fixture.repo, doc.Hash); err != nil {
		test.Fatal(err)
	}
	page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, doc.Hash, request)
	if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
		test.Fatal("ownership removal bypassed", page, err)
	}
}

func TestPGDocDisplayFindReadCostDoesNotScaleWithArchiveLength(test *testing.T) {
	for _, count := range []int{1000, 10000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope}
			for index := 0; index < count; index++ {
				event := historyMessage(domain.RoleUser, "**ordinary** [message](https://hidden.example)")
				event.Seq = index
				document.Events = append(document.Events, event)
			}
			hash := seedDocViewBoundaryPG(test, fixture, document)
			visible := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "chat", Offset: 0, Limit: 20})
			observed := &visibleCostStore{docViewPGStore: &docViewPGStore{PostgresStore: fixture.storage}}
			fixture.service.blobs = observed
			page, err := fixture.service.FindDocPage(fixture.ctx, fixture.repo, hash, domain.DocFindPageRequest{Selection: visible.ReplayCursor, Query: "needle", To: visible.Total, Representation: domain.DocFindDisplayRepresentation})
			if err != nil || page.Index != docFindScanEvents || page.NextCursor == nil || len(page.Matches) != 0 || observed.pages != 1 || observed.chunkReads > 1 || observed.fallbackPages != 0 || observed.unitReads != 0 {
				test.Fatal("display scan loaded unrelated archive data", page, err, observed)
			}
			test.Logf("events=%d scanned_events=%d range_pages=%d chunk_reads=%d chunk_bytes=%d", count, page.Index, observed.pages, observed.chunkReads, observed.chunkBytes)
		})
	}
}
