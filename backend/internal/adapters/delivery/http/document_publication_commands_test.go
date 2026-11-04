package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type wave58DocumentPublicationHTTPCommands struct {
	calls        []string
	commit       inbound.CommitInput
	chunks       inbound.StoreChunksInput
	commitResult inbound.CommitOutput
	chunksResult inbound.StoreChunksOutput
	err          error
	observe      func(context.Context, string)
}

var _ inbound.DocumentPublicationCommands = (*wave58DocumentPublicationHTTPCommands)(nil)

func (commands *wave58DocumentPublicationHTTPCommands) Commit(ctx context.Context, input inbound.CommitInput) (inbound.CommitOutput, error) {
	commands.calls = append(commands.calls, "objects")
	commands.commit = input
	if commands.observe != nil {
		commands.observe(ctx, "objects")
	}
	return commands.commitResult, commands.err
}

func (commands *wave58DocumentPublicationHTTPCommands) StoreChunks(ctx context.Context, input inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	commands.calls = append(commands.calls, "chunks")
	commands.chunks = input
	if commands.observe != nil {
		commands.observe(ctx, "chunks")
	}
	return commands.chunksResult, commands.err
}

type wave58DocumentPublicationHTTPBackend struct {
	Backend
	repository domain.Repo
}

func (backend *wave58DocumentPublicationHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repository, nil
}

func (*wave58DocumentPublicationHTTPBackend) Commit(context.Context, inbound.CommitInput) (inbound.CommitOutput, error) {
	panic("document commit reached the legacy HTTP Backend")
}

func (*wave58DocumentPublicationHTTPBackend) StoreChunks(context.Context, inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	panic("chunk publication reached the legacy HTTP Backend")
}

type wave58DocumentPublicationHTTPFixture struct {
	server   *Server
	commands *wave58DocumentPublicationHTTPCommands
	backend  *wave58DocumentPublicationHTTPBackend
	identity *repositoryAssetHTTPIdentity
}

func newWave58DocumentPublicationHTTPFixture(test *testing.T) wave58DocumentPublicationHTTPFixture {
	test.Helper()
	backend := &wave58DocumentPublicationHTTPBackend{repository: domain.Repo{
		ID: domain.HashContent([]byte("wave58 publication HTTP repository")), RepositoryID: "publication-repository",
	}}
	identity := &repositoryAssetHTTPIdentity{repository: domain.Repository{ID: backend.repository.RepositoryID}, role: domain.RoleMember}
	commands := &wave58DocumentPublicationHTTPCommands{
		commitResult: inbound.CommitOutput{StoredSnapshots: 2, StoredDocs: 3, DedupedSnapshots: 5, DedupedDocs: 7},
		chunksResult: inbound.StoreChunksOutput{Stored: 11, Deduped: 13},
	}
	server := NewServer(backend, identity)
	if err := server.SetDocumentPublicationCommands(commands); err != nil {
		test.Fatal(err)
	}
	return wave58DocumentPublicationHTTPFixture{server: server, commands: commands, backend: backend, identity: identity}
}

type wave58DocumentPublicationHTTPRequest struct {
	name     string
	body     string
	commit   inbound.CommitInput
	chunks   inbound.StoreChunksInput
	response string
}

