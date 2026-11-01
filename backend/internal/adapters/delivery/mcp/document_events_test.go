package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func setPageDocumentEventQueries(test *testing.T, server *Server, repo domain.ContentHash, backend pageBackend) {
	test.Helper()
	setContextQueryTestPort(test, server, backend)
	ctx := systemTestContext()
	storage := store.NewFSStore(test.TempDir())
	for position, snapshot := range backend.snapshots[repo] {
		document, found := backend.docs[snapshot.DocHash]
		if !found {
			test.Fatal("missing explicit document query fixture", snapshot.DocHash)
		}
		if document.CIR.Events == nil {
			document.CIR.Events = []domain.CIREvent{}
		}
		canonical, err := domain.CanonicalBytes(document.CIR)
		if err != nil {
			test.Fatal(err)
		}
		document.Hash = domain.HashContent(canonical)
		if _, err := storage.PutDoc(ctx, repo, document); err != nil {
			test.Fatal(err)
		}
		backend.snapshots[repo][position].DocHash = document.Hash
	}
	queries, err := app.NewService(storage, storage, nil, nil, nil).DocumentEventQueries()
	if err != nil {
		test.Fatal(err)
	}
	if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryCompatibility); err != nil {
		test.Fatal(err)
	}
}

type documentEventQueriesProbe struct {
	err       error
	read      func(context.Context, domain.ContentHash) error
	events    int
	fragments int
	searches  int
}

func (queries *documentEventQueriesProbe) ReadDocEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	queries.events++
	return domain.DocEventPage{}, queries.err
}

func (queries *documentEventQueriesProbe) ReadDocFragments(ctx context.Context, repo, hash domain.ContentHash, index, offset, limit, budget int) (domain.DocFragmentPage, error) {
	queries.fragments++
	if queries.read != nil {
		return domain.DocFragmentPage{}, queries.read(ctx, repo)
	}
	return domain.DocFragmentPage{}, queries.err
}

func (queries *documentEventQueriesProbe) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	queries.searches++
	if queries.read != nil {
		return nil, queries.read(ctx, repo)
	}
	return nil, queries.err
}

type documentEventBackendTrap struct {
	pageBackend
	documentEventQueriesProbe
	whole int
}

func (backend *documentEventBackendTrap) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	backend.whole++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func documentEventRequest(test *testing.T, ctx context.Context, token, tool string, arguments toolArgs) *http.Request {
	test.Helper()
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": arguments})
	if err != nil {
		test.Fatal(err)
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params})
	if err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))).WithContext(ctx)
	for key, value := range remoteMCPHeaders(token) {
		request.Header.Set(key, value)
	}
	return request
}

func TestSetDocumentEventQueriesRejectsNilWithoutReplacingPort(test *testing.T) {
	var typedNil *documentEventQueriesProbe
	for _, installed := range []bool{false, true} {
		server := &Server{}
		valid := &documentEventQueriesProbe{}
		if installed {
			if err := server.SetDocumentEventQueries(valid, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
		}
		for _, rejected := range []inbound.DocumentEventQueries{nil, typedNil} {
			if err := server.SetDocumentEventQueries(rejected, inbound.DocumentQueryCompatibility); !errors.Is(err, domain.ErrValidation) {
				test.Fatal("nil document event dependency was accepted", installed, err)
			}
			if installed && server.documentEvents != valid || !installed && server.documentEvents != nil {
				test.Fatal("rejected dependency changed the installed query port", installed)
			}
		}
	}
}

func TestSetDocumentEventQueriesRejectsUnknownProfile(test *testing.T) {
	server := &Server{}
	queries := &documentEventQueriesProbe{}
	if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryVerifiedRoots); err != nil {
		test.Fatal(err)
	}
	for _, profile := range []inbound.DocumentQueryProfile{0, 255} {
		if err := server.SetDocumentEventQueries(&documentEventQueriesProbe{}, profile); !errors.Is(err, domain.ErrValidation) ||
			server.documentEvents != queries || server.documentQueryProfile != inbound.DocumentQueryVerifiedRoots {
			test.Fatal("invalid profile replaced the pinned read owner or authorization mode", profile, err)
		}
	}
}

func TestMCPDocumentEventsMissingPortDoesNotDiscoverBackend(test *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1} {
		test.Run(string(identity), func(test *testing.T) {
			repo := domain.Repo{ID: pageHash(1)}
			snapshot := domain.Snapshot{ID: pageHash(2), DocHash: pageHash(2), RepoID: repo.ID, DocIdentity: identity}
			backend := &documentEventBackendTrap{pageBackend: pageBackend{fakeContextBackend: fakeContextBackend{
				snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {snapshot}}}}, documentEventQueriesProbe: documentEventQueriesProbe{err: domain.ErrIntegrity}}
			server, err := NewServerWithOrigins(backend, nil, store.NewFSStore(test.TempDir()), "https://events.example.test", "https://events.example.test")
			if err != nil {
				test.Fatal(err)
			}
			for _, call := range []func() (string, error){
				func() (string, error) {
					return server.eventPage(systemTestContext(), repo, toolArgs{Ref: string(snapshot.ID)})
				},
				func() (string, error) { return server.searchPage(systemTestContext(), repo, toolArgs{Query: "needle"}) },
			} {
				body, err := call()
				if body != "" || !errors.Is(err, domain.ErrAgentHistoryUnavailable) || !strings.Contains(err.Error(), "document event queries are not configured") {
					test.Fatal("missing query port silently fell back to the context backend", body, err)
				}
			}
			if backend.whole != 0 || backend.documentEventQueriesProbe.events != 0 || backend.fragments != 0 || backend.searches != 0 {
				test.Fatal("constructor or read discovered an implicit document query capability", backend)
			}
		})
	}
}

