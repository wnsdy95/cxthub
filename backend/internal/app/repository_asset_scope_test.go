package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// assetIdentityStore records the one requirement a scope hands on.
type assetIdentityStore struct {
	calls    int
	repo     domain.ContentHash
	identity domain.DocumentIdentity
	answer   error
}

func (identities *assetIdentityStore) RequireDocumentIdentity(_ context.Context, repo domain.ContentHash, identity domain.DocumentIdentity) error {
	identities.calls++
	identities.repo, identities.identity = repo, identity
	return identities.answer
}

// A repository is made to require the second document format only when the
// server publishes it, the caller and the server both read it, the change
// happens in a transaction and there is a store for the requirement. A
// repository that requires it does not go back.
func TestAssetDocumentIdentityRequirementIsGated(t *testing.T) {
	storage := store.NewFSStore(t.TempDir())
	ctx := systemTestContext()
	legacyRepo, rootRepo := hh("requires nothing"), hh("requires the second format")
	root := domain.DocumentIdentityRootV1
	for _, repo := range []domain.ContentHash{legacyRepo, rootRepo} {
		if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	// A repository is registered without a requirement; the store records one afterwards.
	if err := storage.RequireDocumentIdentity(ctx, rootRepo, root); err != nil {
		t.Fatal(err)
	}
	if stored, err := storage.GetRepo(ctx, rootRepo); err != nil || stored.RequiredDocIdentity != root {
		t.Fatalf("the fixture repository does not require the second format: %+v %v", stored, err)
	}
	reads := inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{root})
	refusal := errors.New("synthetic: the store refused the requirement")
	type scenario struct {
		name          string
		ctx           context.Context
		repo          domain.ContentHash
		next          domain.DocumentIdentity
		supported     []domain.DocumentIdentity
		publication   bool
		transactional bool
		withoutStore  bool
		answer        error
		want          error
		handedOn      bool
	}
	all := []domain.DocumentIdentity{root}
	for _, s := range []scenario{
		{name: "nothing is asked of a repository that keeps the first format", ctx: ctx, repo: legacyRepo, next: domain.DocumentIdentityLegacy},
		{name: "a repository that requires the second format does not go back", ctx: reads, repo: rootRepo, next: domain.DocumentIdentityLegacy, supported: all, publication: true, transactional: true, want: domain.ErrConflict},
		{name: "a format that does not exist", ctx: reads, repo: legacyRepo, next: "future", supported: all, publication: true, transactional: true, want: domain.ErrUnsupportedDocumentIdentity},
		{name: "a repository that is not there", ctx: reads, repo: hh("no such repository"), next: root, supported: all, publication: true, transactional: true, want: domain.ErrNotFound},
		{name: "the server does not publish the second format", ctx: reads, repo: legacyRepo, next: root, supported: all, transactional: true, want: domain.ErrRootPublicationDisabled},
		{name: "the caller does not read the second format", ctx: ctx, repo: legacyRepo, next: root, supported: all, publication: true, transactional: true, want: domain.ErrDocumentIdentityUpgradeRequired},
		{name: "the server does not read the second format", ctx: reads, repo: legacyRepo, next: root, publication: true, transactional: true, want: domain.ErrDocumentIdentityUpgradeRequired},
		{name: "the store has no transactions", ctx: reads, repo: legacyRepo, next: root, supported: all, publication: true, want: domain.ErrRootPublicationDisabled},
		{name: "there is no store for the requirement", ctx: reads, repo: legacyRepo, next: root, supported: all, publication: true, transactional: true, withoutStore: true, want: domain.ErrRootPublicationDisabled},
		{name: "everything holds: the requirement is handed on", ctx: reads, repo: legacyRepo, next: root, supported: all, publication: true, transactional: true, handedOn: true},
		{name: "what the store answers is the answer", ctx: reads, repo: legacyRepo, next: root, supported: all, publication: true, transactional: true, answer: refusal, want: refusal, handedOn: true},
		{name: "a repository that already requires it may be asked again", ctx: reads, repo: rootRepo, next: root, supported: all, publication: true, transactional: true, handedOn: true},
	} {
		identities := &assetIdentityStore{answer: s.answer}
		scope := &repositoryAssetScope{work: &repositoryWork{meta: storage, supported: s.supported}, rootPublication: s.publication, transactional: s.transactional}
		if !s.withoutStore {
			scope.identityStore = identities
		}
		err := scope.RequireAssetDocumentIdentity(s.ctx, s.repo, s.next)
		if s.want == nil && err != nil || s.want != nil && !errors.Is(err, s.want) {
			t.Errorf("%s: %v", s.name, err)
		}
		if s.handedOn != (identities.calls == 1) || identities.calls > 1 {
			t.Errorf("%s: the requirement was handed to the store %d times", s.name, identities.calls)
		}
		if s.handedOn && (identities.repo != s.repo || identities.identity != s.next) {
			t.Errorf("%s: handed on for %s as %q", s.name, identities.repo, identities.identity)
		}
	}
	// A scope that was not put together answers nothing.
	var absent *repositoryAssetScope
	if err := absent.RequireAssetDocumentIdentity(reads, legacyRepo, root); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a scope that is nil: %v", err)
	}
	if err := (&repositoryAssetScope{}).RequireAssetDocumentIdentity(reads, legacyRepo, root); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("a scope without its work: %v", err)
	}
}
