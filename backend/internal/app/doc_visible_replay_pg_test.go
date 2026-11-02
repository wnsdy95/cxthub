//go:build postgres

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func visibleReplayRequest(page domain.DocVisiblePage) domain.DocVisiblePageRequest {
	if page.Kind == "fold" {
		return domain.DocVisiblePageRequest{Expand: page.ReplayCursor}
	}
	return domain.DocVisiblePageRequest{Cursor: page.ReplayCursor}
}

func assertVisibleReplayPG(test *testing.T, fixture docViewPGFixture, page domain.DocVisiblePage) {
	test.Helper()
	selector, err := domain.DecodeDocVisibleSelector(page.ReplayCursor)
	if err != nil {
		test.Fatal("page has no canonical replay cursor", page.Kind, page.From, err)
	}
	if selector.Purpose != page.Kind || selector.Repo != fixture.repo || selector.Hash != page.Hash || selector.Base != page.Base ||
		selector.Generation != page.Generation || selector.View != page.View || selector.Start != page.Start ||
		selector.Total != page.Total || selector.Inherited != page.Inherited || selector.Next != page.From {
		test.Fatal("replay cursor does not describe incoming selection", selector, page)
	}
	replayed, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, page.Hash, visibleReplayRequest(page))
	if err != nil {
		test.Fatal("replay failed", page.Kind, page.From, err)
	}
	if !reflect.DeepEqual(replayed, page) {
		test.Fatal("replay changed page", page, replayed)
	}
	originalJSON, err := json.Marshal(page)
	if err != nil {
		test.Fatal(err)
	}
	replayedJSON, err := json.Marshal(replayed)
	if err != nil || !bytes.Equal(originalJSON, replayedJSON) {
		test.Fatal("replay changed wire bytes", err)
	}
}

func readVisibleReplayReadyPG(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash, request domain.DocVisiblePageRequest) domain.DocVisiblePage {
	test.Helper()
	for attempt := 0; attempt < 3; attempt++ {
		page, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request)
		if err == nil {
			return page
		}
		if !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal(err)
		}
		if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
			test.Fatal(err)
		}
	}
	test.Fatal("visible replay fixture did not become ready")
	return domain.DocVisiblePage{}
}

func readVisibleReplayFoldPG(test *testing.T, fixture docViewPGFixture, page domain.DocVisiblePage) domain.DocVisiblePage {
	test.Helper()
	for _, unit := range page.Units {
		if unit.Fold != nil {
			return readVisibleReplayReadyPG(test, fixture, page.Hash, domain.DocVisiblePageRequest{Expand: unit.Fold.Cursor})
		}
	}
	test.Fatal("missing fold fixture", page)
	return domain.DocVisiblePage{}
}

func TestPGDocVisibleReplayReproducesUnitsAndFolds(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	document := visiblePGDocument(fixture, 139, 3)
	hash := seedDocViewBoundaryPG(test, fixture, document)
	previous := document
	previous.Events = previous.Events[:2]
	base := seedDocViewBoundaryPG(test, fixture, previous)
	readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{Base: base, View: "chat", Offset: -1, Limit: 2})
	foldContinuations := 0
	for _, mode := range []string{"all", "chat", "prompts"} {
		for _, request := range []domain.DocVisiblePageRequest{
			{Offset: -1}, {Offset: 0}, {Offset: 140}, {Offset: -1, Base: base},
		} {
			request.View, request.Limit = mode, 2
			if mode == "all" {
				request.Limit = 100
			}
			pages := readAllVisiblePG(test, fixture, hash, request)
			if len(pages) < 2 {
				test.Fatal("fixture did not exercise continuation", mode, request)
			}
			for position, page := range pages {
				assertVisibleReplayPG(test, fixture, page)
				selector, err := domain.DecodeDocVisibleSelector(page.ReplayCursor)
				if err != nil || selector.Limit != request.Limit {
					test.Fatal("replay lost selected limit", selector, err)
				}
				if position > 0 && page.ReplayCursor != *pages[position-1].NextCursor {
					test.Fatal("continuation cursor was not reused for replay")
				}
				for _, unit := range page.Units {
					if unit.Fold == nil {
						continue
					}
					token := unit.Fold.Cursor
					for attempt := 0; attempt < 10; attempt++ {
						fold, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Expand: token})
						if err != nil {
							test.Fatal(err)
						}
						if fold.ReplayCursor != token || len(fold.Events) == 0 {
							test.Fatal("fold replay did not preserve incoming cursor", fold)
						}
						assertVisibleReplayPG(test, fixture, fold)
						if fold.NextCursor == nil {
							break
						}
						foldContinuations++
						token = *fold.NextCursor
						if attempt == 9 {
							test.Fatal("fold replay did not terminate")
						}
					}
				}
			}
		}
	}
	if foldContinuations == 0 {
		test.Fatal("fixture did not exercise middle fold replay")
	}
}

