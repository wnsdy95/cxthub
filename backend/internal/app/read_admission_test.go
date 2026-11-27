package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Who may read a repository is decided before anything of it is read: whoever
// may view it, which for a public repository is anyone, signed in or not.
func TestRepositoryReadIsAdmittedForWhoMayViewIt(t *testing.T) {
	storage := store.NewFSStore(t.TempDir())
	identity := NewIdentityService(nil, storage)
	ctx := systemTestContext()
	owner := domain.User{ID: "read-owner", Email: "read-owner@example.test", Name: "Owner", Username: "read-owner"}
	stranger := domain.User{ID: "read-stranger", Email: "read-stranger@example.test", Name: "Stranger", Username: "read-stranger"}
	for _, user := range []domain.User{owner, stranger} {
		if err := storage.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := identity.CreateRepository(ctx, owner, "kept-private")
	if err != nil {
		t.Fatal(err)
	}
	admit := func(repositoryID, userID string) error {
		t.Helper()
		permit, err := identity.AdmitRepositoryRead(context.Background(), repositoryID, userID)
		if (err == nil) != (permit != nil) {
			t.Fatalf("a permit without admission, or admission without a permit: %v %v", permit, err)
		}
		return err
	}
	// A private repository: its owner reads it, nobody else does, and a
	// caller who is nobody is told to sign in.
	if err := admit(repository.ID, owner.ID); err != nil {
		t.Errorf("the owner of a private repository: %v", err)
	}
	if err := admit(repository.ID, stranger.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a stranger to a private repository: %v", err)
	}
	if err := admit(repository.ID, ""); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("nobody, at a private repository: %v", err)
	}
	// A repository that does not exist is not found, whoever asks.
	missing := domain.NewID("ws_")
	for _, user := range []string{owner.ID, ""} {
		if err := admit(missing, user); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("a repository that does not exist, asked by %q: %v", user, err)
		}
	}
	// Made public, it is read by a stranger and by nobody at all.
	public := domain.VisibilityPublic
	if _, err := identity.UpdateRepositorySettings(ctx, owner.ID, repository.ID, RepositoryPatch{Visibility: &public}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{owner.ID, stranger.ID, ""} {
		if err := admit(repository.ID, user); err != nil {
			t.Errorf("a public repository, asked by %q: %v", user, err)
		}
	}
	// Private again, it is closed again.
	private := domain.VisibilityPrivate
	if _, err := identity.UpdateRepositorySettings(ctx, owner.ID, repository.ID, RepositoryPatch{Visibility: &private}); err != nil {
		t.Fatal(err)
	}
	if err := admit(repository.ID, ""); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("nobody, after it was made private again: %v", err)
	}
}

// readAdmissionStore is a file store that counts its document searches and
// can say that a repository requires another document identity.
type readAdmissionStore struct {
	*store.FSStore
	searches int
	required domain.DocumentIdentity
}

func (storage *readAdmissionStore) MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error) {
	storage.searches++
	return map[domain.ContentHash]bool{}, nil
}

func (storage *readAdmissionStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if storage.required == "" {
		return storage.FSStore.GetRepo(ctx, repo)
	}
	return domain.Repo{ID: repo, RequiredDocIdentity: storage.required}, nil
}

// A search over a repository's documents is admitted before the store is
// touched: a caller that has given up is not served, and neither is a caller
// that cannot read the document identity the repository requires.
func TestDocumentSearchIsAdmittedBeforeTheStoreIsRead(t *testing.T) {
	storage := &readAdmissionStore{FSStore: store.NewFSStore(t.TempDir())}
	service := NewService(storage, storage, storage)
	repo := hh(t.Name())
	matches, err := service.MatchingDocHashes(systemTestContext(), repo, "  Needle ")
	if err != nil || matches == nil || storage.searches != 1 {
		t.Fatalf("a search: %v %v after %d searches", matches, err, storage.searches)
	}
	// The caller is gone before the search is admitted.
	gone, cancel := context.WithCancel(systemTestContext())
	cancel()
	if matches, err := service.MatchingDocHashes(gone, repo, "needle"); !errors.Is(err, context.Canceled) || matches != nil || storage.searches != 1 {
		t.Fatalf("a caller that gave up: %v %v after %d searches", matches, err, storage.searches)
	}
	// The repository requires a document identity this server does not read.
	storage.required = domain.DocumentIdentityRootV1
	if matches, err := service.MatchingDocHashes(systemTestContext(), repo, "needle"); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || matches != nil || storage.searches != 1 {
		t.Fatalf("a repository of another document identity: %v %v after %d searches", matches, err, storage.searches)
	}
	// What is not a repository's name is refused the same way, before the store.
	storage.required = ""
	if _, err := service.MatchingDocHashes(systemTestContext(), "not-a-hash", "needle"); !errors.Is(err, domain.ErrIntegrity) || storage.searches != 1 {
		t.Fatalf("a name that is no repository's: %v after %d searches", err, storage.searches)
	}
}