func wave58DocumentPublicationHTTPRequests(test *testing.T) []wave58DocumentPublicationHTTPRequest {
	test.Helper()
	repo := domain.HashContent([]byte("wave58 publication HTTP repository"))
	document := identityIngressDoc(test, "publication transport retains the exact CIR")
	chunk := inbound.ChunkObject{Hash: domain.HashContent([]byte("publication chunk")), Data: []byte("publication chunk")}
	objects := objectsBody{
		Snapshots: []domain.Snapshot{{
			ID: document.Hash, RepoID: domain.HashContent([]byte("client supplied repository")), DocHash: document.Hash,
			Branch: "publication-branch", Parents: []domain.ContentHash{domain.HashContent([]byte("publication parent"))},
			Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: "retain wire metadata",
			CreatedAt: time.Date(2026, time.October, 28, 1, 2, 3, 0, time.UTC),
		}},
		Docs: []domain.SessionDoc{document},
		ChunkedDocs: []inbound.ChunkedDoc{{
			Hash: domain.HashContent([]byte("chunked document")), Format: domain.ChunkFormatV1,
			Envelope: json.RawMessage(`{"cir_version":"1","source_provider":"codex"}`), Chunks: []domain.ContentHash{chunk.Hash},
		}},
		ChunkObjects: []inbound.ChunkObject{chunk},
	}
	objectsJSON, err := json.Marshal(objects)
	if err != nil {
		test.Fatal(err)
	}
	chunks := chunksBody{Chunks: []inbound.ChunkObject{chunk, chunk}}
	chunksJSON, err := json.Marshal(chunks)
	if err != nil {
		test.Fatal(err)
	}
	return []wave58DocumentPublicationHTTPRequest{
		{
			name: "objects", body: string(objectsJSON),
			commit:   inbound.CommitInput{RepoID: repo, Snapshots: objects.Snapshots, Docs: objects.Docs, ChunkedDocs: objects.ChunkedDocs, ChunkObjects: objects.ChunkObjects},
			response: `{"StoredSnapshots":2,"StoredDocs":3,"DedupedSnapshots":5,"DedupedDocs":7}`,
		},
		{
			name: "chunks", body: string(chunksJSON), chunks: inbound.StoreChunksInput{RepoID: repo, Chunks: chunks.Chunks},
			response: `{"stored":11,"deduped":13}`,
		},
	}
}

func (fixture wave58DocumentPublicationHTTPFixture) request(input wave58DocumentPublicationHTTPRequest) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+string(fixture.backend.repository.ID)+"/push/"+input.name, strings.NewReader(input.body))
	request.Header.Set("Authorization", "Bearer publication-session")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
	return request
}

func TestSetDocumentPublicationCommandsRejectsNilWithoutReplacingOwner(test *testing.T) {
	var typedNil *wave58DocumentPublicationHTTPCommands
	for _, rejected := range []struct {
		name     string
		commands inbound.DocumentPublicationCommands
	}{
		{name: "nil"},
		{name: "typed nil", commands: typedNil},
	} {
		test.Run(rejected.name, func(test *testing.T) {
			fixture := newWave58DocumentPublicationHTTPFixture(test)
			for _, server := range []*Server{NewServer(nil, nil), NewServer(fixture.backend, fixture.identity), fixture.server} {
				previous := server.documentPublications
				if err := server.SetDocumentPublicationCommands(rejected.commands); !errors.Is(err, domain.ErrValidation) || server.documentPublications != previous {
					test.Fatal("nil publication commands were accepted or replaced the previous owner", err)
				}
			}
		})
	}
}

