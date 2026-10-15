package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestProfileActivityPreservesVisibilityAndRepositoryAttribution(t *testing.T) {
	ctx := systemTestContext()
	storage := store.NewFSStore(t.TempDir())
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	ownerToken, collaboratorToken := "dev:activity-owner@example.com:Owner", "dev:activity-collaborator@example.com:Collaborator"
	owner, err := identity.Authenticate(ctx, ownerToken)
	if err != nil {
		t.Fatal(err)
	}
	collaborator, err := identity.Authenticate(ctx, collaboratorToken)
	if err != nil {
		t.Fatal(err)
	}
	organization, err := identity.CreateOrganization(ctx, owner, "Activity organization", "activity-org")
	if err != nil {
		t.Fatal(err)
	}
	service := app.NewService(storage, storage, nil, nil, storage)
	server := httptest.NewServer(NewServer(service, identity).Handler())
	defer server.Close()
	var public domain.Repository
	for _, name := range []string{"public", "private", "membership-only", "organization"} {
		repository := domain.Repository{ID: domain.NewID("ws_"), Name: name, Slug: name, OwnerID: owner.ID, OwnerUsername: owner.Username,
			Visibility: domain.VisibilityPublic, CreatedAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)}
		if name == "private" {
			repository.Visibility = domain.VisibilityPrivate
		}
		if name == "membership-only" {
			repository.OwnerID, repository.OwnerUsername = collaborator.ID, collaborator.Username
		}
		if name == "organization" {
			repository.OwnerNamespaceID = organization.NamespaceID
		}
		if err := storage.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
		for _, user := range []domain.User{owner, collaborator} {
			if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: user.ID, Role: domain.RoleMember}); err != nil {
				t.Fatal(err)
			}
		}
		if name == "public" {
			public = repository
		}
		repo := domain.HashContent([]byte(repository.ID))
		if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID}); err != nil {
			t.Fatal(err)
		}
		snapshot := domain.HashContent([]byte(name + " capture"))
		if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: snapshot, RepoID: repo, DocHash: snapshot, Branch: "main",
			CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Author: domain.TeamIdentity{Name: collaborator.Name, Email: collaborator.Email}}); err != nil {
			t.Fatal(err)
		}
		publication := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), Branch: "main", BranchID: "main",
			Kind: "publish", Source: snapshot, Target: snapshot, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
		if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
			t.Fatal(err)
		}
	}
	assertProfile := func(token string, want int) {
		t.Helper()
		base := server.URL + "/api/v1/public/users/" + owner.Username
		var calendar struct {
			Total int `json:"total"`
			Days  []struct {
				Date  string `json:"date"`
				Count int    `json:"count"`
			} `json:"days"`
		}
		if code := doJSONAs(t, token, http.MethodGet, base+"/contributions", nil, &calendar); code != http.StatusOK {
			t.Fatalf("contributions status = %d", code)
		}
		if calendar.Total != want || calendar.Days == nil || (want > 0 && (len(calendar.Days) != 1 || calendar.Days[0].Date != "2026-09-01" || calendar.Days[0].Count != want)) {
			t.Fatalf("calendar = %+v; want total %d", calendar, want)
		}
		var activity struct {
			Months []domain.ActivityMonth `json:"months"`
		}
		if code := doJSONAs(t, token, http.MethodGet, base+"/activity", nil, &activity); code != http.StatusOK {
			t.Fatalf("activity status = %d", code)
		}
		if activity.Months == nil {
			t.Fatal("months must be an array")
		}
		total, created := 0, 0
		for _, month := range activity.Months {
			total += month.CommitTotal
			created += len(month.Created)
			if month.CommitRepos == nil || month.Created == nil {
				t.Fatalf("null collections: %+v", month)
			}
			for _, repository := range month.CommitRepos {
				if repository.Name != "public" && !(token == ownerToken && repository.Name == "private") {
					t.Fatalf("out-of-scope repository: %+v", repository)
				}
			}
		}
		if total != want || created != want {
			t.Fatalf("activity = %+v; want commits and creations %d", activity, want)
		}
	}
	assertProfile("", 1)
	assertProfile(collaboratorToken, 1)
	assertProfile(ownerToken, 2)
	public.Visibility = domain.VisibilityPrivate
	if err := storage.CreateRepository(ctx, public); err != nil {
		t.Fatal(err)
	}
	assertProfile("", 0)
	assertProfile(ownerToken, 2)
}
