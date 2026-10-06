//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGRepositoryInitializationApplicationOnboarding(t *testing.T) {
	svc, st, _ := collaborationPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	name := fmt.Sprintf("onboard%d", time.Now().UnixNano())
	owner := domain.User{ID: "dev:" + name, Username: name, Name: name, Email: name + "@example.test"}
	member := domain.User{ID: "dev:" + name + "member", Username: name + "m", Name: "member", Email: name + "m@example.test"}
	for _, u := range []domain.User{owner, member} {
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: name, Name: "code", Slug: "code", CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: member.ID, Role: domain.RoleMaintainer}); err != nil {
		t.Fatal(err)
	}
	request := domain.RepositoryInitializationRequest{RemoteURL: "https://host.test/" + name + "/code", GitRemoteURL: "https://git.test/code", DefaultBranch: "main"}
	id := domain.HashContent([]byte(normalizeGitURL(request.RemoteURL)))
	userCtx := inbound.WithRepositoryActor(ctx, member.ID)
	receipt, err := svc.BeginRepositoryInitialization(userCtx, member.ID, id, request)
	if err != nil {
		t.Fatal(err)
	}
	got, pending, err := svc.GetRepositoryInitializationView(ctx, id, "main")
	if err != nil || !pending || got.ContextProtocol != 1 {
		t.Fatal("prepared discovery", err)
	}
	// Ordinary member registration during capture does not consume initialization.
	if err := st.WithinIdentity(ctx, func(tx context.Context) error {
		return st.AddMember(tx, domain.Membership{RepositoryID: record.ID, UserID: member.ID, Role: domain.RoleMember})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnsureRepo(userCtx, member.ID, receipt.Repo); err != nil {
		t.Fatal("pending member registration regressed", err)
	}
	a := collaborationSnapshot(t, st, id, "capture")
	if err := svc.PutPending(userCtx, id, "session-fixture", domain.Pending{RepoID: id, SessionID: "session-fixture", Provider: domain.ProviderCodex, Branch: "main", Target: a}); err != nil {
		t.Fatal(err)
	}
	b := collaborationSnapshot(t, st, id, "commit", a)
	as, err := st.GetSnapshot(ctx, id, a)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := st.GetSnapshot(ctx, id, b)
	if err != nil {
		t.Fatal(err)
	}
	final := initializationFinalize(receipt, as, bs)
	if _, err := svc.FinalizeRepositoryInitialization(userCtx, id, final); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("stale role accepted", err)
	}
	if _, err := svc.BeginRepositoryInitialization(userCtx, member.ID, id, request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("member recovered manage receipt", err)
	}
	if err := st.WithinIdentity(ctx, func(tx context.Context) error {
		return st.AddMember(tx, domain.Membership{RepositoryID: record.ID, UserID: member.ID, Role: domain.RoleMaintainer})
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ListPendings(ctx, id)
	completed, err := svc.FinalizeRepositoryInitialization(userCtx, id, final)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Anchor == nil {
		t.Fatal("no completion receipt")
	}
	after, _ := st.ListPendings(ctx, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("pending capture was reconciled")
	}
	got, pending, err = svc.GetRepositoryInitializationView(ctx, id, "main")
	if err != nil || pending || got.ContextProtocol != 1 {
		t.Fatal("completed discovery", err)
	}
	refs, _ := st.ListRefs(ctx, id)
	history, _ := st.ListHistoryEvents(ctx, id)
	if len(refs) != 1 || refs[0] != final.Anchor.Ref || len(history) != 0 {
		t.Fatal("extra refs or invented birth")
	}
	if _, err := svc.FinalizeRepositoryInitialization(userCtx, id, final); err != nil {
		t.Fatal("same retry", err)
	}
}

func TestPGRepositoryInitializationApplicationLegacyMemberUnchanged(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := context.Background()
	// A legacy/unowned repo still exposes pending=false without invoking manage
	// or inferring creation from an empty manifest.
	got, pending, err := svc.GetRepositoryInitializationView(ctx, repo, "main")
	if err != nil || pending || got.ContextProtocol != 0 {
		t.Fatal("legacy discovery", err)
	}
	if _, err := st.GetRepositoryInitialization(ctx, repo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("read minted receipt", err)
	}
}
