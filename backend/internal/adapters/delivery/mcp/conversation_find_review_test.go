package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/memorypreparation"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func conversationFindReviewSelection() (*preparedMemoryTestBackend, domain.Repo, toolArgs) {
	backend, repo, args := preparedMemoryTestSetup("main")
	args.Mode, args.ReadVersion, args.CodeCommit, args.Query = "", nil, "", "needle"
	return backend, repo, args
}

func conversationFindReviewPage(hash domain.ContentHash) domain.ConversationFindPage {
	return domain.ConversationFindPage{ReplayCursor: "native-replay", Page: domain.DocFindPage{
		Hash: hash, Projection: domain.DocFindPreparedProjection, Prepared: &domain.DocFindPreparedProgress{},
		Matches: []domain.DocFindMatch{{Preview: strings.Repeat("한🙂\"<>&\n", 1600)}},
	}}
}

func conversationFindReviewRequest(test *testing.T, fixture *memoryAdmissionFixture, ctx context.Context, args toolArgs) *http.Request {
	test.Helper()
	params, err := json.Marshal(map[string]any{"name": "context_find", "arguments": map[string]any{
		"repository": string(fixture.repo.ID), "ref": args.Ref, "query": args.Query, "cursor": args.Cursor,
	}})
	if err != nil {
		test.Fatal(err)
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
	if err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))).WithContext(ctx)
	for key, value := range remoteMCPHeaders(fixture.token) {
		request.Header.Set(key, value)
	}
	return request
}

func conversationFindReviewFailure(test *testing.T, result string, err error, code string) *conversationFindToolError {
	test.Helper()
	var failure *conversationFindToolError
	if result != "" || !errors.As(err, &failure) || failure.Code != code {
		test.Fatalf("result bytes=%d error=%v; want structured %s", len(result), err, code)
	}
	return failure
}

func TestConversationFindReviewRejectsTransportCursorMutationBeforeQuery(test *testing.T) {
	backend, repo, args := conversationFindReviewSelection()
	cursor, err := cursorFor(string(repo.ID), "context_find", args)
	if err != nil {
		test.Fatal(err)
	}
	cursor.Version, cursor.Snapshot, cursor.DocumentCursor = 6, pageHash(2), "native-replay"
	server := &Server{context: backend}
	server.SetConversationFind(conversationFindQueryFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
		test.Fatal("invalid transport cursor reached query port")
		return domain.ConversationFindPage{}, nil
	}), nil)
	for name, mutate := range map[string]func(*pageCursor){
		"repository":            func(value *pageCursor) { value.Repository = string(pageHash(9)) },
		"tool":                  func(value *pageCursor) { value.Tool = "context_fetch" },
		"filter":                func(value *pageCursor) { value.Filter = "forged" },
		"missing snapshot":      func(value *pageCursor) { value.Snapshot = "" },
		"invalid snapshot":      func(value *pageCursor) { value.Snapshot = "not-a-hash" },
		"negative offset":       func(value *pageCursor) { value.Offset = -1 },
		"index":                 func(value *pageCursor) { value.Index = 1 },
		"memory":                func(value *pageCursor) { value.Memory = pageHash(3) },
		"generation":            func(value *pageCursor) { value.Generation = pageHash(3) },
		"projection":            func(value *pageCursor) { value.Projection = pageHash(3) },
		"branch":                func(value *pageCursor) { value.ContextBranch = "main" },
		"effective cursor":      func(value *pageCursor) { value.EffectiveCursor = "foreign" },
		"fragment format":       func(value *pageCursor) { value.FragmentFormat = "foreign" },
		"prepared pending":      func(value *pageCursor) { value.PreparedPending = true },
		"after":                 func(value *pageCursor) { value.After = "foreign" },
		"top":                   func(value *pageCursor) { value.Top = "foreign" },
		"missing fragment hash": func(value *pageCursor) { value.Offset = 1 },
		"missing native replay": func(value *pageCursor) { value.Offset, value.Scan, value.DocumentCursor = 1, pageHash(3), "" },
	} {
		test.Run(name, func(test *testing.T) {
			changed := cursor
			mutate(&changed)
			input := args
			input.Cursor = encodeCursor(changed)
			result, err := server.conversationFindPage(context.Background(), repo, input)
			conversationFindReviewFailure(test, result, err, "validation")
		})
	}
	for _, version := range []int{0, 1, 2, 3, 4, 5, 7} {
		cursor.Version = version
		args.Cursor = encodeCursor(cursor)
		result, err := server.conversationFindPage(context.Background(), repo, args)
		conversationFindReviewFailure(test, result, err, "validation")
	}
	cursor.Version = 6
	args.Cursor = encodeCursor(cursor)
	for _, field := range []string{"query", "ref", "repository"} {
		input := args
		switch field {
		case "query":
			input.Query = "different"
		case "ref":
			input.Ref = "other"
		case "repository":
			input.Repository = "other/repository"
		}
		result, err := server.conversationFindPage(context.Background(), repo, input)
		conversationFindReviewFailure(test, result, err, "validation")
	}
	args.Cursor = strings.Repeat("A", maxConversationFindTransportCursorBytes+1)
	result, err := server.conversationFindPage(context.Background(), repo, args)
	conversationFindReviewFailure(test, result, err, "validation")
	if backend.refReads != 0 || backend.snapshotReads != 0 {
		test.Fatal("invalid cursor triggered initial ref resolution")
	}
}

