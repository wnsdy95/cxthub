package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type catalogCapabilityHTTPBackend struct {
	Backend
	version int
}

func (b catalogCapabilityHTTPBackend) CatalogVersion() int { return b.version }

func getCatalogCapabilityRepo(t *testing.T, backend Backend, repo domain.Repo, query string) map[string]json.RawMessage {
	t.Helper()
	id := branchPullHTTPIdentity{repo: domain.Repository{ID: repo.RepositoryID}, role: domain.RoleViewer}
	server := httptest.NewServer(NewServer(backend, id).Handler())
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/repos/"+string(repo.ID)+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fixture-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET Repo status = %d", response.StatusCode)
	}
	var wire map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&wire); err != nil {
		t.Fatal(err)
	}
	var got domain.ContentHash
	if err := json.Unmarshal(wire["id"], &got); err != nil || got != repo.ID {
		t.Fatalf("GET Repo id = %q, want %q: %v", got, repo.ID, err)
	}
	return wire
}

func TestCatalogCapabilityHTTPGetRepoOptional(t *testing.T) {
	repo := domain.Repo{ID: domain.HashContent([]byte("catalog capability")), RepositoryID: "ws_" + strings.Repeat("e", 32)}
	base := &branchPullHTTPBackend{repo: repo}
	initialization := initializationHTTPView{initializationHTTPBackend: &initializationHTTPBackend{repo: repo}, pending: true}
	type initializationCapabilities struct {
		catalogCapabilityHTTPBackend
		inbound.RepositoryInitializationQuery
	}
	for _, tc := range []struct {
		name           string
		backend        Backend
		want           string
		initialization bool
	}{
		{"legacy", base, "", false},
		{"query_only", struct {
			Backend
			inbound.CatalogQuery
		}{Backend: base}, "", false},
		{"unsupported", catalogCapabilityHTTPBackend{Backend: base, version: 0}, "", false},
		{"supported", catalogCapabilityHTTPBackend{Backend: base, version: 1}, "1", false},
		{"future_version", catalogCapabilityHTTPBackend{Backend: base, version: 2}, "2", false},
		{"initialization_legacy", initialization, "", true},
		{"initialization_unsupported", initializationCapabilities{catalogCapabilityHTTPBackend{Backend: initialization, version: 0}, initialization}, "", true},
		{"initialization_supported", initializationCapabilities{catalogCapabilityHTTPBackend{Backend: initialization, version: 1}, initialization}, "1", true},
		{"initialization_future_version", initializationCapabilities{catalogCapabilityHTTPBackend{Backend: initialization, version: 2}, initialization}, "2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := getCatalogCapabilityRepo(t, tc.backend, repo, "?initial_branch=main")
			if got := string(wire["catalog_version"]); got != tc.want {
				t.Fatalf("catalog_version = %q, want %q", got, tc.want)
			}
			if tc.initialization {
				if string(wire["initial_anchor_available"]) != "true" || len(wire["branch_pull_version"]) != 0 {
					t.Fatalf("initialization capabilities changed: %s", wire)
				}
			} else if string(wire["branch_pull_version"]) != "1" || len(wire["initial_anchor_available"]) != 0 {
				t.Fatalf("branch pull capability changed: %s", wire)
			}
			if raw := wire["context_protocol"]; len(raw) != 0 && string(raw) != "0" {
				t.Fatalf("catalog capability changed context_protocol: %s", raw)
			}
		})
	}
}

func TestCatalogCapabilityHTTPGetRepoFS(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	repo := domain.Repo{ID: domain.HashContent([]byte("FS catalog capability")), RepositoryID: "ws_" + strings.Repeat("e", 32)}
	if _, err := st.PutRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	service := app.NewService(st, st, nil, nil, nil)
	wire := getCatalogCapabilityRepo(t, service, repo, "")
	if _, present := wire["catalog_version"]; present {
		t.Fatalf("FS advertised catalog support: %s", wire["catalog_version"])
	}
}

func TestCatalogCapabilityIsNotPersistedRepoMetadata(t *testing.T) {
	// Decode even a modern GET response as registration metadata. Runtime
	// discovery fields must not become part of the stored repository shape.
	var repo domain.Repo
	if err := json.Unmarshal([]byte(`{"catalog_version":1}`), &repo); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"catalog_version"`) {
		t.Fatalf("runtime capability persisted: %s", raw)
	}
}