func TestMCPDocumentEventsUseSeparateCoreQueries(test *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	snapshot := domain.Snapshot{ID: pageHash(2), DocHash: pageHash(2), RepoID: repo.ID}
	document := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 7, Role: domain.RoleUser,
		Blocks: []domain.ContentBlock{{Type: "text", Text: "needle \ud55c🙂\n"}}}}}}
	backend := &documentEventBackendTrap{pageBackend: pageBackend{fakeContextBackend: fakeContextBackend{
		snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {snapshot}}}, docs: map[domain.ContentHash]domain.SessionDoc{snapshot.DocHash: document}},
		documentEventQueriesProbe: documentEventQueriesProbe{err: domain.ErrIntegrity}}
	server := &Server{context: backend}
	setPageDocumentEventQueries(test, server, repo.ID, backend.pageBackend)
	raw, err := server.eventPage(systemTestContext(), repo, toolArgs{Ref: string(snapshot.ID)})
	if err != nil {
		test.Fatal(err)
	}
	var page struct {
		Snapshot  domain.ContentHash `json:"snapshot_id"`
		Doc       domain.ContentHash `json:"doc_hash"`
		Total     int                `json:"total_events"`
		Fragments []eventFragment    `json:"fragments"`
		Next      string             `json:"next_cursor"`
	}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	var encoded struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &encoded); err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &page); err != nil || page.Snapshot != snapshot.ID || page.Doc != domain.HashContent(canonical) || page.Total != 1 ||
		len(page.Fragments) != 1 || page.Fragments[0].Index != 0 || page.Fragments[0].Offset != 0 || !page.Fragments[0].Complete ||
		page.Fragments[0].JSON != string(encoded.Events[0]) || page.Next != "" {
		test.Fatal("separate core query changed canonical fragments or wire identity", raw, err)
	}
	raw, err = server.searchPage(systemTestContext(), repo, toolArgs{Query: "needle", Limit: 2})
	if err != nil {
		test.Fatal(err)
	}
	var search struct {
		Hits []struct {
			Snapshot domain.ContentHash `json:"snapshot_id"`
			Seq      int                `json:"seq"`
			Snippet  string             `json:"snippet"`
		} `json:"hits"`
		Next string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(raw), &search); err != nil || len(search.Hits) != 1 || search.Hits[0].Snapshot != snapshot.ID ||
		search.Hits[0].Seq != 7 || !strings.Contains(search.Hits[0].Snippet, "needle \ud55c🙂") || search.Next != "" {
		test.Fatal("separate core search was not used", raw, err)
	}
	if backend.whole != 0 || backend.documentEventQueriesProbe.events != 0 || backend.fragments != 0 || backend.searches != 0 {
		test.Fatal("explicit query read escaped into context backend capabilities", backend)
	}
}

func TestMCPDocumentEventQueryErrorsDoNotFallBack(test *testing.T) {
	for _, expected := range []error{domain.ErrNotFound, domain.ErrIntegrity, domain.ErrUnsupportedDocumentIdentity,
		domain.ErrDocumentIdentityUpgradeRequired, domain.ErrDocRangeIndexPending, domain.ErrAgentHistoryUnavailable,
		domain.ErrCatalogUnsupported, domain.ErrUnauthorized, domain.ErrForbidden, context.Canceled} {
		test.Run(expected.Error(), func(test *testing.T) {
			repo := domain.Repo{ID: pageHash(1)}
			snapshot := domain.Snapshot{ID: pageHash(2), DocHash: pageHash(2), RepoID: repo.ID, DocIdentity: domain.DocumentIdentityRootV1}
			backend := &documentEventBackendTrap{pageBackend: pageBackend{fakeContextBackend: fakeContextBackend{
				snapshots: map[domain.ContentHash][]domain.Snapshot{repo.ID: {snapshot}}}}}
			queries := &documentEventQueriesProbe{err: expected}
			server := &Server{context: backend}
			setContextQueryTestPort(test, server, backend)
			if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			body, err := server.eventPage(systemTestContext(), repo, toolArgs{Ref: string(snapshot.ID)})
			if body != "" || !errors.Is(err, expected) {
				test.Fatal("fragment query error was hidden", body, err)
			}
			body, err = server.searchPage(systemTestContext(), repo, toolArgs{Query: "needle"})
			if body != "" || !errors.Is(err, expected) {
				test.Fatal("search query error was hidden", body, err)
			}
			if queries.events != 0 || queries.fragments != 1 || queries.searches != 1 || backend.whole != 0 || backend.documentEventQueriesProbe.events != 0 || backend.fragments != 0 || backend.searches != 0 {
				test.Fatal("failed explicit query retried through a different capability", queries, backend)
			}
		})
	}
}
