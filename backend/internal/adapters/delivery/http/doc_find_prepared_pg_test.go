//go:build postgres

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGDocFindPreparedHTTPQueuesThenContinues(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	test.Cleanup(cancel)
	ctx := inbound.WithDocumentIdentities(inbound.WithSystemActor(parent), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	owner, session, err := identity.Login(ctx, "dev:"+domain.NewID("prepared-find-http-")+"@example.test", "prepared find owner")
	if err != nil {
		test.Fatal(err)
	}
	service := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	service.ConfigureDocumentDisplay(storage)
	queries, err := service.DocumentQueriesForProfile(inbound.DocumentQueryVerifiedRoots)
	if err != nil {
		test.Fatal(err)
	}
	server := NewServer(service, identity)
	if err := server.SetDocumentQueries(queries, inbound.DocumentQueryVerifiedRoots); err != nil {
		test.Fatal(err)
	}
	handler := server.Handler()
	for _, scenario := range []struct {
		legacy, structured bool
		tool               string
		implicit           bool
	}{
		{}, {legacy: true}, {structured: true}, {legacy: true, structured: true},
		{tool: "Edit"}, {legacy: true, tool: "Edit"}, {tool: "Write"}, {legacy: true, tool: "Write"},
		{tool: "Bash"}, {legacy: true, tool: "Bash"}, {tool: "generic"}, {legacy: true, tool: "generic"},
		{legacy: true, implicit: true}, {legacy: true, structured: true, implicit: true},
	} {
		legacy, structured := scenario.legacy, scenario.structured
		name := "root"
		if legacy {
			name = "legacy"
		}
		if structured {
			name += "Structured"
		}
		if scenario.implicit {
			name += "ImplicitV1"
		}
		name += scenario.tool
		test.Run(name, func(test *testing.T) {
			repository, err := identity.CreateRepository(ctx, owner, "PreparedFindHTTP"+name)
			if err != nil {
				test.Fatal(err)
			}
			repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main"}
			if _, err := storage.PutRepo(ctx, repo); err != nil {
				test.Fatal(err)
			}
			prefix := 0
			if structured {
				prefix = len("{\n  \"payload\": \"")
			}
			if scenario.tool != "" {
				prefix = len(scenario.tool + "(…/b/c/d)")
				switch scenario.tool {
				case "Edit":
					prefix += len("\n− ")
				case "Write":
					prefix += len("\n+ ")
				case "Bash":
					prefix += len("\n$ ")
				default:
					prefix += len("\n{\n  \"content\": \"")
				}
			}
			output := strings.Repeat("a", domain.DocDisplayPageTextBytes-2-prefix) + "NEEDLE🙂" + strings.Repeat("x", 700_000) + "NEEDLE🙂" + string(repo.ID)
			var outputValue any = output
			if structured {
				outputValue = map[string]any{"payload": output}
			}
			document := domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo.ID), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
				Events:   []domain.CIREvent{{Kind: domain.EventToolResult, Output: outputValue}},
			}
			if scenario.implicit {
				document.Envelope.CIRVersion = ""
			}
			if scenario.tool != "" {
				input := map[string]any{"file_path": "/a/b/c/d"}
				field := "content"
				if scenario.tool == "Edit" {
					field = "old_string"
				} else if scenario.tool == "Bash" {
					field = "command"
				}
				input[field] = output
				document.Events[0] = domain.CIREvent{Kind: domain.EventToolCall, ToolName: scenario.tool, Input: input}
			}
			var hash domain.ContentHash
			identityKind := domain.DocumentIdentityRootV1
			if legacy {
				canonical, err := domain.CanonicalBytes(document)
				if err != nil {
					test.Fatal(err)
				}
				hash, identityKind = domain.HashContent(canonical), domain.DocumentIdentityLegacy
				if _, err := storage.PutDoc(ctx, repo.ID, domain.SessionDoc{Hash: hash, CIR: document}); err != nil {
					test.Fatal(err)
				}
			} else {
				manifest, chunks, err := domain.ConversationManifestForCIR(document)
				if err != nil {
					test.Fatal(err)
				}
				hash, err = domain.ConversationManifestHash(manifest)
				if err != nil {
					test.Fatal(err)
				}
				canonical, err := domain.CanonicalConversationManifest(manifest)
				if err != nil {
					test.Fatal(err)
				}
				if _, _, err := storage.PutChunks(ctx, repo.ID, chunks); err != nil {
					test.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, hash, canonical); err != nil {
					test.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, repo.ID, hash); err != nil {
					test.Fatal(err)
				}
			}
			if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: hash, RepoID: repo.ID, DocHash: hash, DocIdentity: identityKind, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
				test.Fatal(err)
			}
			query := url.Values{"q": {"needle🙂"}, "representation": {domain.DocFindPreparedRepresentation}, "from": {"0"}, "to": {"1"}}
			if legacy {
				query.Set("legacy", "1")
				query.Set("total", "1")
				query.Set("inherited", "0")
			} else {
				if err := storage.EnsureDocRangeIndex(ctx, repo.ID, hash); err != nil {
					test.Fatal(err)
				}
				for attempt := 0; attempt < 4; attempt++ {
					visible, err := service.ReadDocVisiblePage(ctx, repo.ID, hash, domain.DocVisiblePageRequest{View: "all", Limit: 20})
					if err == nil {
						query.Set("selection", visible.ReplayCursor)
						break
					}
					if !errors.Is(err, domain.ErrDocRangeIndexPending) {
						test.Fatal(err)
					}
					if err := service.ProcessDocRangeJobs(ctx, 1); err != nil {
						test.Fatal(err)
					}
				}
				if query.Get("selection") == "" {
					test.Fatal("visible selection did not become ready")
				}
			}
			read := func(values url.Values, token string) *httptest.ResponseRecorder {
				request := docFindHTTPTestRequest(repo.ID, hash, values.Encode()).WithContext(parent)
				request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
				if token != "" {
					request.Header.Set("Authorization", "Bearer "+token)
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}
			if response := read(query, ""); response.Code != http.StatusUnauthorized {
				test.Fatal("find bypassed authentication", response.Code, response.Body.String())
			}
			pending, completed, pages, workerBatches := false, false, 0, 0
			preRejected := false
			var matches []domain.DocFindMatch
			for attempt := 0; attempt < 150; attempt++ {
				response := read(query, session.Token)
				if response.Code == http.StatusServiceUnavailable {
					var failure struct {
						Error struct{ Code string } `json:"error"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
						test.Fatal(err)
					}
					switch failure.Error.Code {
					case "doc_range_index_pending":
						if err := service.ProcessDocRangeJobs(ctx, 1); err != nil {
							test.Fatal(err)
						}
					case "doc_display_pending":
						pending = true
						if response.Header().Get("Retry-After") != "1" || strings.Contains(response.Body.String(), `"matches"`) {
							test.Fatal("pending response lost retry policy or leaked results", response.Header(), response.Body.String())
						}
						if err := service.ProcessDocDisplayJobs(ctx, 1); structured && !preRejected && errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
							preRejected = true
						} else if err != nil {
							test.Fatal(err)
						}
						workerBatches++
					default:
						test.Fatal("unexpected preparation failure", response.Body.String())
					}
					continue
				}
				var page domain.DocFindPage
				decoder := json.NewDecoder(response.Body)
				decoder.DisallowUnknownFields()
				if response.Code != http.StatusOK {
					test.Fatal("find failed after preparation", response.Code, response.Body.String())
				}
				if err := decoder.Decode(&page); err != nil {
					test.Fatal(err)
				}
				wantProjection := domain.DocFindPreparedProjection
				if legacy {
					wantProjection = domain.DocLegacyFindPreparedProjection
				}
				if page.Projection != wantProjection || page.Prepared == nil || page.Hash != hash || page.Field != 0 ||
					legacy && page.Generation != "" || !legacy && domain.ValidateContentHash(page.Generation) != nil ||
					page.Prepared.Generation != "" && page.Prepared.Generation == page.Generation || response.Header().Get("Retry-After") != "" {
					test.Fatal("prepared response lost profile or separate generations", page)
				}
				pages++
				for _, match := range page.Matches {
					if match.PreparedPage == nil || match.SourceRange == nil || match.SourceRange.From >= match.SourceRange.To || match.Raw || match.Field != 0 ||
						legacy && match.RevealCursor != "" || !legacy && match.RevealCursor == "" {
						test.Fatal("prepared match lost source coordinates without Accept", match)
					}
					matches = append(matches, match)
				}
				if page.NextCursor == nil {
					if page.Index != 1 || page.Byte != 0 || page.Prepared.Page != 0 || page.Prepared.Generation != "" {
						test.Fatal("terminal prepared progress was not reset", page)
					}
					completed = true
					break
				}
				query = url.Values{"q": {"needle🙂"}, "cursor": {*page.NextCursor}, "representation": {domain.DocFindPreparedRepresentation}}
				if legacy {
					query.Set("legacy", "1")
					selector, err := domain.DecodeDocLegacyFindSelector(*page.NextCursor)
					if err != nil || selector.Version != 3 || selector.Prepared == nil {
						test.Fatal("invalid legacy prepared continuation", selector, err)
					}
				} else {
					selector, err := domain.DecodeDocFindSelector(*page.NextCursor)
					if err != nil || selector.Version != 3 || selector.Prepared == nil {
						test.Fatal("invalid root prepared continuation", selector, err)
					}
				}
			}
			if !pending || !completed || workerBatches == 0 || pages < 2 || len(matches) != 2 || matches[0].Offset != domain.DocDisplayPageTextBytes-2 ||
				!structured && scenario.tool == "" && *matches[0].PreparedPage != 1 || matches[1].Offset <= domain.MaxDocFindDisplayBytes || structured != preRejected {
				test.Fatal("HTTP preparation/continuation lost a boundary or large-offset match", pending, completed, workerBatches, pages, matches)
			}
			test.Logf("legacy=%t authenticated_pending=true worker_batches=%d response_pages=%d cross_page_offset=%d late_offset=%d", legacy, workerBatches, pages, matches[0].Offset, matches[1].Offset)
		})
	}
}