func TestDocumentPublicationHTTPUsesInjectedCommands(test *testing.T) {
	for _, input := range wave58DocumentPublicationHTTPRequests(test) {
		test.Run(input.name, func(test *testing.T) {
			fixture := newWave58DocumentPublicationHTTPFixture(test)
			requestKey := struct{ name string }{"document publication request"}
			parent, cancel := context.WithTimeout(context.WithValue(test.Context(), requestKey, "retained"), 30*time.Second)
			defer cancel()
			var commandContext context.Context
			fixture.commands.observe = func(ctx context.Context, command string) {
				commandContext = ctx
				if command != input.name || ctx.Value(requestKey) != "retained" || ctx.Err() != nil || ctx.Done() == nil {
					test.Fatal("publication command lost its request context", command, ctx.Err())
				}
				deadline, present := ctx.Deadline()
				if original, _ := parent.Deadline(); !present || !deadline.Equal(original) {
					test.Fatal("publication command changed the earlier request deadline", deadline, original)
				}
				if actor, system := inbound.RepositoryActor(ctx); actor != "asset-actor" || system {
					test.Fatal("publication command lost the authenticated actor", actor, system)
				}
				if identities := inbound.DocumentIdentities(ctx); !reflect.DeepEqual(identities, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}) {
					test.Fatal("publication command lost the peer identity declaration", identities)
				}
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(input).WithContext(parent))
			var actual, expected any
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				test.Fatal(err)
			}
			if err := json.Unmarshal([]byte(input.response), &expected); err != nil {
				test.Fatal(err)
			}
			if response.Code != http.StatusOK || !reflect.DeepEqual(actual, expected) || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				test.Fatal("publication changed the response contract", response.Code, response.Header(), response.Body)
			}
			if fixture.server.documentPublications != fixture.commands || !reflect.DeepEqual(fixture.commands.calls, []string{input.name}) || !reflect.DeepEqual(fixture.commands.commit, input.commit) || !reflect.DeepEqual(fixture.commands.chunks, input.chunks) {
				test.Fatal("publication did not dispatch exactly one unchanged input to the narrow owner", fixture.commands)
			}
			if commandContext == nil || !errors.Is(commandContext.Err(), context.Canceled) || parent.Err() != nil {
				test.Fatal("publication request scope did not end independently of its parent")
			}
		})
	}
}

