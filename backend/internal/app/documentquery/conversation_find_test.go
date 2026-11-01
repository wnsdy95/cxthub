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

func conversationFindTestCursor(test *testing.T, repo, hash domain.ContentHash, query string, legacy bool, representation string) string {
	test.Helper()
	request := domain.DocFindPageRequest{Representation: representation, Query: query, Legacy: legacy, Total: 1, To: 1}
	if legacy {
		selector, err := domain.DocLegacyFindSelection(repo, hash, request)
		if err != nil {
			test.Fatal(err)
		}
		cursor, err := selector.Encode()
		if err != nil {
			test.Fatal(err)
		}
		return cursor
	}
	selection := domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: repo, Hash: hash,
		Generation: domain.HashContent([]byte("visible-generation")), View: "all", Total: 1, Limit: 1}
	var err error
	request.Selection, err = selection.Encode()
	if err != nil {
		test.Fatal(err)
	}
	request.Total = 0
	selector, err := docFindSelection(repo, hash, request)
	if err != nil {
		test.Fatal(err)
	}
	cursor, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return cursor
}

func TestConversationFindOwnerValidatesBeforeAdmissionOrCancellation(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("document"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	queries := &Queries{}
	for _, legacy := range []bool{false, true} {
		cursor := conversationFindTestCursor(test, repo, hash, "needle", legacy, domain.DocFindPreparedRepresentation)
		display := conversationFindTestCursor(test, repo, hash, "needle", legacy, domain.DocFindDisplayRepresentation)
		for _, candidate := range []struct {
			repo, hash domain.ContentHash
			request    domain.ConversationFindRequest
		}{
			{"invalid", hash, domain.ConversationFindRequest{Query: "needle"}},
			{repo, "invalid", domain.ConversationFindRequest{Query: "needle"}},
			{repo, hash, domain.ConversationFindRequest{}},
			{repo, hash, domain.ConversationFindRequest{Query: "needle", Cursor: "malformed"}},
			{repo, hash, domain.ConversationFindRequest{Query: "needle", Cursor: strings.Repeat("A", 32<<10)}},
			{repo, hash, domain.ConversationFindRequest{Query: "Needle", Cursor: cursor}},
			{domain.HashContent([]byte("other-repo")), hash, domain.ConversationFindRequest{Query: "needle", Cursor: cursor}},
			{repo, domain.HashContent([]byte("other-document")), domain.ConversationFindRequest{Query: "needle", Cursor: cursor}},
			{repo, hash, domain.ConversationFindRequest{Query: "needle", Cursor: display}},
		} {
			page, err := queries.QueryConversationFind(ctx, candidate.repo, candidate.hash, candidate.request)
			if err != domain.ErrValidation || !reflect.DeepEqual(page, domain.ConversationFindPage{}) {
				test.Fatal("invalid conversation find reached dependencies or changed validation precedence", legacy, candidate, page, err)
			}
			if err := queries.PrepareConversationFind(ctx, candidate.repo, candidate.hash, candidate.request); err != domain.ErrValidation {
				test.Fatal("invalid preparation reached dependencies or changed validation precedence", legacy, candidate, err)
			}
		}
	}
	if err := queries.PrepareConversationFind(outbound.WithDocReadOnly(ctx), "", "", domain.ConversationFindRequest{}); err != domain.ErrForbidden {
		test.Fatal("explicit preparation lost its read-only guard precedence", err)
	}
	if _, exposed := any(queries).(inbound.ConversationFindDemand); exposed {
		test.Fatal("standalone query owner exposes the application demand command")
	}
}

func TestConversationFindOwnerColdQueryIsReadOnlyAndRetainsPendingCursor(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprint(legacy), func(test *testing.T) {
			queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
			fixture.ready = false
			cursor := conversationFindTestCursor(test, fixture.repo, fixture.hash, "needle", legacy, domain.DocFindPreparedRepresentation)
			retained := 0
			ctx := inbound.WithDocumentResponseRetainer(inbound.WithSystemActor(context.Background()), func(func()) bool {
				retained++
				return true
			})
			for _, cursor := range []string{"", cursor} {
				page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: cursor})
				var pending *domain.ConversationFindNotReady
				if !errors.As(err, &pending) || pending.Cursor != cursor || !errors.Is(err, domain.ErrDocRangeIndexUnavailable) ||
					!domain.IsOnlyConversationFindPending(err, true) || !reflect.DeepEqual(page, domain.ConversationFindPage{}) {
					test.Fatal("cold query lost strict typed pending or its replay target", page, err)
				}
				if fixture.preparations != 0 || fixture.enqueued != 0 || fixture.pages != 0 || fixture.chunks != 0 || fixture.displays != 0 || fixture.active != 0 || retained != 0 {
					test.Fatal("read-only cold query prepared, scanned, or retained response resources", fixture, retained)
				}
			}
		})
	}
}

