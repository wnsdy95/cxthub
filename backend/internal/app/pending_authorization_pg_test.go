//go:build postgres

package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGPendingCreationResolvesOwnershipInsideTransaction(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	services := []*Service{NewService(st, st, nil, gitengine.NewEngine(st), st), NewService(peer, peer, nil, gitengine.NewEngine(peer), peer)}
	repo := domain.Repo{ID: hh(f.repository.ID), RepositoryID: f.repository.ID, DefaultBranch: "main"}
	if _, err = st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	for _, u := range []domain.User{f.member, f.outsider} {
		if err = st.AddMember(ctx, domain.Membership{RepositoryID: f.repository.ID, UserID: u.ID, Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err = st.PutDoc(ctx, repo.ID, doc); err != nil {
		t.Fatal(err)
	}
	target := doc.Hash
	if err = st.PutSnapshot(ctx, domain.Snapshot{ID: target, DocHash: target, RepoID: repo.ID, Fidelity: domain.FidelityFull, Provider: domain.ProviderUnknown, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for n, u := range []domain.User{f.member, f.outsider} {
		wg.Add(1)
		go func(n int, u domain.User) {
			defer wg.Done()
			<-start
			results <- services[n].PutPending(inbound.WithRepositoryActor(context.Background(), u.ID), repo.ID, "same-session", domain.Pending{Target: target, Author: domain.TeamIdentity{Email: "forged@example.test"}})
		}(n, u)
	}
	close(start)
	wg.Wait()
	close(results)
	allowed, denied := 0, 0
	for err := range results {
		if err == nil {
			allowed++
		} else if errors.Is(err, domain.ErrForbidden) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if allowed != 1 || denied != 1 {
		t.Fatalf("owners allowed=%d denied=%d", allowed, denied)
	}
	pending, err := st.ListPendings(ctx, repo.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	loser := f.member
	if pending[0].Author.Email == f.member.Email {
		loser = f.outsider
	} else if pending[0].Author.Email != f.outsider.Email {
		t.Fatal("author not authenticated")
	}
	actor := inbound.WithRepositoryActor(context.Background(), loser.ID)
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"dismiss", func() error { return services[0].DismissPending(actor, repo.ID, "same-session") }},
		{"undismiss", func() error { return services[0].UndismissPending(actor, repo.ID, "same-session") }},
		{"delete", func() error { return services[0].DeletePending(actor, repo.ID, "same-session") }},
		{"compare-delete", func() error {
			_, err := services[0].CompareAndDeletePending(actor, repo.ID, "same-session", target)
			return err
		}},
	} {
		if err := op.run(); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("%s ownership bypass: %v", op.name, err)
		}
	}
}
func TestPGRepoProfileRejectsPartialPatch(t *testing.T) {
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	old, err := st.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	branch, badSite := "changed", "javascript:alert(1)"
	if _, err = svc.PatchRepoProfile(ctx, repo, inbound.RepoProfilePatch{DefaultBranch: &branch, Website: &badSite}); err == nil {
		t.Fatal("invalid website accepted")
	}
	after, err := st.GetRepo(ctx, repo)
	if err != nil || after.DefaultBranch != old.DefaultBranch {
		t.Fatalf("partial configuration write: %+v %v", after, err)
	}
}
