//go:build postgres

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/preparation"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/memorypreparation"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func publishConversationFindPG(test *testing.T, ctx context.Context, storage *store.PostgresStore, writer *app.Service, repo domain.Repo, document domain.CIRDocument, legacy bool) domain.Snapshot {
	test.Helper()
	identity := domain.DocumentIdentityLegacy
	var hash domain.ContentHash
	if legacy {
		raw, err := domain.CanonicalBytes(document)
		if err != nil {
			test.Fatal(err)
		}
		hash = domain.HashContent(raw)
		if _, err := storage.PutDoc(ctx, repo.ID, domain.SessionDoc{Hash: hash, CIR: document}); err != nil {
			test.Fatal(err)
		}
	} else {
		if err := storage.RequireDocumentIdentity(ctx, repo.ID, domain.DocumentIdentityRootV1); err != nil {
			test.Fatal(err)
		}
		manifest, chunks, err := domain.ConversationManifestForCIR(document)
		if err != nil {
			test.Fatal(err)
		}
		hash, err = domain.ConversationManifestHash(manifest)
		if err != nil {
			test.Fatal(err)
		}
		raw, err := domain.CanonicalConversationManifest(manifest)
		if err != nil {
			test.Fatal(err)
		}
		if _, _, err := storage.PutChunks(ctx, repo.ID, chunks); err != nil {
			test.Fatal(err)
		}
		job, err := domain.NewDocFinalizationJobForRepresentation(repo.ID, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, time.Now().UTC())
		if err != nil {
			test.Fatal(err)
		}
		if _, err := storage.EnqueueDocJob(ctx, job); err != nil {
			test.Fatal(err)
		}
		if err := writer.ProcessDocFinalizations(ctx, 1); err != nil {
			test.Fatal(err)
		}
		identity = domain.DocumentIdentityRootV1
	}
	snapshot := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo.ID, DocIdentity: identity, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	if err := storage.PutSnapshot(ctx, snapshot); err != nil {
		test.Fatal(err)
	}
	return snapshot
}