func TestConversationFindOwnerNativeReadReplayAndResponseLifetime(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprint(legacy), func(test *testing.T) {
			queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "visible NEEDLE 한글")})
			var releases []func()
			ctx := inbound.WithDocumentResponseRetainer(inbound.WithSystemActor(context.Background()), func(release func()) bool {
				releases = append(releases, release)
				return true
			})
			test.Cleanup(func() {
				for _, release := range releases {
					release()
				}
			})
			page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
			if err != nil || page.Page.Hash != fixture.hash || page.ReplayCursor == "" || page.Page.Prepared == nil || page.Page.NextCursor != nil || len(page.Page.Matches) != 1 {
				test.Fatal("actual owner did not read the native prepared projection", page, err)
			}
			projection := domain.DocFindPreparedProjection
			if legacy {
				projection = domain.DocLegacyFindPreparedProjection
				selector, err := domain.DecodeDocLegacyFindSelector(page.ReplayCursor)
				if err != nil || selector.Version != 3 || selector.Total != 1 || selector.To != 1 || selector.QueryHash != domain.HashContent([]byte("needle")) || selector.Prepared == nil {
					test.Fatal("legacy conversation cursor changed its native wire or query binding", selector, err)
				}
			} else {
				selector, err := domain.DecodeDocFindSelector(page.ReplayCursor)
				if err != nil || selector.Version != 3 || selector.To != 1 || selector.QueryHash != domain.HashContent([]byte("needle")) || selector.Prepared == nil ||
					selector.Selection.Repo != fixture.repo || selector.Selection.Hash != fixture.hash || selector.Selection.View != "all" || selector.Selection.Limit != 1 || selector.Selection.Generation != page.Page.Generation {
					test.Fatal("root conversation cursor lost its pinned native selection", selector, err)
				}
			}
			match := page.Page.Matches[0]
			if page.Page.Projection != projection || match.SourceRange == nil || match.PreparedPage == nil || match.Raw || match.Field != 0 || !legacy && match.RevealCursor == "" {
				test.Fatal("native conversation find changed match provenance or reveal semantics", page)
			}
			if len(releases) != 1 || fixture.active != 1 || fixture.preparations != 0 || fixture.enqueued != 0 || fixture.chunks == 0 {
				test.Fatal("query lost its response lifetime or performed preparation", fixture, len(releases))
			}
			replay, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: page.ReplayCursor})
			if err != nil || !reflect.DeepEqual(page, replay) || len(releases) != 2 || fixture.active != 2 {
				test.Fatal("native replay changed the original selection or response resources", replay, err, fixture.active, len(releases))
			}
			before := fixture.admissions
			if _, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "Needle", Cursor: page.ReplayCursor}); err != domain.ErrValidation || fixture.admissions != before {
				test.Fatal("case-insensitive matching weakened exact replay query hashing", err, fixture.admissions-before)
			}
		})
	}
}

func TestConversationFindOwnerMissingDisplayReturnsGeneratedReplayWithoutWrites(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprint(legacy), func(test *testing.T) {
			queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("x", docViewBytes+1)}})
			ctx := inbound.WithSystemActor(context.Background())
			page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
			var pending *domain.ConversationFindNotReady
			if !errors.As(err, &pending) || pending.Cursor == "" || !errors.Is(err, domain.ErrDocFindDisplayUnavailable) || !errors.Is(err, domain.ErrConversationFindPreparationRequired) ||
				!reflect.DeepEqual(page, domain.ConversationFindPage{}) || fixture.displays != 1 || fixture.preparations != 0 || fixture.enqueued != 0 || fixture.chunks != 0 {
				test.Fatal("missing display changed failure mapping, replay selection, or read-only behavior", page, err, fixture)
			}
			cursor := pending.Cursor
			_, err = queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: cursor})
			if !errors.As(err, &pending) || pending.Cursor != cursor || fixture.displays != 2 || fixture.preparations != 0 || fixture.enqueued != 0 {
				test.Fatal("pending display replay reselected or prepared work", err, fixture)
			}
			queries.find.DisplayRead = nil
			_, err = queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: cursor})
			if err != domain.ErrDocFindDisplayUnavailable || errors.As(err, &pending) || fixture.displays != 2 {
				test.Fatal("unsupported display was incorrectly presented as preparable", err, fixture)
			}
		})
	}
}