func TestConversationFindReviewNativeSelectorsFailBeforePreparation(test *testing.T) {
	backend, repo, args := conversationFindReviewSelection()
	root := domain.DocFindSelector{Version: 3, Representation: domain.DocFindPreparedRepresentation,
		Prepared: &domain.DocFindPreparedProgress{}, QueryHash: domain.HashContent([]byte(args.Query)), To: 3,
		Selection: domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: repo.ID, Hash: pageHash(2), Generation: pageHash(3), View: "all", Total: 3, Limit: 1}}
	legacy := domain.DocLegacyFindSelector{Version: 3, Representation: domain.DocFindPreparedRepresentation,
		Prepared: &domain.DocFindPreparedProgress{}, Repo: repo.ID, Hash: pageHash(2), Total: 3, To: 3, QueryHash: root.QueryHash}
	server := &Server{context: backend}
	server.SetConversationFind(app.NewService(nil, nil, nil, nil, nil), nil)
	cursor, err := cursorFor(string(repo.ID), "context_find", args)
	if err != nil {
		test.Fatal(err)
	}
	cursor.Version, cursor.Snapshot = 6, pageHash(2)
	for _, format := range []string{"root", "legacy"} {
		for _, change := range []string{"repository", "hash", "query", "raw version", "display version", "noncanonical", "malformed"} {
			test.Run(format+"/"+change, func(test *testing.T) {
				selectedRoot, selectedLegacy := root, legacy
				switch change {
				case "repository":
					selectedRoot.Selection.Repo, selectedLegacy.Repo = pageHash(8), pageHash(8)
				case "hash":
					selectedRoot.Selection.Hash, selectedLegacy.Hash = pageHash(8), pageHash(8)
				case "query":
					selectedRoot.QueryHash, selectedLegacy.QueryHash = pageHash(8), pageHash(8)
				case "raw version":
					selectedRoot.Version, selectedLegacy.Version = 1, 1
					selectedRoot.Representation, selectedLegacy.Representation = "", ""
					selectedRoot.Prepared, selectedLegacy.Prepared = nil, nil
				case "display version":
					selectedRoot.Version, selectedLegacy.Version = 2, 2
					selectedRoot.Representation, selectedLegacy.Representation = domain.DocFindDisplayRepresentation, domain.DocFindDisplayRepresentation
					selectedRoot.Prepared, selectedLegacy.Prepared = nil, nil
				}
				var native string
				var err error
				if format == "root" {
					native, err = selectedRoot.Encode()
				} else {
					native, err = selectedLegacy.Encode()
				}
				if err != nil {
					test.Fatal("invalid selector fixture", err)
				}
				if change == "noncanonical" {
					raw, err := base64.RawURLEncoding.DecodeString(native)
					if err != nil {
						test.Fatal(err)
					}
					native = base64.RawURLEncoding.EncodeToString(append([]byte(" "), raw...))
				} else if change == "malformed" {
					native = "not-a-native-selector"
				}
				input, changed := args, cursor
				changed.DocumentCursor = native
				input.Cursor = encodeCursor(changed)
				result, err := server.conversationFindPage(context.Background(), repo, input)
				conversationFindReviewFailure(test, result, err, "validation")
			})
		}
	}
}

