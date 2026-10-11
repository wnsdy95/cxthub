package app_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	httpdelivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestGitProviderObservationSignedWebhook(t *testing.T) {
	ctx := context.Background()
	storage := store.NewFSStore(t.TempDir())
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	owner, _, err := identity.Login(ctx, "dev:provider-observation@example.test:Provider Observation", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := identity.CreateRepository(inbound.WithSystemActor(ctx), owner, "ProviderObservation")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: repository.ID, GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	connection := domain.GitHubConnection{NamespaceID: repository.OwnerNamespaceID, Installation: domain.GitHubInstallation{ID: 789, AccountID: 456, Kind: "User", Login: "acme"}, Enabled: true, Generation: 1, Status: "connected", CheckedAt: time.Now().UTC(), NextSync: time.Now().UTC().Add(time.Hour), Repositories: []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}, Bindings: []domain.GitHubBinding{{RepositoryID: repository.ID, ContextRepoID: repo.ID, ExternalID: 91, GitOrigin: repo.GitRemoteURL}}}
	if err := storage.PutGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	core := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	worker := app.NewGitHubConnections(identity, core, storage, nil)
	server := httpdelivery.NewServer(core, identity)
	server.SetGitHubConnections(worker, "fixture-secret", "https://example.test/connect/github")
	handler := server.Handler()
	for _, testcase := range []struct {
		name                     string
		signed                   bool
		installation, repository int64
		qualified                bool
	}{
		{name: "unsigned", installation: 789, repository: 91},
		{name: "foreign installation", signed: true, installation: 790, repository: 91},
		{name: "foreign repository", signed: true, installation: 789, repository: 92},
		{name: "matching binding", signed: true, installation: 789, repository: 91, qualified: true},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"installation":{"id":%d},"repository":{"id":%d},"ref":"refs/heads/main","after":"%s","provider":{"installation_id":1,"repository_id":2}}`, testcase.installation, testcase.repository, strings.Repeat("a", 40)))
			request := httptest.NewRequest(http.MethodPost, "/api/v1/github/webhook", bytes.NewReader(body))
			request.Header.Set("X-GitHub-Delivery", testcase.name)
			request.Header.Set("X-GitHub-Event", "push")
			if testcase.signed {
				mac := hmac.New(sha256.New, []byte("fixture-secret"))
				mac.Write(body)
				request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			wantStatus := http.StatusUnauthorized
			if testcase.signed {
				wantStatus = http.StatusOK
			}
			if response.Code != wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if err := worker.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			deliveryHash := sha256.Sum256([]byte(testcase.name))
			expected := domain.GitRefObservation{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Source: "github-push", Delivery: hex.EncodeToString(deliveryHash[:]), Ref: "refs/heads/main", After: strings.Repeat("a", 40), Provider: &domain.GitProviderObservation{InstallationID: 789, RepositoryID: 91}}.WithID()
			actual, err := storage.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, expected.ID)
			if testcase.qualified {
				if err != nil || !reflect.DeepEqual(actual, expected) {
					t.Fatalf("signed evidence did not bind verified identity: %+v %v", actual, err)
				}
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("unqualified webhook created proof: %+v %v", actual, err)
			}
			if !testcase.signed {
				if _, err := storage.GetGitHubDelivery(ctx, expected.Delivery); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("unsigned payload persisted: %v", err)
				}
			}
		})
	}
}