func TestConversationFindOwnerNativeContinuationDoesNotRestart(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, strings.Repeat("needle ", domain.DocFindPageMatches+5))})
		ctx := inbound.WithSystemActor(context.Background())
		first, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
		if err != nil || first.Page.NextCursor == nil || len(first.Page.Matches) != domain.DocFindPageMatches {
			test.Fatal("native first page lost the bounded match limit or continuation", legacy, first, err)
		}
		cursor := *first.Page.NextCursor
		next, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: cursor})
		if err != nil || next.ReplayCursor != cursor || next.Page.NextCursor != nil || len(next.Page.Matches) != 5 ||
			next.Page.Matches[0].Offset <= first.Page.Matches[len(first.Page.Matches)-1].Offset || fixture.preparations != 0 || fixture.enqueued != 0 || fixture.active != 0 {
			test.Fatal("native continuation restarted, changed its replay target, or prepared work", legacy, next, err, fixture)
		}
	}
}

func TestConversationFindOwnerRetainsIdentitySpecificCursorAndRootOnlyVisibleMeaning(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
		cursor := conversationFindTestCursor(test, fixture.repo, fixture.hash, "needle", !legacy, domain.DocFindPreparedRepresentation)
		ctx := inbound.WithSystemActor(context.Background())
		page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: cursor})
		if err != domain.ErrValidation || !reflect.DeepEqual(page, domain.ConversationFindPage{}) || fixture.admissions != 1 || fixture.pages != 0 || fixture.chunks != 0 {
			test.Fatal("conversation find reinterpreted a cursor for another current identity", legacy, page, err, fixture)
		}
		if legacy {
			if _, err := queries.ReadDocVisiblePage(outbound.WithDocReadOnly(ctx), fixture.repo, fixture.hash, domain.DocVisiblePageRequest{View: "all", Limit: 1}); err != domain.ErrDocVisibleProjectionUnsupported {
				test.Fatal("legacy conversation find broadened root-only visible projection meaning", err)
			}
		}
	}
}

func TestConversationFindOwnerSelectorFailureDoesNotInventReplayCursor(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
		fixture.generationFailure = domain.ErrDocRangeIndexUnavailable
		page, err := queries.QueryConversationFind(inbound.WithSystemActor(context.Background()), fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
		var pending *domain.ConversationFindNotReady
		if !errors.As(err, &pending) || pending.Cursor != "" || !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(page, domain.ConversationFindPage{}) ||
			fixture.preparations != 0 || fixture.enqueued != 0 || fixture.chunks != 0 || fixture.active != 0 {
			test.Fatal("selector failure invented a replay target or fell back to document bytes", legacy, page, err, fixture)
		}
	}
}

func TestConversationFindOwnerKeepsTerminalFailuresAndCurrentAuthority(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
		ctx := inbound.WithSystemActor(context.Background())
		page, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
		if err != nil {
			test.Fatal(err)
		}
		for _, failure := range []error{domain.ErrForbidden, domain.ErrUnauthorized, domain.ErrNotFound, domain.ErrIntegrity, domain.ErrConflict, domain.ErrDocumentIdentityUpgradeRequired,
			domain.ErrDocByteRangeBackfillRequired, errors.Join(domain.ErrDocRangeIndexUnavailable, domain.ErrForbidden)} {
			fixture.readFailure = failure
			reads := fixture.snapshots
			out, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: page.ReplayCursor})
			var pending *domain.ConversationFindNotReady
			classifiedPending := errors.As(err, &pending)
			matchingFailure := err == failure && !classifiedPending
			if failure == domain.ErrDocByteRangeBackfillRequired {
				matchingFailure = classifiedPending && pending.Cause == failure && pending.Cursor == page.ReplayCursor && errors.Is(err, domain.ErrDocRangeIndexPending)
			}
			if !matchingFailure || !reflect.DeepEqual(out, domain.ConversationFindPage{}) || fixture.snapshots != reads || fixture.active != 0 {
				test.Fatal("warm replay bypassed current authority or changed failure classification", legacy, failure, err, fixture.snapshots-reads, fixture.active)
			}
		}
		fixture.readFailure = nil
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := queries.QueryConversationFind(canceled, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: page.ReplayCursor}); err != context.Canceled {
			test.Fatal("canceled warm replay returned data", legacy, err)
		}
		if !legacy {
			fixture.binding.SourceVersion++
			if _, err := queries.QueryConversationFind(ctx, fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle", Cursor: page.ReplayCursor}); err != domain.ErrDocVisibleGenerationStale {
				test.Fatal("warm root replay ignored a changed source lifetime", err)
			}
		}
	}
}