func TestPGConversationFindMCPPreparationRoundTrip(test *testing.T) {
	for _, scenario := range []string{"root", "legacy", "implicit_legacy", "outage", "acknowledgment_lost", "revoked", "reasoning_root", "reasoning_legacy", "message_root", "message_legacy", "summary_root", "summary_legacy", "seed_root", "seed_legacy",
		"tool_edit_root", "tool_edit_legacy", "tool_write_root", "tool_write_legacy", "tool_bash_root", "tool_bash_legacy", "tool_generic_root", "tool_generic_legacy"} {
		test.Run(scenario, func(test *testing.T) {
			legacy := scenario == "legacy" || strings.HasSuffix(scenario, "_legacy")
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			test.Cleanup(cancel)
			dsn := rootProjectionIsolatedDSN(test, ctx)
			storage, err := store.NewPostgresStore(ctx, dsn)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(storage.Close)
			if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
				test.Fatal(err)
			}
			sql, err := pgx.Connect(ctx, dsn)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() { _ = sql.Close(context.Background()) })
			identities := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
			system := outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), identities), identities, identities)
			identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
			suffix := fmt.Sprintf("conversation-find-%d", time.Now().UnixNano())
			owner, _, err := identity.Login(ctx, "dev:"+suffix+"@example.test:Find Owner", "find test")
			if err != nil {
				test.Fatal(err)
			}
			record, err := identity.CreateRepository(system, owner, "Find")
			if err != nil {
				test.Fatal(err)
			}
			repo := domain.Repo{ID: domain.HashContent([]byte(record.ID)), RepositoryID: record.ID, DefaultBranch: "main"}
			if _, err := storage.PutRepo(system, repo); err != nil {
				test.Fatal(err)
			}
			countDisplayJobs := func() int {
				var count int
				if err := sql.QueryRow(ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1`, repo.ID).Scan(&count); err != nil {
					test.Fatal(err)
				}
				return count
			}
			writer := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
			writer.ConfigureDocumentDisplay(storage)
			if err := writer.ConfigureConversationRootPublication(true); err != nil {
				test.Fatal(err)
			}
			reader := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
			reader.ConfigureDocumentDisplayReads(storage)
			output := strings.Repeat("x", domain.DocDisplayPageTextBytes-2) + "NEEDLE" + strings.Repeat("y", 700_000) + "NEEDLE"
			document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, SessionOriginID: suffix}, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}}
			if scenario == "implicit_legacy" {
				document.Envelope.CIRVersion = ""
			}
			if strings.HasPrefix(scenario, "tool_") {
				name := strings.Split(scenario, "_")[1]
				name = map[string]string{"edit": "Edit", "write": "Write", "bash": "Bash", "generic": "generic"}[name]
				field := "content"
				if name == "Edit" {
					field = "old_string"
				} else if name == "Bash" {
					field = "command"
				}
				document.Events = []domain.CIREvent{{Kind: domain.EventToolCall, ToolName: name, CallID: suffix, Input: map[string]any{"file_path": "/tmp/src/fixture", field: output}}}
			}
			if strings.HasPrefix(scenario, "reasoning_") {
				output = strings.Repeat("x", domain.DocDisplayPageTextBytes-2) + "NEEDLE" + strings.Repeat("y", 180_000) + "NEEDLE"
				document.Events = []domain.CIREvent{{Kind: domain.EventReasoning, RedactedSummary: "**" + output + "** [label](https://hidden-needle.invalid)",
					Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: suffix + strings.Repeat("opaque NEEDLE ", 10_000)}}}
			}
			if strings.HasPrefix(scenario, "message_") || strings.HasPrefix(scenario, "summary_") || strings.HasPrefix(scenario, "seed_") {
				document.Envelope.CIRVersion = "2"
				prefix := "public "
				if strings.HasPrefix(scenario, "seed_") {
					prefix = "[cxt seed] Branch-switch context: "
				}
				document.Events = []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleAssistant,
					CompactSummary: strings.HasPrefix(scenario, "summary_"), AgentMessage: true,
					Blocks: []domain.ContentBlock{
						{Type: "text", Text: prefix + strings.Repeat("x", 180_000)},
						{Type: "text", Text: "**NEEDLE"},
						{Type: "text", Text: "NEEDLE** [label](https://hidden-needle.invalid)"},
					}, Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: suffix + strings.Repeat("opaque NEEDLE ", 10_000)}}}
			}
			snapshot := publishConversationFindPG(test, system, storage, writer, repo, document, legacy)
			hash := snapshot.ID
			if err := storage.CompareAndSwapRef(system, repo.ID, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: hash}, ""); err != nil {
				test.Fatal(err)
			}
			const origin = "https://conversation-find.example.test"
			pair, err := identity.IssueMCPTokenPair(system, owner.ID, "find-client", origin+"/mcp")
			if err != nil {
				test.Fatal(err)
			}
			handler, err := preparation.NewConversationFindPreparationHandler(writer, identity, writer, storage, origin, origin)
			if err != nil {
				test.Fatal(err)
			}
			var received atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				attempt := received.Add(1)
				if scenario == "revoked" {
					if err := identity.RevokeMCPToken(request.Context(), pair.AccessToken, "find-client"); err != nil {
						test.Error(err)
					}
				}
				if scenario == "acknowledgment_lost" && attempt == 1 {
					buffered := memoryPreparationBufferedResponse{ResponseRecorder: httptest.NewRecorder(), underlying: response}
					handler.ServeHTTP(buffered, request)
					if buffered.Code != http.StatusAccepted {
						test.Error("lost acknowledgment did not follow durable acceptance", buffered.Code)
					}
					response.Header().Set("Content-Type", "application/json")
					response.WriteHeader(http.StatusServiceUnavailable)
					_, _ = response.Write([]byte(`{"error":{"code":"conversation_find_preparation_unavailable"}}`))
					return
				}
				handler.ServeHTTP(response, request)
			}))
			test.Cleanup(api.Close)
			client, err := memorypreparation.NewClient(api.URL)
			if err != nil {
				test.Fatal(err)
			}
			outage := scenario == "outage"
			server, err := NewServer(reader, identity, storage, origin)
			if err != nil {
				test.Fatal(err)
			}
			server.SetConversationFind(reader, conversationFindPreparationFunc(func(read context.Context, token string, request domain.ConversationFindPreparationRequest) error {
				if outage {
					return domain.ErrConversationFindPreparationUnavailable
				}
				return client.RequestConversationFindPreparation(read, token, request)
			}))
			cursor, matches, changedRef := "", 0, false
			var assembled strings.Builder
			finished := false
			for attempt := 0; attempt < 100; attempt++ {
				arguments := map[string]any{"repository": string(repo.ID), "ref": "main", "query": "needle"}
				if cursor != "" {
					arguments["cursor"] = cursor
				}
				payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "context_find", "arguments": arguments}})
				response := mcpRequest(test, server.Handler(), http.MethodPost, "/mcp", bytes.NewReader(payload), remoteMCPHeaders(pair.AccessToken))
				var envelope prParityRPCResponse
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &envelope) != nil || len(envelope.Result.Content) != 1 {
					test.Fatal("authenticated conversation tool failed", response.Code, response.Body.String())
				}
				text := envelope.Result.Content[0].Text
				if len(text) > pageBytes {
					test.Fatal("MCP exceeded bounded page bytes")
				}
				if envelope.Result.IsError {
					var failure struct {
						Error conversationFindToolError `json:"error"`
					}
					if json.Unmarshal([]byte(text), &failure) != nil {
						test.Fatal("invalid structured tool failure", text)
					}
					if scenario == "revoked" {
						if failure.Error.Code != "unauthenticated" || failure.Error.RetryCursor != "" || received.Load() != 1 || countDisplayJobs() != 0 {
							test.Fatal("API forwarding retained revoked credentials", text)
						}
						return
					}
					if !failure.Error.Retryable || failure.Error.RetryCursor == "" || failure.Error.Code != "conversation_find_not_ready" && failure.Error.Code != "conversation_find_preparation_unavailable" {
						test.Fatal("preparation did not return pinned retry", text)
					}
					if outage && (received.Load() != 0 || countDisplayJobs() != 0) {
						test.Fatal("outage incorrectly reached API")
					}
					if scenario == "acknowledgment_lost" && received.Load() == 1 && countDisplayJobs() != 1 {
						test.Fatal("lost acknowledgment lost the durably accepted preparation")
					}
					outage = false
					cursor = failure.Error.RetryCursor
					if err := writer.ProcessDocRangeJobs(system, 1); err != nil {
						test.Fatal(err)
					}
					if err := writer.ProcessDocDisplayJobs(system, 1); err != nil {
						test.Fatal(err)
					}
					continue
				}
				var transport struct {
					Snapshot domain.ContentHash `json:"snapshot_id"`
					Offset   int                `json:"byte_offset"`
					Fragment string             `json:"json_fragment"`
					Complete bool               `json:"page_complete"`
					Next     string             `json:"next_cursor"`
				}
				if json.Unmarshal([]byte(text), &transport) != nil || transport.Snapshot != hash || transport.Offset != assembled.Len() {
					test.Fatal("MCP lost pinned snapshot or fragment position", text)
				}
				assembled.WriteString(transport.Fragment)
				if transport.Complete {
					var result domain.DocFindPage
					if json.Unmarshal([]byte(assembled.String()), &result) != nil || result.Prepared == nil {
						test.Fatal("native result reconstruction failed")
					}
					for _, match := range result.Matches {
						if match.SourceRange == nil || match.Raw {
							test.Fatal("MCP lost source provenance")
						}
						matches++
					}
					assembled.Reset()
				}
				cursor = transport.Next
				if cursor == "" {
					finished = true
					break
				}
				if !changedRef {
					otherDoc := domain.CIRDocument{Envelope: document.Envelope, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "another immutable conversation"}}}}}
					other := publishConversationFindPG(test, system, storage, writer, repo, otherDoc, legacy)
					if err := storage.CompareAndSwapRef(system, repo.ID, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: other.ID}, hash); err != nil {
						test.Fatal(err)
					}
					changedRef = true
				}
			}
			if !finished || matches != 2 || received.Load() < 1 || !changedRef {
				test.Fatal("authenticated prepared search did not converge", finished, matches, received.Load(), changedRef)
			}
		})
	}
}