func TestConversationFindReviewRejectsChangedImmutableFragment(test *testing.T) {
	for _, change := range []string{"content", "generation", "pending", "replay"} {
		test.Run(change, func(test *testing.T) {
			backend, repo, args := conversationFindReviewSelection()
			page := conversationFindReviewPage(pageHash(2))
			calls := 0
			server := &Server{context: backend}
			server.SetConversationFind(conversationFindQueryFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
				calls++
				if calls > 1 {
					switch change {
					case "content":
						page.Page.Matches[0].Preview += "changed"
					case "generation":
						page.Page.Generation = pageHash(8)
					case "pending":
						return domain.ConversationFindPage{}, &domain.ConversationFindNotReady{Cursor: "replacement", Cause: domain.ErrDocDisplayPending}
					case "replay":
						page.ReplayCursor = "replacement"
					}
				}
				return page, nil
			}), conversationFindPreparationFunc(func(context.Context, string, domain.ConversationFindPreparationRequest) error {
				test.Fatal("changed immutable fragment forwarded preparation")
				return nil
			}))
			raw, err := server.conversationFindPage(context.Background(), repo, args)
			var transport struct {
				Next string `json:"next_cursor"`
			}
			if err != nil || json.Unmarshal([]byte(raw), &transport) != nil || transport.Next == "" {
				test.Fatal("fixture did not fragment", err)
			}
			args.Cursor = transport.Next
			raw, err = server.conversationFindPage(context.Background(), repo, args)
			code := "doc_visible_generation_stale"
			if change == "replay" {
				code = "integrity_violation"
			}
			failure := conversationFindReviewFailure(test, raw, err, code)
			failure = server.forwardConversationFindPreparation(context.Background(), "credential", failure)
			if failure.Retryable || failure.RetryCursor != "" {
				test.Fatal("changed fragment retained a durable retry")
			}
		})
	}
}

func TestConversationFindReviewMaximumNestedCursorAndEscapedBounds(test *testing.T) {
	backend, repo, args := conversationFindReviewSelection()
	page := conversationFindReviewPage(pageHash(2))
	page.ReplayCursor = strings.Repeat("A", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes))
	server := &Server{context: backend}
	server.SetConversationFind(conversationFindQueryFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
		return page, nil
	}), nil)
	var assembled strings.Builder
	for fragment := 0; fragment < 200; fragment++ {
		raw, err := server.conversationFindPage(context.Background(), repo, args)
		if err != nil || len(raw) > pageBytes || !utf8.ValidString(raw) {
			test.Fatal("invalid bounded fragment", len(raw), err)
		}
		var transport struct {
			Offset   int                `json:"byte_offset"`
			Fragment string             `json:"json_fragment"`
			Hash     domain.ContentHash `json:"page_hash"`
			Complete bool               `json:"page_complete"`
			Next     string             `json:"next_cursor"`
		}
		if json.Unmarshal([]byte(raw), &transport) != nil || transport.Offset != assembled.Len() || !utf8.ValidString(transport.Fragment) || len(transport.Next) > maxConversationFindTransportCursorBytes {
			test.Fatal("nested cursor or fragment exceeded bounds")
		}
		assembled.WriteString(transport.Fragment)
		if transport.Complete {
			expected, err := json.Marshal(page.Page)
			if err != nil || assembled.String() != string(expected) || transport.Hash != domain.HashContent(expected) || transport.Next != "" {
				test.Fatal("bounded fragments changed immutable page", err)
			}
			return
		}
		if transport.Next == "" {
			test.Fatal("fragment lost maximum-size native continuation")
		}
		args.Cursor = transport.Next
	}
	test.Fatal("maximum-size cursor prevented bounded progress")
}