func TestPGDocVisibleReplayHandlesEmptyAndFullyInheritedPages(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	for _, empty := range []bool{false, true} {
		document := domain.CIRDocument{Envelope: fixture.cir.Envelope}
		if !empty {
			document.Events = fixture.cir.Events[:4]
		}
		hash := seedDocViewBoundaryPG(test, fixture, document)
		total := len(document.Events)
		for _, mode := range []string{"all", "chat", "prompts"} {
			requests := []domain.DocVisiblePageRequest{
				{View: mode, Offset: total},
				{View: mode, Base: hash, Offset: -1},
			}
			if empty {
				requests = append(requests, domain.DocVisiblePageRequest{View: mode, Offset: -1})
			}
			for _, request := range requests {
				page := readVisibleReplayReadyPG(test, fixture, hash, request)
				if page.Start != total || page.From != total || page.To != total || page.Total != total ||
					page.Kind != "units" || len(page.Units) != 0 || len(page.Events) != 0 || page.Fold != nil || page.NextCursor != nil {
					test.Fatal("unexpected empty page", page)
				}
				if request.Base != "" && page.Inherited != total {
					test.Fatal("fully inherited start was not resolved", page)
				}
				selector, err := domain.DecodeDocVisibleSelector(page.ReplayCursor)
				if err != nil || selector.Unit != 0 || selector.Start != total || selector.Next != total {
					test.Fatal("terminal replay tuple is not canonical", selector, err)
				}
				assertVisibleReplayPG(test, fixture, page)
			}
		}
	}
}

func TestPGDocVisibleReplayRejectsChangedGenerations(test *testing.T) {
	for _, scenario := range []string{"units", "fold", "end offset", "empty", "base", "fully inherited"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			document := visiblePGDocument(fixture, 139, 3)
			if scenario == "empty" {
				document.Events = nil
			}
			hash := seedDocViewBoundaryPG(test, fixture, document)
			request := domain.DocVisiblePageRequest{View: "chat", Offset: 0, Limit: 3}
			changed := hash
			switch scenario {
			case "end offset":
				request.Offset = len(document.Events)
			case "base":
				previous := document
				previous.Events = previous.Events[:2]
				request.Base = seedDocViewBoundaryPG(test, fixture, previous)
				request.Offset, changed = -1, request.Base
			case "fully inherited":
				request.Base, request.Offset = hash, -1
			}
			page := readVisibleReplayReadyPG(test, fixture, hash, request)
			if scenario == "fold" {
				page = readVisibleReplayFoldPG(test, fixture, page)
			}
			assertVisibleReplayPG(test, fixture, page)
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, changed); err != nil {
				test.Fatal(err)
			}
			if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, changed); err != nil {
				test.Fatal(err)
			}
			stale, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, visibleReplayRequest(page))
			if errors.Is(err, domain.ErrDocRangeIndexPending) {
				if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
					test.Fatal(err)
				}
				stale, err = fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, visibleReplayRequest(page))
			}
			if !errors.Is(err, domain.ErrDocVisibleGenerationStale) || !reflect.DeepEqual(stale, domain.DocVisiblePage{}) {
				test.Fatal("replay bypassed current generation check", stale, err)
			}
			fresh := readVisibleReplayReadyPG(test, fixture, hash, request)
			if fresh.Generation == page.Generation || fresh.ReplayCursor == page.ReplayCursor {
				test.Fatal("fresh selection reused stale replay", fresh)
			}
			assertVisibleReplayPG(test, fixture, fresh)
		})
	}
}

func TestPGDocVisibleReplayRechecksAuthorizationAndOwnership(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	hash := seedDocViewBoundaryPG(test, fixture, visiblePGDocument(fixture, 139, 3))
	page := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "chat", Offset: 0, Limit: 3})
	fold := readVisibleReplayFoldPG(test, fixture, page)
	terminal := readVisibleReplayReadyPG(test, fixture, hash, domain.DocVisiblePageRequest{View: "chat", Offset: page.Total})
	pages := []domain.DocVisiblePage{page, fold, terminal}
	for _, selected := range pages {
		calls := 0
		denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
			calls++
			if repo != fixture.repo {
				return bound, domain.ErrIntegrity
			}
			return bound, domain.ErrForbidden
		})
		out, err := fixture.service.ReadDocVisiblePage(denied, fixture.repo, hash, visibleReplayRequest(selected))
		if !errors.Is(err, domain.ErrForbidden) || calls != 1 || !reflect.DeepEqual(out, domain.DocVisiblePage{}) {
			test.Fatal("replay bypassed authorization", selected.Kind, calls, out, err)
		}
		cancelled, cancel := context.WithCancel(fixture.ctx)
		cancel()
		out, err = fixture.service.ReadDocVisiblePage(cancelled, fixture.repo, hash, visibleReplayRequest(selected))
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(out, domain.DocVisiblePage{}) {
			test.Fatal("cancelled replay returned data", selected.Kind, out, err)
		}
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='doc'`, fixture.repo, hash); err != nil {
		test.Fatal(err)
	}
	for _, selected := range pages {
		out, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, visibleReplayRequest(selected))
		if !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(out, domain.DocVisiblePage{}) {
			test.Fatal("replay bypassed ownership revocation", selected.Kind, out, err)
		}
	}
}