func TestDocumentPublicationHTTPPreservesCommandErrors(test *testing.T) {
	for _, input := range wave58DocumentPublicationHTTPRequests(test) {
		for _, failure := range []struct {
			name   string
			err    error
			status int
			code   string
			retry  string
		}{
			{name: "validation", err: domain.ErrValidation, status: http.StatusUnprocessableEntity, code: "validation"},
			{name: "integrity", err: domain.ErrIntegrity, status: http.StatusUnprocessableEntity, code: "integrity_violation"},
			{name: "forbidden", err: domain.ErrForbidden, status: http.StatusForbidden, code: "forbidden"},
			{name: "not found", err: domain.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
			{name: "identity", err: domain.ErrUnsupportedDocumentIdentity, status: http.StatusConflict, code: "unsupported_document_identity"},
			{name: "upgrade", err: domain.ErrDocumentIdentityUpgradeRequired, status: http.StatusConflict, code: "document_identity_upgrade_required"},
			{name: "storage limit", err: domain.ErrStorageLimit, status: http.StatusConflict, code: "storage_limit"},
			{name: "overload", err: domain.ErrResourceOverloaded, status: http.StatusServiceUnavailable, code: "resource_overloaded", retry: "1"},
			{name: "internal", err: errors.New("private publication storage detail"), status: http.StatusInternalServerError, code: "internal"},
		} {
			test.Run(input.name+"/"+failure.name, func(test *testing.T) {
				fixture := newWave58DocumentPublicationHTTPFixture(test)
				fixture.commands.err = fmt.Errorf("publication command: %w", failure.err)
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request(input))
				if response.Code != failure.status || !strings.Contains(response.Body.String(), `"code":"`+failure.code+`"`) || strings.Contains(response.Body.String(), "private publication storage detail") || strings.Contains(response.Body.String(), `"StoredDocs"`) || strings.Contains(response.Body.String(), `"stored"`) || response.Header().Get("Retry-After") != failure.retry || response.Header().Get("Cache-Control") != "no-store" || !reflect.DeepEqual(fixture.commands.calls, []string{input.name}) {
					test.Fatal("publication error changed status, sanitization, retry or dispatch", response.Code, response.Header(), response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestDocumentPublicationHTTPPropagatesCancellation(test *testing.T) {
	for _, input := range wave58DocumentPublicationHTTPRequests(test) {
		test.Run(input.name, func(test *testing.T) {
			fixture := newWave58DocumentPublicationHTTPFixture(test)
			parent, cancel := context.WithCancel(test.Context())
			defer cancel()
			fixture.commands.err = context.Canceled
			fixture.commands.observe = func(ctx context.Context, command string) {
				cancel()
				if command != input.name || !errors.Is(ctx.Err(), context.Canceled) {
					test.Fatal("publication command detached from live request cancellation", command, ctx.Err())
				}
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(input).WithContext(parent))
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"internal"`) || !reflect.DeepEqual(fixture.commands.calls, []string{input.name}) {
				test.Fatal("canceled publication was retried or remapped", response.Code, response.Body, fixture.commands.calls)
			}
		})
	}
}

func TestDocumentPublicationHTTPRejectsBeforeCommandDispatch(test *testing.T) {
	for _, input := range wave58DocumentPublicationHTTPRequests(test) {
		for _, rejected := range []struct {
			name   string
			status int
		}{
			{name: "authentication", status: http.StatusUnauthorized},
			{name: "membership", status: http.StatusForbidden},
			{name: "csrf", status: http.StatusForbidden},
			{name: "content type", status: http.StatusUnsupportedMediaType},
			{name: "malformed JSON", status: http.StatusBadRequest},
			{name: "invalid field", status: http.StatusBadRequest},
			{name: "identity declaration", status: http.StatusBadRequest},
		} {
			test.Run(input.name+"/"+rejected.name, func(test *testing.T) {
				fixture := newWave58DocumentPublicationHTTPFixture(test)
				request := fixture.request(input)
				switch rejected.name {
				case "authentication":
					request.Header.Del("Authorization")
				case "membership":
					fixture.identity.role = domain.RolePuller
				case "csrf":
					request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "publication-session"})
				case "content type":
					request.Header.Set("Content-Type", "text/plain")
				case "malformed JSON":
					request.Body = io.NopCloser(strings.NewReader(`{"`))
				case "invalid field":
					body := `{"docs":"not an array"}`
					if input.name == "chunks" {
						body = `{"chunks":[{"data":"invalid base64!"}]}`
					}
					request.Body = io.NopCloser(strings.NewReader(body))
				case "identity declaration":
					request.Header.Set(documentIdentitiesHeader, "unknown-publication-identity")
				}
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, request)
				if response.Code != rejected.status || len(fixture.commands.calls) != 0 {
					test.Fatal("transport rejection reached a publication command", response.Code, response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestDocumentPublicationHTTPPreservesPublicationDecodeLimits(test *testing.T) {
	for _, rejected := range []struct {
		name   string
		route  string
		body   string
		status int
		code   string
	}{
		{name: "duplicate publication field", route: "objects", body: `{"docs":[],"docs":[]}`, status: http.StatusConflict, code: "unsupported_document_identity"},
		{name: "unknown document identity", route: "objects", body: `{"docs":[{"identity":"future-document-identity"}]}`, status: http.StatusConflict, code: "unsupported_document_identity"},
		{name: "bounded chunk body", route: "chunks", body: `{"chunks":[],"ignored":"` + strings.Repeat("x", inbound.MaxChunkWireJSONBody) + `"}`, status: http.StatusRequestEntityTooLarge, code: "payload_too_large"},
	} {
		test.Run(rejected.name, func(test *testing.T) {
			fixture := newWave58DocumentPublicationHTTPFixture(test)
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(wave58DocumentPublicationHTTPRequest{name: rejected.route, body: rejected.body}))
			if response.Code != rejected.status || !strings.Contains(response.Body.String(), `"code":"`+rejected.code+`"`) || len(fixture.commands.calls) != 0 {
				test.Fatal("publication decoder lost its fail-closed contract", response.Code, response.Body, fixture.commands.calls)
			}
		})
	}
}

type wave58DocumentPublicationHTTPBody struct {
	io.Reader
	observe func()
}

func (body *wave58DocumentPublicationHTTPBody) Read(buffer []byte) (int, error) {
	body.observe()
	return body.Reader.Read(buffer)
}

func (*wave58DocumentPublicationHTTPBody) Close() error { return nil }

type wave58DocumentPublicationHTTPWriter struct {
	*httptest.ResponseRecorder
	observe func()
	writes  int
	err     error
}

func (writer *wave58DocumentPublicationHTTPWriter) Write(body []byte) (int, error) {
	writer.writes++
	writer.observe()
	if writer.err != nil {
		return 0, writer.err
	}
	return writer.ResponseRecorder.Write(body)
}

func wave58DocumentPublicationHTTPAssertBulkHeld(test *testing.T) {
	test.Helper()
	probe, cancel := context.WithTimeout(test.Context(), 20*time.Millisecond)
	defer cancel()
	_, release, err := app.AcquireResources(probe, app.ResourceBulk)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		test.Fatal("publication escaped its existing bulk resource scope", err)
	}
}

func TestDocumentPublicationHTTPRetainsResourcesThroughDecodeCommandAndResponse(test *testing.T) {
	for _, input := range wave58DocumentPublicationHTTPRequests(test) {
		for _, outcome := range []string{"success", "command failure", "decode failure", "response cancellation", "write failure"} {
			test.Run(input.name+"/"+outcome, func(test *testing.T) {
				fixture := newWave58DocumentPublicationHTTPFixture(test)
				parent, cancel := context.WithCancel(test.Context())
				defer cancel()
				request := fixture.request(input).WithContext(parent)
				payload := input.body
				expectedStatus, expectedCalls := http.StatusOK, 1
				if outcome == "decode failure" {
					payload = `{"`
					expectedStatus, expectedCalls = http.StatusBadRequest, 0
				}
				if outcome == "command failure" {
					fixture.commands.err = domain.ErrIntegrity
					expectedStatus = http.StatusUnprocessableEntity
				}
				reads := 0
				request.Body = &wave58DocumentPublicationHTTPBody{Reader: strings.NewReader(payload), observe: func() {
					reads++
					wave58DocumentPublicationHTTPAssertBulkHeld(test)
				}}
				fixture.commands.observe = func(ctx context.Context, command string) {
					wave58DocumentPublicationHTTPAssertBulkHeld(test)
					_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
					if err != nil {
						test.Fatal("publication command could not reuse transport admission", command, err)
					}
					release()
				}
				writer := &wave58DocumentPublicationHTTPWriter{ResponseRecorder: httptest.NewRecorder(), observe: func() {
					wave58DocumentPublicationHTTPAssertBulkHeld(test)
					if outcome == "response cancellation" {
						cancel()
					}
				}}
				if outcome == "write failure" {
					writer.err = io.ErrClosedPipe
				}
				fixture.server.Handler().ServeHTTP(writer, request)
				if writer.Code != expectedStatus || writer.writes != 1 || reads == 0 || len(fixture.commands.calls) != expectedCalls {
					test.Fatal("publication resource scope changed decode, execution or response", writer.Code, writer.Body, reads, writer.writes, fixture.commands.calls)
				}
				probe, stop := context.WithTimeout(test.Context(), time.Second)
				defer stop()
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if err != nil {
					test.Fatal("publication completion leaked bulk admission", err)
				}
				release()
			})
		}
	}
}

func TestDocumentPublicationHTTPResourceRejectionPrecedesDecode(test *testing.T) {
	for _, input := range wave58DocumentPublicationHTTPRequests(test) {
		test.Run(input.name, func(test *testing.T) {
			fixture := newWave58DocumentPublicationHTTPFixture(test)
			_, release, err := app.AcquireResources(test.Context(), app.ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			body := &admissionUnreadBody{}
			request := fixture.request(input)
			request.Body = body
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || !strings.Contains(response.Body.String(), `"code":"resource_overloaded"`) || body.reads != 0 || len(fixture.commands.calls) != 0 {
				test.Fatal("publication decoded or dispatched before resource admission", response.Code, response.Header(), response.Body, body.reads, fixture.commands.calls)
			}
		})
	}
}
