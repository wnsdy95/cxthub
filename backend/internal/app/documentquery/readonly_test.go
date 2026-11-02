package documentquery

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

func TestReadOnlyOwnerForcesPlainContextAcrossEveryPublicRead(test *testing.T) {
	queries, fixture := newReadOnlyQueries(test, false, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "visible needle")})
	assertReadOnlyDependencies(test, queries)
	ctx := inbound.WithSystemActor(context.Background())
	if index, err := queries.ReadIndex(ctx, fixture.repo, fixture.hash); err != nil || len(index.Events) != 1 {
		test.Fatal("read-only index failed", index, err)
	}
	if page, err := queries.ReadDocEvents(ctx, fixture.repo, fixture.hash, "", 0, 1); err != nil || len(page.Events) != 1 {
		test.Fatal("read-only event page failed", page, err)
	}
	if matches, err := queries.SearchDocEvents(ctx, fixture.repo, fixture.hash, "needle", -1, 1); err != nil || len(matches) != 1 {
		test.Fatal("read-only event search failed", matches, err)
	}
	if page, err := queries.ReadDocFragments(ctx, fixture.repo, fixture.hash, 0, 0, 1, 64<<10); err != nil || len(page.Fragments) != 1 {
		test.Fatal("read-only event fragments failed", page, err)
	}
	if page, err := queries.ReadDocViewEvents(ctx, fixture.repo, fixture.hash, "", 0, 1, "all"); err != nil || len(page.Events) != 1 {
		test.Fatal("read-only view page failed", page, err)
	}
	if page, err := queries.ReadDocViewFragment(ctx, fixture.repo, fixture.hash, 0, 0, 64); err != nil || page.JSON == "" {
		test.Fatal("read-only view fragment failed", page, err)
	}
	visible, err := queries.ReadDocVisiblePage(ctx, fixture.repo, fixture.hash, domain.DocVisiblePageRequest{View: "all", Limit: 1})
	if err != nil || visible.ReplayCursor == "" || len(visible.Units) != 1 || visible.Units[0].Event == nil {
		test.Fatal("read-only visible page failed without a preparation port", visible, err)
	}
	find, err := queries.FindDocPage(ctx, fixture.repo, fixture.hash, domain.DocFindPageRequest{Representation: domain.DocFindPreparedRepresentation, Selection: visible.ReplayCursor, Query: "needle", To: 1})
	if err != nil || len(find.Matches) != 1 || find.Matches[0].RevealCursor == "" {
		test.Fatal("read-only native find failed without a demand port", find, err)
	}
	if page, err := queries.ReadDocFindFragment(ctx, fixture.repo, fixture.hash, domain.DocFindFragmentRequest{Selection: find.Matches[0].RevealCursor, Budget: 64}); err != nil || page.JSON == "" {
		test.Fatal("read-only find reveal failed", page, err)
	}
	if page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"}); err != nil || len(page.Page.Matches) != 1 {
		test.Fatal("read-only conversation owner failed", page, err)
	}
	if outbound.DocReadOnly(ctx) || fixture.preparations != 0 || fixture.enqueued != 0 || fixture.active != 0 {
		test.Fatal("read-only instance mutated caller context, prepared work, or leaked resources", fixture)
	}
	before := fixture.admissions
	if err := queries.PrepareConversationFind(ctx, "", "", domain.ConversationFindRequest{}); err != domain.ErrForbidden || fixture.admissions != before {
		test.Fatal("read-only instance entered explicit preparation with a writable caller context", err)
	}
}

