package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type conversationFindQueryFunc func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error)

func (query conversationFindQueryFunc) QueryConversationFind(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
	return query(ctx, repo, hash, request)
}

type conversationFindPreparationFunc func(context.Context, string, domain.ConversationFindPreparationRequest) error

func (requester conversationFindPreparationFunc) RequestConversationFindPreparation(ctx context.Context, token string, request domain.ConversationFindPreparationRequest) error {
	return requester(ctx, token, request)
}

func TestConversationFindFragmentsPinRefAndNativeReplay(test *testing.T) {
	backend, repo, args := preparedMemoryTestSetup("main")
	args.Mode, args.ReadVersion, args.CodeCommit, args.Query = "", nil, "", "needle"
	nextNative := "native-next"
	page := domain.DocFindPage{Projection: domain.DocFindPreparedProjection, Hash: pageHash(2), Prepared: &domain.DocFindPreparedProgress{}, To: 1, Matches: []domain.DocFindMatch{}, NextCursor: &nextNative}
	for index := 0; index < 20; index++ {
		page.Matches = append(page.Matches, domain.DocFindMatch{Index: index, Preview: strings.Repeat("한글🙂\"<>needle", 110), RevealCursor: strings.Repeat("x", 160)})
	}
	server := &Server{context: backend}
	calls := 0
	server.SetConversationFind(conversationFindQueryFunc(func(ctx context.Context, selectedRepo, hash domain.ContentHash, request domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
		calls++
		if !outbound.DocReadOnly(ctx) || selectedRepo != repo.ID || hash != page.Hash || request.Query != args.Query {
			test.Fatal("query lost read-only selection", selectedRepo, hash, request)
		}
		if request.Cursor == nextNative {
			last := page
			last.Matches, last.NextCursor, last.Index = []domain.DocFindMatch{}, nil, 1
			return domain.ConversationFindPage{Page: last, ReplayCursor: nextNative}, nil
		}
		if request.Cursor != "" && request.Cursor != "native-replay" {
			test.Fatal("fragment did not replay the original native selector", request)
		}
		return domain.ConversationFindPage{Page: page, ReplayCursor: "native-replay"}, nil
	}), nil)
	var assembled strings.Builder
	fragments, completed := 0, false
	for attempt := 0; attempt < 150; attempt++ {
		raw, err := server.conversationFindPage(context.Background(), repo, args)
		if err != nil || len(raw) > pageBytes {
			test.Fatal("fragment exceeds transport bound", len(raw), err)
		}
		var transport struct {
			Snapshot domain.ContentHash `json:"snapshot_id"`
			Offset   int                `json:"byte_offset"`
			Fragment string             `json:"json_fragment"`
			Complete bool               `json:"page_complete"`
			Next     string             `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(raw), &transport); err != nil || transport.Snapshot != page.Hash {
			test.Fatal("invalid fragment identity", err)
		}
		backend.denyReads = true
		if completed {
			if !transport.Complete || transport.Next != "" {
				test.Fatal("empty final match page was dropped or kept continuing", raw)
			}
			var last domain.DocFindPage
			if json.Unmarshal([]byte(transport.Fragment), &last) != nil || len(last.Matches) != 0 {
				test.Fatal("empty continuation result malformed")
			}
			break
		}
		if transport.Offset != assembled.Len() {
			test.Fatal("fragment skipped or repeated bytes", transport.Offset, assembled.Len())
		}
		assembled.WriteString(transport.Fragment)
		fragments++
		args.Cursor = transport.Next
		if transport.Complete {
			var restored domain.DocFindPage
			if json.Unmarshal([]byte(assembled.String()), &restored) != nil || len(restored.Matches) != len(page.Matches) || transport.Next == "" {
				test.Fatal("fragment reconstruction lost results or next application page")
			}
			completed = true
		}
	}
	if !completed || fragments < 2 || calls != fragments+1 || backend.refReads != 1 {
		test.Fatal("fragment sequence did not finish once on the pinned ref", calls, fragments, backend.refReads)
	}
}

func TestConversationFindPendingForwardingRequiresPurePending(test *testing.T) {
	backend, repo, args := preparedMemoryTestSetup("main")
	args.Mode, args.ReadVersion, args.CodeCommit, args.Query = "", nil, "", "needle"
	server := &Server{context: backend}
	for _, cause := range []error{domain.ErrDocDisplayPending, errors.Join(domain.ErrDocDisplayPending, domain.ErrIntegrity), errors.Join(domain.ErrDocDisplayPending, errors.New("private provider message"))} {
		calls := 0
		server.SetConversationFind(conversationFindQueryFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
			pending := &domain.ConversationFindNotReady{Cause: domain.ErrDocDisplayPending}
			if cause == domain.ErrDocDisplayPending {
				return domain.ConversationFindPage{}, pending
			}
			return domain.ConversationFindPage{}, errors.Join(pending, cause)
		}), conversationFindPreparationFunc(func(_ context.Context, token string, input domain.ConversationFindPreparationRequest) error {
			calls++
			if token != "credential" || input.RepoID != repo.ID || input.Hash != pageHash(2) || input.Find.Query != args.Query || input.Validate() != nil {
				test.Fatal("preparation changed the authorized request", input)
			}
			return nil
		}))
		_, err := server.conversationFindPage(context.Background(), repo, args)
		var failure *conversationFindToolError
		if !errors.As(err, &failure) {
			test.Fatal(err)
		}
		failure = server.forwardConversationFindPreparation(context.Background(), "credential", failure)
		if cause == domain.ErrDocDisplayPending {
			if calls != 1 || !failure.Retryable || failure.RetryCursor == "" {
				test.Fatal("genuine pending was not forwarded with pinned retry", failure)
			}
		} else if calls != 0 || failure.RetryCursor != "" || failure.Retryable || strings.Contains(failure.Error(), "private") {
			test.Fatal("mixed failure became queued work or leaked private data", failure)
		}
	}
}

func TestConversationFindForwardingFailureIsNotAcceptedPreparation(test *testing.T) {
	request := domain.ConversationFindPreparationRequest{Version: 1, RepoID: pageHash(1), Hash: pageHash(2), Find: domain.ConversationFindRequest{Query: "needle"}}
	for _, cause := range []error{domain.ErrConversationFindPreparationUnavailable, domain.ErrForbidden, domain.ErrIntegrity, domain.ErrDocFindDisplayUnavailable, domain.ErrResourceOverloaded, context.Canceled} {
		server := &Server{conversationFindPreparation: conversationFindPreparationFunc(func(context.Context, string, domain.ConversationFindPreparationRequest) error { return cause })}
		pending := &conversationFindToolError{Code: "conversation_find_not_ready", cause: domain.ErrDocDisplayPending, Retryable: true, RetryCursor: "retry", preparation: &request}
		failure := server.forwardConversationFindPreparation(context.Background(), "credential", pending)
		if failure == pending || failure.Code == pending.Code || !errors.Is(failure, cause) || failure.Retryable && failure.RetryCursor != "retry" || !failure.Retryable && failure.RetryCursor != "" {
			test.Fatal("API rejection became accepted preparation or lost retry semantics", cause, failure)
		}
	}
}
