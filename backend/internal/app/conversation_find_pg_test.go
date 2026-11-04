//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGConversationFindReadOnlyDemandAndNativeReplay(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		fixture := newDocDisplayPGFixture(test)
		fixture.service.ConfigureDocumentDisplay(fixture.storage)
		output := strings.Repeat("a", domain.DocDisplayPageTextBytes-2) + "NEEDLE" + strings.Repeat("z", 700_000) + "NEEDLE" + string(fixture.repo)
		events := []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}
		var hash domain.ContentHash
		if legacy {
			hash = seedLegacyFindPGDocument(test, fixture, events).Hash
		} else {
			hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: events})
		}
		request := domain.ConversationFindRequest{Query: "needle"}
		if err := fixture.service.RequestConversationFind(fixture.ctx, fixture.repo, hash, domain.ConversationFindRequest{Query: request.Query, Cursor: "malformed"}); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("malformed cursor reached preparation", err)
		}
		if _, err := fixture.service.QueryConversationFind(fixture.ctx, fixture.repo, hash, request); err == nil {
			test.Fatal("cold query unexpectedly ready")
		}
		var jobs int
		if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&jobs); err != nil || jobs != 0 {
			test.Fatal("read-only query created display work", jobs, err)
		}
		matches := 0
		finished := false
		for attempt := 0; attempt < 100; attempt++ {
			result, err := fixture.service.QueryConversationFind(fixture.ctx, fixture.repo, hash, request)
			var pending *domain.ConversationFindNotReady
			if errors.As(err, &pending) {
				if !domain.IsOnlyConversationFindPending(pending, true) {
					test.Fatal("not-ready cause was not strictly pending", err)
				}
				request.Cursor = pending.Cursor
				if err := fixture.service.RequestConversationFind(fixture.ctx, fixture.repo, hash, request); err != nil {
					test.Fatal("API demand rejected valid read target", legacy, err)
				}
				if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
					test.Fatal(err)
				}
				if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
					test.Fatal(err)
				}
				continue
			}
			if err != nil || result.Page.Hash != hash || result.ReplayCursor == "" || result.Page.Prepared == nil {
				test.Fatal("bounded native conversation search failed", legacy, result, err)
			}
			replay, err := fixture.service.QueryConversationFind(fixture.ctx, fixture.repo, hash, domain.ConversationFindRequest{Query: request.Query, Cursor: result.ReplayCursor})
			if err != nil || replay.ReplayCursor != result.ReplayCursor || replay.Page.Index != result.Page.Index || len(replay.Page.Matches) != len(result.Page.Matches) {
				test.Fatal("native replay did not retain first query selection", replay, err)
			}
			for _, match := range result.Page.Matches {
				if match.SourceRange == nil || match.Raw || match.Field != 0 {
					test.Fatal("display query lost exact source provenance", match)
				}
				matches++
			}
			if _, err := fixture.service.QueryConversationFind(fixture.ctx, fixture.repo, hash, domain.ConversationFindRequest{Query: "different", Cursor: result.ReplayCursor}); !errors.Is(err, domain.ErrValidation) {
				test.Fatal("replay cursor accepted another query", err)
			}
			if result.Page.NextCursor == nil {
				finished = true
				break
			}
			request.Cursor = *result.Page.NextCursor
		}
		if !finished || matches != 2 {
			test.Fatal("conversation search did not finish exact matches", legacy, finished, matches)
		}
	}
}

func TestPGConversationFindRechecksAuthorityAndCurrentGeneration(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{historyMessage(domain.RoleUser, "visible needle")}})
	request := domain.ConversationFindRequest{Query: "needle"}
	allowed := true
	generalChecks, snapshotChecks := 0, 0
	authority := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(read context.Context, requested domain.ContentHash) (context.Context, error) {
		if requested != fixture.repo || fixture.storage.InTransaction(read) {
			test.Fatal("general conversation authority ran inside a SQL snapshot")
		}
		generalChecks++
		if !allowed {
			return read, domain.ErrForbidden
		}
		return read, nil
	})
	unsupported, err := fixture.service.QueryConversationFind(authority, fixture.repo, hash, request)
	if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(unsupported, domain.ConversationFindPage{}) || generalChecks == 0 {
		test.Fatal("general-only PostgreSQL authority was silently used for a pinned read", unsupported, err, generalChecks)
	}
	authority = inbound.WithDocumentSnapshotReadAuthorizer(authority, func(read context.Context, requested domain.ContentHash) (context.Context, error) {
		if requested != fixture.repo || !fixture.storage.InReadOnlyTransaction(read) {
			test.Fatal("snapshot conversation authority ran outside its read-only snapshot")
		}
		snapshotChecks++
		if !allowed {
			return read, domain.ErrForbidden
		}
		return read, nil
	})
	result, err := fixture.service.QueryConversationFind(authority, fixture.repo, hash, request)
	if err != nil || snapshotChecks == 0 {
		test.Fatal("conversation read omitted current snapshot authority", err, snapshotChecks)
	}
	request.Cursor = result.ReplayCursor
	allowed = false
	deniedResult, err := fixture.service.QueryConversationFind(authority, fixture.repo, hash, request)
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(deniedResult, domain.ConversationFindPage{}) {
		test.Fatal("replay bypassed current read authority", deniedResult, err)
	}
	denied, cancel := context.WithCancel(fixture.ctx)
	cancel()
	if _, err := fixture.service.QueryConversationFind(denied, fixture.repo, hash, request); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled replay read returned data", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.service.QueryConversationFind(fixture.ctx, fixture.repo, hash, request); err == nil {
		test.Fatal("replaced source reused old selection")
	}
	if err := fixture.service.RequestConversationFind(fixture.ctx, fixture.repo, hash, domain.ConversationFindRequest{Query: "needle"}); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.service.QueryConversationFind(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocVisibleGenerationStale) {
		test.Fatal("generation replacement did not reject pinned replay", err)
	}
}