func TestReadOnlyOwnerPreservesNativeReplayPendingAndIdentityMeaning(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprint(legacy), func(test *testing.T) {
			queries, fixture := newReadOnlyQueries(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "visible needle")})
			assertReadOnlyDependencies(test, queries)
			ctx := inbound.WithSystemActor(context.Background())
			fixture.ready = false
			page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
			var pending *domain.ConversationFindNotReady
			if !errors.As(err, &pending) || pending.Cursor != "" || !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(page, domain.ConversationFindPage{}) || fixture.chunks != 0 {
				test.Fatal("read-only cold conversation lost pending semantics or loaded document bytes", page, err, fixture)
			}
			fixture.ready = true
			page, err = queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
			if err != nil || len(page.Page.Matches) != 1 || page.ReplayCursor == "" || page.Page.Prepared == nil {
				test.Fatal("read-only warm native search failed", page, err)
			}
			replay, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: page.ReplayCursor})
			if err != nil || !reflect.DeepEqual(replay, page) {
				test.Fatal("read-only mode changed a native replay", replay, err)
			}
			before := fixture.admissions
			if _, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "Needle", Cursor: page.ReplayCursor}); err != domain.ErrValidation || fixture.admissions != before {
				test.Fatal("read-only mode changed exact query-hash validation precedence", err)
			}
			if legacy {
				if _, err := queries.ReadDocFindFragment(ctx, fixture.repo, fixture.hash, domain.DocFindFragmentRequest{Legacy: true, Index: 0, Total: 1, Budget: 64}); err != nil {
					test.Fatal("read-only legacy reveal fell back or failed", err)
				}
				if _, err := queries.ReadDocVisiblePage(ctx, fixture.repo, fixture.hash, domain.DocVisiblePageRequest{View: "all", Limit: 1}); err != domain.ErrDocVisibleProjectionUnsupported {
					test.Fatal("read-only mode broadened root-only visible meaning", err)
				}
			} else {
				fixture.binding.SourceVersion++
				if _, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: page.ReplayCursor}); err != domain.ErrDocVisibleGenerationStale {
					test.Fatal("read-only replay ignored a changed pinned source", err)
				}
			}
			fixture.readFailure = domain.ErrForbidden
			if _, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: page.ReplayCursor}); err != domain.ErrForbidden {
				test.Fatal("read-only replay hid current authority behind pending or a stale source", err)
			}
			if fixture.preparations != 0 || fixture.enqueued != 0 || fixture.active != 0 {
				test.Fatal("read-only native replay prepared work or leaked admission", fixture)
			}
		})
	}
}

