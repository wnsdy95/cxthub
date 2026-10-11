package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type associationStatusMCPBackend struct {
	fakeContextBackend
	requests []domain.GitAssociationStatusRequest
	page     domain.GitAssociationStatusPage
	err      error
}

func (backend *associationStatusMCPBackend) GetGitAssociationStatus(_ context.Context, repo domain.ContentHash, request domain.GitAssociationStatusRequest) (domain.GitAssociationStatusPage, error) {
	backend.requests = append(backend.requests, request)
	page := backend.page
	page.RepoID, page.Kind, page.Filter = repo, request.Kind, request.State
	return page, backend.err
}

func TestGitAssociationStatusMCPBoundedAndSelectionBound(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	backend := &associationStatusMCPBackend{page: domain.GitAssociationStatusPage{
		Items: []domain.GitAssociationStatus{}, CurrentApplicability: "not_evaluated",
		Discovery: domain.GitAssociationDiscoveryStatus{State: "checkpoint_recorded", Version: 9007199254740993},
	}}
	for index := 1; index <= 20; index++ {
		backend.page.Items = append(backend.page.Items, domain.GitAssociationStatus{ID: fmt.Sprintf("%064x", index), Kind: "integration", State: "waiting", SourceBranch: strings.Repeat("source-", 100), DestinationBranch: strings.Repeat("destination-", 100)})
	}
	server := &Server{context: backend}
	args := toolArgs{Repository: string(repo.ID), Limit: 20}
	raw, err := server.gitAssociationStatusPage(systemTestContext(), repo, args)
	if err != nil || len(raw) > pageBytes {
		t.Fatal("unbounded response", len(raw), err)
	}
	var page domain.GitAssociationStatusPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil || len(page.Items) == 0 || len(page.Items) >= 20 || page.NextCursor == "" || page.Discovery.Version != backend.page.Discovery.Version {
		t.Fatalf("lost continuation or precision: %+v %v", page, err)
	}
	args.Cursor = page.NextCursor
	backend.page.Items = nil
	if _, err := server.gitAssociationStatusPage(systemTestContext(), repo, args); err != nil || backend.requests[1].Cursor != page.Items[len(page.Items)-1].ID {
		t.Fatal("byte trimming skipped records", backend.requests, err)
	}
	for _, change := range []string{"repo", "kind", "state", "limit"} {
		selection, selectedRepo := args, repo
		switch change {
		case "repo":
			selectedRepo.ID = pageHash(2)
		case "kind":
			selection.Kind = "memory"
		case "state":
			selection.State = "all"
		case "limit":
			selection.Limit = 21
		}
		if _, err := server.gitAssociationStatusPage(systemTestContext(), selectedRepo, selection); err == nil {
			t.Fatal("accepted invalid continuation", change)
		}
	}
	if len(backend.requests) != 2 {
		t.Fatal("invalid continuation reached service")
	}
	backend.err = domain.ErrIntegrity
	if _, err := server.gitAssociationStatusPage(systemTestContext(), repo, args); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("query error masked", err)
	}
	server.context = fakeContextBackend{}
	if _, err := server.gitAssociationStatusPage(systemTestContext(), repo, args); !errors.Is(err, domain.ErrGitAssociationStatusUnavailable) {
		t.Fatal("unsupported service reported success", err)
	}
}

func TestGitAssociationStatusMCPLegalEscapedBranchFitsSinglePage(t *testing.T) {
	repo := domain.Repo{ID: pageHash(1)}
	branch := strings.Repeat("<>", 512)
	if err := domain.ValidateBranchName(branch); err != nil {
		t.Fatal(err)
	}
	item := domain.GitAssociationStatus{ID: strings.Repeat("a", 64), Kind: "integration", State: "completed", SourceBranch: branch, DestinationBranch: branch,
		SourceBranchID: strings.Repeat("b", 128), DestinationBranchID: strings.Repeat("c", 128), Capture: pageHash(2), OriginalHead: strings.Repeat("d", 64),
		PublicationID: strings.Repeat("e", 32), AdmissionID: strings.Repeat("f", 64), ContributionID: strings.Repeat("1", 64)}
	backend := &associationStatusMCPBackend{page: domain.GitAssociationStatusPage{Items: []domain.GitAssociationStatus{item}, NextCursor: item.ID, CurrentApplicability: "not_evaluated"}}
	server := &Server{context: backend}
	raw, err := server.gitAssociationStatusPage(systemTestContext(), repo, toolArgs{Repository: string(repo.ID), State: "all"})
	if err != nil || len(raw) > pageBytes {
		t.Fatal("valid labels blocked pagination", len(raw), err)
	}
	var page domain.GitAssociationStatusPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil || len(page.Items) != 1 || page.Items[0] != item || page.NextCursor == "" {
		t.Fatal("label bytes or continuation changed", err)
	}
}

func TestGitAssociationStatusMCPUsesRepositoryAuthorization(t *testing.T) {
	ctx := systemTestContext()
	storage := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "status-owner", Email: "status@example.test", Name: "Status", Username: "status-owner"}
	identity := app.NewIdentityService(fixedVerifier{user: user}, storage)
	if _, _, err := identity.Login(ctx, "idp-token", "test"); err != nil {
		t.Fatal(err)
	}
	repository, err := identity.CreateRepository(ctx, user, "Status")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: pageHash(1), RepositoryID: repository.ID}
	backend := &associationStatusMCPBackend{fakeContextBackend: fakeContextBackend{repos: []domain.Repo{repo}}, page: domain.GitAssociationStatusPage{Items: []domain.GitAssociationStatus{}}}
	server, err := NewServer(backend, identity, storage, "https://cxthub.test")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"repository": string(repo.ID)})
	if _, err := server.runTool(ctx, user, "context_link_status", args); err != nil {
		t.Fatal(err)
	}
	if _, err := server.runTool(ctx, domain.User{ID: "outsider"}, "context_link_status", args); err == nil {
		t.Fatal("private diagnostics leaked")
	}
	if len(backend.requests) != 1 {
		t.Fatal("unauthorized query executed")
	}
}