func TestConversationFindReviewRejectsInvalidReadyEnvelopes(test *testing.T) {
	for name, mutate := range map[string]func(*domain.ConversationFindPage){
		"wrong snapshot":     func(page *domain.ConversationFindPage) { page.Page.Hash = pageHash(9) },
		"raw projection":     func(page *domain.ConversationFindPage) { page.Page.Projection = domain.DocFindProjection },
		"unprepared display": func(page *domain.ConversationFindPage) { page.Page.Projection = domain.DocFindDisplayProjection },
		"missing progress":   func(page *domain.ConversationFindPage) { page.Page.Prepared = nil },
		"missing replay":     func(page *domain.ConversationFindPage) { page.ReplayCursor = "" },
		"oversized replay": func(page *domain.ConversationFindPage) {
			page.ReplayCursor = strings.Repeat("A", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes)+1)
		},
		"empty next": func(page *domain.ConversationFindPage) { page.Page.NextCursor = new(string) },
		"oversized next": func(page *domain.ConversationFindPage) {
			next := strings.Repeat("A", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes)+1)
			page.Page.NextCursor = &next
		},
	} {
		test.Run(name, func(test *testing.T) {
			backend, repo, args := conversationFindReviewSelection()
			page := conversationFindReviewPage(pageHash(2))
			mutate(&page)
			server := &Server{context: backend}
			server.SetConversationFind(conversationFindQueryFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
				return page, nil
			}), nil)
			result, err := server.conversationFindPage(context.Background(), repo, args)
			conversationFindReviewFailure(test, result, err, "integrity_violation")
		})
	}
}

func TestConversationFindReviewRPCCancellationPreservesContext(test *testing.T) {
	fixture := newMemoryAdmissionFixture(test)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queries := 0
	fixture.server.SetConversationFind(conversationFindQueryFunc(func(bound context.Context, repo, _ domain.ContentHash, _ domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
		queries++
		cancel()
		authorize := inbound.DocumentReadAuthorizer(bound)
		if authorize == nil {
			test.Fatal("canceled query lost current authorization callback")
		}
		_, err := authorize(bound, repo)
		if !errors.Is(err, context.Canceled) || !errors.Is(bound.Err(), context.Canceled) {
			test.Fatal("query or admission detached from RPC cancellation", err)
		}
		return domain.ConversationFindPage{}, err
	}), conversationFindPreparationFunc(func(context.Context, string, domain.ConversationFindPreparationRequest) error {
		test.Fatal("canceled read forwarded preparation")
		return nil
	}))
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, conversationFindReviewRequest(test, fixture, ctx, toolArgs{Query: "needle", Ref: string(fixture.snapshot.ID)}))
	text, failed := memoryAdmissionResult(test, response)
	var body struct {
		Error conversationFindToolError `json:"error"`
	}
	if json.Unmarshal([]byte(text), &body) != nil || !failed || body.Error.Code != "canceled" || body.Error.Retryable || body.Error.RetryCursor != "" || queries != 1 {
		test.Fatal("cancellation became accepted work or retry", text, queries)
	}
}

func TestConversationFindReviewRPCFreshAuthorizationCallbacks(test *testing.T) {
	for _, phase := range []string{"admission", "snapshot"} {
		for _, change := range []string{"none", "token", "role", "identity"} {
			test.Run(phase+"/"+change, func(test *testing.T) {
				fixture := newMemoryAdmissionFixture(test)
				identity := &memoryPreparationIdentity{IdentityService: fixture.identity, test: test}
				fixture.server.identity = identity
				mutate := func() {
					switch change {
					case "token":
						if err := fixture.identity.RevokeMCPToken(systemTestContext(), fixture.token, "memory-admission-client"); err != nil {
							test.Fatal(err)
						}
					case "role":
						if err := fixture.identity.RemoveMember(systemTestContext(), fixture.owner.ID, fixture.repository.ID, fixture.reader.ID); err != nil {
							test.Fatal(err)
						}
					case "identity":
						identity.swapUser = true
					}
				}
				fixture.server.SetConversationFind(conversationFindQueryFunc(func(ctx context.Context, repo, hash domain.ContentHash, _ domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
					if phase == "admission" {
						mutate()
					}
					authorize := inbound.DocumentReadAuthorizer(ctx)
					if authorize == nil {
						test.Fatal("query has no current admission callback")
					}
					bound, err := authorize(ctx, repo)
					if err != nil {
						return domain.ConversationFindPage{}, err
					}
					if phase == "snapshot" {
						mutate()
					}
					snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
					if snapshot == nil {
						test.Fatal("query has no fresh snapshot callback")
					}
					bound, err = snapshot(context.WithValue(bound, memoryPreparationSnapshotKey{}, true), repo)
					if err != nil {
						return domain.ConversationFindPage{}, err
					}
					if actor, system := inbound.RepositoryActor(bound); actor != fixture.reader.ID || system {
						test.Fatal("fresh callback elevated repository authority")
					}
					page := conversationFindReviewPage(hash)
					page.Page.Matches = nil
					return page, nil
				}), conversationFindPreparationFunc(func(context.Context, string, domain.ConversationFindPreparationRequest) error {
					test.Fatal("authorization failure forwarded preparation")
					return nil
				}))
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, conversationFindReviewRequest(test, fixture, context.Background(), toolArgs{Query: "needle", Ref: string(fixture.snapshot.ID)}))
				text, failed := memoryAdmissionResult(test, response)
				if failed != (change != "none") || strings.Contains(text, "retry_cursor") {
					test.Fatal("stale callback accepted a read or preparation", text)
				}
				if !reflect.DeepEqual(identity.tokens, []string{fixture.token, fixture.token}) || !reflect.DeepEqual(identity.resources, []string{fixture.server.resource, fixture.server.resource}) {
					test.Fatal("admission callback changed bearer or audience")
				}
				if phase == "snapshot" || change == "none" {
					if !reflect.DeepEqual(identity.snapshotTokens, []string{fixture.token}) || !reflect.DeepEqual(identity.snapshotResource, []string{fixture.server.resource}) {
						test.Fatal("snapshot callback changed bearer or audience")
					}
				}
			})
		}
	}
}

