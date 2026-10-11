//go:build postgres

package http

import (
	"context"
	"encoding/json"
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
)

func TestPGConditionalAppendHTTPRevocationDuringBody(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated test DSN required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	revoker := app.NewIdentityService(auth.NewDevVerifier(), peer)
	owner, ownerSession, err := identity.Login(ctx, "dev:"+domain.NewID("cas-owner-")+"@example.test", "conditional owner")
	if err != nil {
		t.Fatal(err)
	}
	member, memberSession, err := identity.Login(ctx, "dev:"+domain.NewID("cas-member-")+"@example.test", "conditional member")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := identity.CreateRepository(ctx, owner, "ConditionalAppend")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: member.ID, Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	snapshot := func(label string) domain.ContentHash {
		t.Helper()
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: repository.ID + label}}}}}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err := storage.PutDoc(ctx, repo.ID, doc); err != nil {
			t.Fatal(err)
		}
		if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo.ID, DocHash: doc.Hash, Provider: domain.ProviderUnknown, Fidelity: domain.FidelityFull}); err != nil {
			t.Fatal(err)
		}
		return doc.Hash
	}
	before, target := snapshot("current"), snapshot("incoming")
	ref := domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: domain.LegacyContextBranchID(string(repo.ID), "main"), Target: before}
	if err := storage.CompareAndSwapRef(ctx, repo.ID, ref, ""); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPending(ctx, repo.ID, domain.Pending{RepoID: repo.ID, SessionID: "incoming-session", Branch: "main", Target: target, Provider: domain.ProviderUnknown, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	revision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"branch_id": ref.BranchID, "target": target, "expected_target": before, "expected_revision": map[string]string{"graph": "0", "evidence": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if revision.Graph != 0 || revision.Evidence != 0 {
		t.Fatalf("unexpected fixture revision: %+v", revision)
	}
	state := func() string {
		t.Helper()
		refs, err := storage.ListRefs(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		incoming, err := storage.GetSnapshot(ctx, repo.ID, target)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := storage.ListPendings(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := storage.RepositoryRevision(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		reflog, err := storage.ReadReflog(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal([]any{refs, incoming, pending, revision, reflog})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	initial := state()
	service := app.NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	handler := NewServer(service, identity).Handler()
	body := &pausedWriteBody{reader: strings.NewReader(string(payload)), entered: make(chan struct{}), release: make(chan struct{})}
	path := "/api/v1/repos/" + string(repo.ID) + "/conditional-appends/v1/main"
	request := httptest.NewRequest(http.MethodPost, path, body).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+memberSession.Token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(body.release) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("request did not finish")
		}
	})
	go func() { defer close(done); handler.ServeHTTP(response, request) }()
	select {
	case <-body.entered:
	case <-done:
		t.Fatalf("request rejected before body: %d %s", response.Code, response.Body.String())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := revoker.RemoveMember(ctx, owner.ID, repository.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(body.release) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"forbidden"`) {
		t.Fatalf("revoked member appended: %d %s", response.Code, response.Body.String())
	}
	if after := state(); after != initial {
		t.Fatalf("revoked request mutated repository:\nbefore=%s\nafter=%s", initial, after)
	}
	request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(payload))).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+ownerSession.Token)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid same-plan control failed: %d %s", response.Code, response.Body.String())
	}
	current, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if err != nil || current.Target != target {
		t.Fatalf("authorized control did not append: %+v %v", current, err)
	}
}
