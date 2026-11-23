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
)

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

func TestCatalogCapabilityHTTPGetRepoFS(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	repo := domain.Repo{ID: domain.HashContent([]byte("FS catalog capability")), RepositoryID: "ws_" + strings.Repeat("e", 32)}
	if _, err := st.PutRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	service := app.NewService(st, st, nil, nil)
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