func TestConversationFindReviewNestedMixedPendingNeverForwards(test *testing.T) {
	for _, cause := range []error{domain.ErrUnauthorized, domain.ErrForbidden, domain.ErrIntegrity, context.Canceled, context.DeadlineExceeded, errors.New("private storage detail")} {
		for _, inside := range []bool{false, true} {
			backend, repo, args := conversationFindReviewSelection()
			pending := &domain.ConversationFindNotReady{Cursor: "pinned", Cause: domain.ErrDocDisplayPending}
			mixed := errors.Join(pending, cause)
			if inside {
				pending.Cause = errors.Join(domain.ErrDocDisplayPending, cause)
				mixed = errors.Join(pending)
			}
			server := &Server{context: backend}
			server.SetConversationFind(conversationFindQueryFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
				return domain.ConversationFindPage{}, fmt.Errorf("private wrapper: %w", mixed)
			}), conversationFindPreparationFunc(func(context.Context, string, domain.ConversationFindPreparationRequest) error {
				test.Fatal("nested mixed pending forwarded preparation")
				return nil
			}))
			result, err := server.conversationFindPage(context.Background(), repo, args)
			var failure *conversationFindToolError
			if result != "" || !errors.As(err, &failure) || !errors.Is(failure, cause) {
				test.Fatal("lost terminal failure", err)
			}
			failure = server.forwardConversationFindPreparation(context.Background(), "credential", failure)
			if failure.Code == "conversation_find_not_ready" || failure.RetryCursor != "" || strings.Contains(failure.Error(), "private") {
				test.Fatal("mixed error became accepted preparation or leaked private data", failure)
			}
		}
	}
}

func TestConversationFindReviewMissingPreparationClientNeverClaimsAcceptance(test *testing.T) {
	for _, typedNil := range []bool{false, true} {
		test.Run(fmt.Sprint("typed-nil=", typedNil), func(test *testing.T) {
			fixture := newMemoryAdmissionFixture(test)
			query := conversationFindQueryFunc(func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
				return domain.ConversationFindPage{}, &domain.ConversationFindNotReady{Cause: domain.ErrDocDisplayPending}
			})
			fixture.server.SetConversationFind(query, nil)
			if typedNil {
				var client *memorypreparation.Client
				fixture.server.SetConversationFind(query, client)
			}
			defer func() {
				if recovered := recover(); recovered != nil {
					test.Errorf("missing preparation client panicked instead of failing closed: %v", recovered)
				}
			}()
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, conversationFindReviewRequest(test, fixture, context.Background(), toolArgs{Query: "needle", Ref: string(fixture.snapshot.ID)}))
			text, failed := memoryAdmissionResult(test, response)
			var body struct {
				Error conversationFindToolError `json:"error"`
			}
			if json.Unmarshal([]byte(text), &body) != nil || !failed || body.Error.Code != "conversation_find_preparation_unavailable" || !body.Error.Retryable || body.Error.RetryCursor == "" {
				test.Fatal("missing preparation service claimed durable acceptance or lost pinned retry", text)
			}
		})
	}
}
