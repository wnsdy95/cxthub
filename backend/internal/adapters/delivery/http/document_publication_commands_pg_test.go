//go:build postgres

package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type wave58DocumentPublicationPGPoisonFacade struct{ *app.Service }

func (*wave58DocumentPublicationPGPoisonFacade) Commit(context.Context, inbound.CommitInput) (inbound.CommitOutput, error) {
	panic("actual PostgreSQL document publication reached the legacy HTTP Backend")
}

func (*wave58DocumentPublicationPGPoisonFacade) StoreChunks(context.Context, inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	panic("actual PostgreSQL chunk publication reached the legacy HTTP Backend")
}

func wave58DocumentPublicationPGBody(test *testing.T, route string, repo domain.ContentHash, label string, forged bool) (string, domain.ContentHash) {
	test.Helper()
	document := identityIngressDoc(test, string(repo)+" "+label)
	hash := document.Hash
	var value any
	if route == "objects" {
		if forged {
			document.CIR = identityIngressDoc(test, label+" different body").CIR
		}
		value = objectsBody{Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{{
			ID: hash, RepoID: repo, DocHash: hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull,
		}}}
	} else {
		data := []byte(string(repo) + " " + label)
		hash = domain.HashContent(data)
		if forged {
			data = []byte("different chunk body")
		}
		value = chunksBody{Chunks: []inbound.ChunkObject{{Hash: hash, Data: data}}}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return string(raw), hash
}

func TestPGDocumentPublicationHTTPUsesActualCompleteOwner(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("isolated test DSN required")
	}
	ctx, cancel := context.WithTimeout(test.Context(), 45*time.Second)
	test.Cleanup(cancel)
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	revoker := app.NewIdentityService(auth.NewDevVerifier(), peer)
	service := app.NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	commands, err := service.DocumentPublicationCommands()
	if err != nil {
		test.Fatal("actual Complete document publication factory failed", err)
	}
	if owner, actual := commands.(*app.DocumentPublicationCommands); !actual || owner == nil {
		test.Fatalf("Complete factory returned %T instead of the actual owner", commands)
	}
	server := NewServer(&wave58DocumentPublicationPGPoisonFacade{Service: service}, identity)
	if err := server.SetDocumentPublicationCommands(commands); err != nil {
		test.Fatal(err)
	}
	handler := server.Handler()
	for _, route := range []struct {
		name      string
		stored    string
		duplicate string
	}{
		{name: "objects", stored: `{"StoredSnapshots":1,"StoredDocs":1,"DedupedSnapshots":0,"DedupedDocs":0}`, duplicate: `{"StoredSnapshots":0,"StoredDocs":0,"DedupedSnapshots":1,"DedupedDocs":1}`},
		{name: "chunks", stored: `{"stored":1,"deduped":0}`, duplicate: `{"stored":0,"deduped":1}`},
	} {
		test.Run(route.name, func(test *testing.T) {
			owner, ownerSession, err := identity.Login(ctx, "dev:"+domain.NewID("publication-owner-")+"@example.test", "publication owner")
			if err != nil {
				test.Fatal(err)
			}
			actor, actorSession, err := identity.Login(ctx, "dev:"+domain.NewID("publication-member-")+"@example.test", "publication member")
			if err != nil {
				test.Fatal(err)
			}
			record, err := identity.CreateRepository(ctx, owner, "publication")
			if err != nil {
				test.Fatal(err)
			}
			repo := domain.HashContent([]byte(record.ID))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID, DefaultBranch: "main"}); err != nil {
				test.Fatal(err)
			}
			if err := storage.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: actor.ID, Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
				test.Fatal(err)
			}
			request := func(token string, body io.Reader) *http.Request {
				input := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+string(repo)+"/push/"+route.name, body).WithContext(ctx)
				input.Header.Set("Authorization", "Bearer "+token)
				input.Header.Set("Content-Type", "application/json")
				return input
			}
			assertResponse := func(response *httptest.ResponseRecorder, status int, expected string) {
				test.Helper()
				matches := strings.TrimSpace(response.Body.String()) == expected
				if status != http.StatusOK {
					matches = strings.Contains(response.Body.String(), `"code":"`+expected+`"`)
				}
				if response.Code != status || !matches {
					test.Fatal("actual owner HTTP result changed", response.Code, response.Body, status, expected)
				}
			}
			assertStored := func(hash domain.ContentHash, expected bool) {
				test.Helper()
				var present []domain.ContentHash
				var failure error
				if route.name == "objects" {
					present, failure = storage.HasDocs(ctx, repo, []domain.ContentHash{hash})
					snapshot, snapshotErr := storage.GetSnapshot(ctx, repo, hash)
					if expected && (snapshotErr != nil || snapshot.DocHash != hash) || !expected && !errors.Is(snapshotErr, domain.ErrNotFound) {
						test.Fatal("actual owner snapshot persistence changed", snapshot, snapshotErr, expected)
					}
				} else {
					present, failure = storage.HasChunks(ctx, repo, []domain.ContentHash{hash})
				}
				if failure != nil || expected && (len(present) != 1 || present[0] != hash) || !expected && len(present) != 0 {
					test.Fatal("actual owner object persistence changed", present, failure, expected)
				}
			}
			payload, hash := wave58DocumentPublicationPGBody(test, route.name, repo, "accepted", false)
			for _, expected := range []string{route.stored, route.duplicate} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request(actorSession.Token, strings.NewReader(payload)))
				assertResponse(response, http.StatusOK, expected)
				assertStored(hash, true)
			}
			for _, failure := range []struct {
				name   string
				status int
				code   string
			}{
				{name: "malformed", status: http.StatusBadRequest, code: "bad_request"},
				{name: "integrity", status: http.StatusUnprocessableEntity, code: "integrity_violation"},
			} {
				body, rejectedHash := wave58DocumentPublicationPGBody(test, route.name, repo, failure.name, failure.name == "integrity")
				if failure.name == "malformed" {
					body = `{"`
				}
				before, err := storage.RepositoryRevision(ctx, repo)
				if err != nil {
					test.Fatal(err)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request(actorSession.Token, strings.NewReader(body)))
				assertResponse(response, failure.status, failure.code)
				assertStored(rejectedHash, false)
				if after, err := storage.RepositoryRevision(ctx, repo); err != nil || after != before {
					test.Fatal("rejected publication changed the graph revision", before, after, err)
				}
			}
			pending, pendingHash := wave58DocumentPublicationPGBody(test, route.name, repo, "revoked during body", false)
			body := &pausedWriteBody{reader: strings.NewReader(pending), entered: make(chan struct{}), release: make(chan struct{})}
			response := httptest.NewRecorder()
			done := make(chan struct{})
			var unblock sync.Once
			finish := func() {
				unblock.Do(func() { close(body.release) })
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					test.Fatal("publication request did not finish after releasing its body")
				}
			}
			test.Cleanup(finish)
			go func() {
				defer close(done)
				handler.ServeHTTP(response, request(actorSession.Token, body))
			}()
			select {
			case <-body.entered:
			case <-done:
				test.Fatal("authorized publication did not reach body decoding", response.Code, response.Body)
			case <-ctx.Done():
				test.Fatal("publication body barrier timed out", ctx.Err())
			}
			if err := revoker.RemoveMember(ctx, owner.ID, record.ID, actor.ID); err != nil {
				test.Fatal(err)
			}
			before, err := storage.RepositoryRevision(ctx, repo)
			if err != nil {
				test.Fatal(err)
			}
			denied := httptest.NewRecorder()
			handler.ServeHTTP(denied, request(actorSession.Token, strings.NewReader(pending)))
			assertResponse(denied, http.StatusForbidden, "forbidden")
			finish()
			assertResponse(response, http.StatusForbidden, "forbidden")
			assertStored(pendingHash, false)
			if after, err := storage.RepositoryRevision(ctx, repo); err != nil || after != before {
				test.Fatal("revoked publication changed the graph revision", before, after, err)
			}
			accepted := httptest.NewRecorder()
			handler.ServeHTTP(accepted, request(ownerSession.Token, strings.NewReader(pending)))
			assertResponse(accepted, http.StatusOK, route.stored)
			assertStored(pendingHash, true)
		})
	}
}