func TestReadOnlyOwnerUsesLegacyReadinessWithoutInferringDemand(test *testing.T) {
	ctx := inbound.WithSystemActor(context.Background())
	queries, fixture := newReadOnlyQueries(test, true, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
	fixture.ready = false
	prepared, err := queries.prepareDocViewSource(ctx, fixture.repo, fixture.hash, docRangeLegacyBytes)
	if !prepared || err != domain.ErrDocRangeIndexUnavailable || fixture.pages != 0 || fixture.chunks != 0 {
		test.Fatal("read-only legacy readiness was skipped because RangeJobs is nil", prepared, err, fixture)
	}
	fixture.ready = true
	fixture.generationFailure = domain.ErrDocRangeIndexUnavailable
	if _, err := queries.ReadDocViewEvents(ctx, fixture.repo, fixture.hash, "", 0, 1, "all"); err != domain.ErrDocRangeIndexUnavailable || fixture.chunks != 0 {
		test.Fatal("read-only missing generation fell back without a RangeJobs dependency", err, fixture)
	}
	fixture.generationFailure = nil
	bytes := queries.view.ByteReadiness.(*readOnlyByteReadiness)
	bytes.enabled = true
	if _, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"}); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || bytes.checks != 1 || fixture.chunks != 0 {
		test.Fatal("read-only legacy byte readiness required a writer or loaded bytes", err, bytes.checks, fixture)
	}
	bytes.ready = true
	if _, err := queries.ReadDocViewEvents(ctx, fixture.repo, fixture.hash, "", 0, 1, "all"); err != domain.ErrDocRangeIndexUnavailable || fixture.chunks != 0 {
		test.Fatal("metadata-only generation satisfied an enabled byte-read requirement", err, fixture)
	}
	compatibility, legacy := newConversationFindFixture(test, true, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
	compatibility.view.RangeJobs = nil
	legacy.ready = false
	if prepared, err := compatibility.prepareDocViewSource(outbound.WithDocReadOnly(ctx), legacy.repo, legacy.hash, docRangeLegacyBytes); prepared || err != nil {
		test.Fatal("default mode changed the existing nil-RangeJobs legacy meaning", prepared, err)
	}
}

func TestReadOnlyOwnerPreservesBoundaryAndDisplayPreparationFailures(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		queries, fixture := newReadOnlyQueries(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
		boundary := queries.view.BoundaryRead.(*readOnlyBoundaryReads)
		boundary.failure = domain.ErrDocRangeIndexUnavailable
		ctx := inbound.WithSystemActor(context.Background())
		if _, err := queries.ReadDocViewEvents(ctx, fixture.repo, fixture.hash, fixture.hash, 0, 1, "all"); err != domain.ErrDocRangeIndexUnavailable || boundary.calls != 1 || fixture.chunks != 0 {
			test.Fatal("read-only boundary lookup required a demand dependency or read a full prefix", legacy, err, boundary.calls, fixture)
		}
		queries, fixture = newReadOnlyQueries(test, legacy, []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("x", docViewBytes+1)}})
		page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
		var pending *domain.ConversationFindNotReady
		if !errors.As(err, &pending) || pending.Cursor == "" || !errors.Is(err, domain.ErrConversationFindPreparationRequired) || !reflect.DeepEqual(page, domain.ConversationFindPage{}) || fixture.displays != 1 || fixture.chunks != 0 {
			test.Fatal("read-only display miss lost strict pending or its native replay", legacy, page, err, fixture)
		}
		cursor := pending.Cursor
		_, err = queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: cursor})
		if !errors.As(err, &pending) || pending.Cursor != cursor || fixture.displays != 2 || fixture.enqueued != 0 || fixture.preparations != 0 || fixture.active != 0 {
			test.Fatal("read-only display retry changed replay selection or prepared locally", legacy, err, fixture)
		}
	}
}

func TestReadOnlyOwnerUsesMinimalFindFieldsAndRetainsOnlySuccessfulResponses(test *testing.T) {
	queries, fixture := newReadOnlyQueries(test, false, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, strings.Repeat("x", docViewBytes+1))})
	ctx := inbound.WithSystemActor(context.Background())
	visible, err := queries.ReadDocVisiblePage(ctx, fixture.repo, fixture.hash, domain.DocVisiblePageRequest{View: "all", Limit: 1})
	if err != nil {
		test.Fatal(err)
	}
	retained := 0
	ctx = inbound.WithDocumentResponseRetainer(ctx, func(func()) bool {
		retained++
		return true
	})
	_, err = queries.FindDocPage(ctx, fixture.repo, fixture.hash, domain.DocFindPageRequest{Representation: domain.DocFindDisplayRepresentation, Selection: visible.ReplayCursor, Query: "needle", To: 1})
	if err != domain.ErrDocFindFieldsUnavailable || queries.find.FieldsRead.(*readOnlyFindFields).calls != 1 || fixture.enqueued != 0 || fixture.active != 0 || retained != 0 {
		test.Fatal("read-only find fields needed enqueue or retained an unsuccessful response", err, fixture, retained)
	}
	queries, fixture = newReadOnlyQueries(test, false, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
	var releases []func()
	ctx = inbound.WithDocumentResponseRetainer(inbound.WithSystemActor(context.Background()), func(release func()) bool {
		releases = append(releases, release)
		return true
	})
	test.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})
	if _, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"}); err != nil || len(releases) != 1 || fixture.active != 1 {
		test.Fatal("read-only policy changed successful response lifetime", err, fixture.active, len(releases))
	}
}
