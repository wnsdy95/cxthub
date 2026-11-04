//go:build postgres

package http

import (
	"context"
	"encoding/json"
	"errors"
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

type repositoryRefPGPoisonBackend struct {
	*repositoryAssetPGPoisonFacade
}

func (*repositoryRefPGPoisonBackend) UpdateRef(context.Context, inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	panic("actual PostgreSQL ref write reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) UpdateRefs(context.Context, inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error) {
	panic("actual PostgreSQL ref batch reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) RecordHistory(context.Context, domain.HistoryEvent) error {
	panic("actual PostgreSQL history write reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) SubmitPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	panic("actual PostgreSQL PR acceptance reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) DeliverPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	panic("actual PostgreSQL PR delivery reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) ListPRPromotions(context.Context, domain.ContentHash) ([]domain.PRPromotionJob, error) {
	panic("actual PostgreSQL PR listing reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) RetryPRPromotion(context.Context, domain.ContentHash, string) error {
	panic("actual PostgreSQL PR retry reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) Fork(context.Context, inbound.ForkInput) (inbound.ForkOutput, error) {
	panic("actual PostgreSQL fork reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) PreviewJoin(context.Context, inbound.JoinPreviewInput) (inbound.JoinPreviewOutput, error) {
	panic("actual PostgreSQL preview reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) ConfirmJoin(context.Context, inbound.ConfirmJoinInput) (inbound.JoinOutput, error) {
	panic("actual PostgreSQL join reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) EnableContextProtocol(context.Context, domain.ContentHash) error {
	panic("actual PostgreSQL protocol reached the broad facade")
}

func (*repositoryRefPGPoisonBackend) ListHistory(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error) {
	panic("actual PostgreSQL history read reached the broad facade")
}

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
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/conditional-append.git"}
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
	commands, err := service.RepositoryRefCommands()
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&repositoryRefPGPoisonBackend{repositoryAssetPGPoisonFacade: &repositoryAssetPGPoisonFacade{Service: service}}, identity)
	if err := server.SetRepositoryRefCommands(commands); err != nil {
		t.Fatal(err)
	}
	historyCommands, err := service.HistoryCommands()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SetHistoryCommands(historyCommands); err != nil {
		t.Fatal(err)
	}
	prCommands, err := service.PRPromotionCommands()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SetPRPromotionCommands(prCommands); err != nil {
		t.Fatal(err)
	}
	branchOperations, err := service.ContextBranchOperations()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SetContextBranchOperations(branchOperations); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
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
	t.Run("actual-history-and-memory-selection", func(test *testing.T) {
		before := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo.ID), BranchID: ref.BranchID,
			Branch: "main", Kind: "position", Source: target, Target: target, SharedTarget: target,
			MemoryPinned: true, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("b", 32), CreatedAt: time.Now().UTC()}
		memory, err := storage.PutMemory(ctx, repo.ID, domain.MemoryDigest{SnapshotID: target, Summary: "HTTP selected memory"})
		if err != nil {
			test.Fatal(err)
		}
		after := before
		after.ID, after.MemoryHash, after.MemorySelectionParent = strings.Repeat("2", 32), memory, before.ID
		after.CreatedAt = before.CreatedAt.Add(-time.Minute)
		for _, delivery := range []struct {
			suffix string
			event  domain.HistoryEvent
		}{{"/history", before}, {"/history/memory-selection", after}, {"/history/memory-selection", after}} {
			encoded, err := json.Marshal(delivery.event)
			if err != nil {
				test.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+string(repo.ID)+delivery.suffix, strings.NewReader(string(encoded))).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+ownerSession.Token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), delivery.event.ID) {
				test.Fatalf("actual history owner rejected %s: %d %s", delivery.suffix, response.Code, response.Body.String())
			}
		}
		accepted, err := storage.ListHistoryEvents(ctx, repo.ID)
		if err != nil || len(accepted) != 2 || accepted[0].ID != before.ID || accepted[1].ID != after.ID || accepted[1].MemoryHash != memory || accepted[1].MemorySelectionParent != before.ID {
			test.Fatal("HTTP did not publish exact predecessor/selected memory once", accepted, err)
		}
		shared, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
		if err != nil || shared.Target != target || shared.BranchID != current.BranchID {
			test.Fatal("history/memory selection moved shared main", shared, err)
		}
	})
	t.Run("actual-PR-acceptance-list-retry-and-starved-delivery", func(test *testing.T) {
		promotion := domain.PullRequestMerge{Number: 62, BaseBranch: "main", HeadBranch: "feature/http",
			HeadSHA: strings.Repeat("c", 40), MergeSHA: strings.Repeat("d", 40)}
		encoded, err := json.Marshal(promotion)
		if err != nil {
			test.Fatal(err)
		}
		call := func(method, suffix string, want int) *httptest.ResponseRecorder {
			test.Helper()
			request := httptest.NewRequest(method, "/api/v1/repos/"+string(repo.ID)+suffix, strings.NewReader(string(encoded))).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+ownerSession.Token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != want {
				test.Fatalf("actual PR owner rejected %s: %d %s", suffix, response.Code, response.Body.String())
			}
			return response
		}
		call(http.MethodPost, "/prs/promotions", http.StatusOK)
		response := call(http.MethodGet, "/prs/promotions", http.StatusOK)
		var jobs []domain.PRPromotionJob
		if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil || len(jobs) != 1 || jobs[0].PR != promotion || jobs[0].State != "waiting" || jobs[0].Association == nil || jobs[0].Association.CurrentApplicability != "not_evaluated" {
			test.Fatal("actual PR listing lost acceptance or fabricated applicability", jobs, err)
		}
		call(http.MethodPost, "/prs/promotions/"+jobs[0].ID+"/retry", http.StatusOK)
		accepted, err := storage.GetPRJob(ctx, repo.ID, jobs[0].ID)
		if err != nil || accepted.Version != jobs[0].Version || accepted.State != "waiting" || accepted.PR != promotion || !accepted.UpdatedAt.Equal(jobs[0].UpdatedAt) || !accepted.LeaseUntil.IsZero() {
			test.Fatal("actual PR retry changed an already waiting durable job", accepted, err)
		}
		_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
		if err != nil {
			test.Fatal(err)
		}
		defer release()
		call(http.MethodPost, "/prs/promote", http.StatusServiceUnavailable)
		if current, err := storage.GetPRJob(ctx, repo.ID, jobs[0].ID); err != nil || current.State != "waiting" || current.BaseBranchID != accepted.BaseBranchID || current.PR != accepted.PR || current.Attempts != 0 {
			test.Fatal("starved HTTP delivery lost acceptance or claimed work", current, err)
		}
		shared, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
		if err != nil || shared.Target != target || shared.BranchID != current.BranchID {
			test.Fatal("PR queue control moved shared main", shared, err)
		}
	})
	t.Run("actual-PR-current-role-after-body", func(test *testing.T) {
		for index, suffix := range []string{"/prs/promotions", "/prs/promote"} {
			test.Run(suffix, func(test *testing.T) {
				if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: member.ID, Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
					test.Fatal(err)
				}
				promotion := domain.PullRequestMerge{Number: 63 + index, BaseBranch: "main", HeadBranch: "feature/revoked",
					HeadSHA: strings.Repeat("e", 40), MergeSHA: strings.Repeat("f", 40)}
				encoded, err := json.Marshal(promotion)
				if err != nil {
					test.Fatal(err)
				}
				before := state()
				body := &pausedWriteBody{reader: strings.NewReader(string(encoded)), entered: make(chan struct{}), release: make(chan struct{})}
				request := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+string(repo.ID)+suffix, body).WithContext(ctx)
				request.Header.Set("Authorization", "Bearer "+memberSession.Token)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					handler.ServeHTTP(response, request)
				}()
				select {
				case <-body.entered:
				case <-ctx.Done():
					test.Fatal(ctx.Err())
				}
				if err := revoker.RemoveMember(ctx, owner.ID, repository.ID, member.ID); err != nil {
					close(body.release)
					<-finished
					test.Fatal(err)
				}
				close(body.release)
				select {
				case <-finished:
				case <-ctx.Done():
					test.Fatal(ctx.Err())
				}
				if response.Code != http.StatusForbidden || state() != before {
					test.Fatalf("revoked PR caller changed repository state: %d %s", response.Code, response.Body.String())
				}
				if _, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, promotion.Number)); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("revoked PR caller durably accepted work", err)
				}
			})
		}
	})
	t.Run("actual-branch-operations", func(test *testing.T) {
		call := func(method, suffix, payload string, want int) *httptest.ResponseRecorder {
			test.Helper()
			request := httptest.NewRequest(method, "/api/v1/repos/"+string(repo.ID)+suffix, strings.NewReader(payload)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+ownerSession.Token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != want {
				test.Fatalf("actual branch operation rejected %s: %d %s", suffix, response.Code, response.Body.String())
			}
			return response
		}
		call(http.MethodPost, "/fork", `{"from":"`+string(target)+`","new_branch":"feature/http-branch"}`, http.StatusOK)
		call(http.MethodPost, "/context-protocol", `{}`, http.StatusOK)
		call(http.MethodPost, "/fork", `{"from":"`+string(target)+`","new_branch":"feature/http-identity"}`, http.StatusOK)
		var events []domain.HistoryEvent
		response := call(http.MethodGet, "/history", "", http.StatusOK)
		if err := json.Unmarshal(response.Body.Bytes(), &events); err != nil || len(events) != 3 || events[2].Kind != "birth" || events[2].Branch != "feature/http-identity" || events[2].Source != target || events[2].Target != target {
			test.Fatal("actual fork did not publish its source-bound identity", events, err)
		}
		response = call(http.MethodGet, "/join/preview?snapshot="+string(target)+"&branch=main", "", http.StatusOK)
		var preview inbound.JoinPreviewOutput
		if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil || preview.Snapshot != target || preview.Reason == "" {
			test.Fatal("actual preview lost the domain denial", preview, err)
		}
		call(http.MethodPost, "/join", `{"snapshot":"`+string(target)+`","branch":"main","expected_head":"`+string(target)+`","plan_revision":"`+string(domain.HashContent([]byte("stale HTTP plan")))+`","branch_id":"`+current.BranchID+`"}`, http.StatusConflict)
		if shared, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main"); err != nil || shared.Target != target || shared.BranchID != current.BranchID {
			test.Fatal("branch preview or rejected confirmation moved main", shared, err)
		}
	})
	t.Run("actual-fork-revocation-during-body", func(test *testing.T) {
		if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: member.ID, Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
			test.Fatal(err)
		}
		body := &pausedWriteBody{reader: strings.NewReader(`{"from":"` + string(target) + `","new_branch":"feature/revoked-http"}`), entered: make(chan struct{}), release: make(chan struct{})}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+string(repo.ID)+"/fork", body).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+memberSession.Token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			handler.ServeHTTP(response, request)
		}()
		select {
		case <-body.entered:
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		}
		if err := revoker.RemoveMember(ctx, owner.ID, repository.ID, member.ID); err != nil {
			close(body.release)
			<-finished
			test.Fatal(err)
		}
		close(body.release)
		select {
		case <-finished:
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		}
		if response.Code != http.StatusForbidden {
			test.Fatalf("revoked caller forked: %d %s", response.Code, response.Body.String())
		}
		if _, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "feature/revoked-http"); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("revoked fork published a branch", err)
		}
	})
}