func TestConversationFindOwnerFailureMappingRemainsStrict(test *testing.T) {
	cursor := "original-replay-cursor"
	for _, candidate := range []struct {
		failure  error
		readOnly bool
		pending  bool
	}{
		{domain.ErrDocRangeIndexPending, false, true},
		{domain.ErrDocFindFieldsPending, false, true},
		{domain.ErrDocDisplayPending, false, true},
		{domain.ErrDocRangeIndexUnavailable, true, true},
		{domain.ErrDocFindFieldsUnavailable, true, true},
		{domain.ErrDocRangeIndexUnavailable, false, false},
		{domain.ErrConversationFindPreparationRequired, false, false},
		{domain.ErrDocFindDisplayUnavailable, true, false},
		{errors.Join(domain.ErrDocFindDisplayUnavailable, domain.ErrConversationFindPreparationRequired), true, true},
		{errors.Join(domain.ErrDocFindDisplayUnavailable, domain.ErrConversationFindPreparationRequired, domain.ErrIntegrity), true, false},
		{errors.Join(domain.ErrDocDisplayPending, domain.ErrForbidden), true, false},
		{context.Canceled, true, false},
		{nil, true, false},
	} {
		mapped := conversationFindFailure(candidate.failure, cursor, candidate.readOnly)
		var pending *domain.ConversationFindNotReady
		if errors.As(mapped, &pending) != candidate.pending || candidate.pending && (pending.Cursor != cursor || pending.Cause != candidate.failure) || !candidate.pending && mapped != candidate.failure {
			test.Fatal("owner failure mapping changed a cause, replay cursor, or terminal classification", candidate, mapped)
		}
	}
}

func TestConversationFindOwnerReauthorizesAfterInteractiveAdmission(test *testing.T) {
	for _, stage := range []string{"query", "root-selection", "legacy-selection"} {
		test.Run(stage, func(test *testing.T) {
			assertVisibleReadAuthorizationAfterAdmission(test, func(ctx context.Context, queries *Queries, selection domain.DocVisibleSelector) error {
				if stage == "query" {
					_, err := queries.QueryConversationFind(ctx, selection.Repo, selection.Hash, domain.ConversationFindRequest{Query: "needle"})
					return err
				}
				_, err := queries.conversationFindSelection(ctx, selection.Repo, selection.Hash, "needle", stage == "root-selection")
				return err
			})
		})
	}
}

func TestConversationFindOwnerPreparationPrimitiveKeepsCommandPendingSemantics(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, candidate := range []struct {
			state, reason string
			failure       error
		}{
			{"waiting", "", nil},
			{"running", "", nil},
			{"retrying", "", nil},
			{"rejected", "source_unavailable", domain.ErrNotFound},
			{"rejected", "invalid_document", domain.ErrIntegrity},
			{"waiting", "byte_backfill_required", nil},
		} {
			test.Run(fmt.Sprintf("%t/%s/%s", legacy, candidate.state, candidate.reason), func(test *testing.T) {
				queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{legacyFindGenerationMessage(domain.RoleUser, "needle")})
				fixture.ready, fixture.readOnly = false, false
				fixture.job = domain.DocRangeIndexJob{State: candidate.state, Reason: candidate.reason}
				err := queries.PrepareConversationFind(inbound.WithSystemActor(context.Background()), fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"})
				if err != candidate.failure || fixture.preparations != 1 || fixture.enqueued != 1 || fixture.pages != 0 || fixture.chunks != 0 || fixture.active != 0 {
					test.Fatal("explicit preparation changed command pending/errors or ran workers", err, fixture)
				}
			})
		}
		queries, fixture := newConversationFindFixture(test, legacy, []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("x", docViewBytes+1)}})
		fixture.readOnly = false
		if err := queries.PrepareConversationFind(inbound.WithSystemActor(context.Background()), fixture.repo, fixture.hash, domain.ConversationFindRequest{Query: "needle"}); err != nil ||
			fixture.displays != 1 || fixture.enqueued != 1 || fixture.preparations != 1 || fixture.active != 0 {
			test.Fatal("shared actual owner did not preserve explicit display preparation demand", legacy, err, fixture)
		}
	}
}
