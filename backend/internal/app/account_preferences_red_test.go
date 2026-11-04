package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func accountPreferencesFSUser(test *testing.T) (*store.FSStore, domain.User) {
	test.Helper()
	storage := store.NewFSStore(test.TempDir())
	user := domain.User{
		ID:        "account-preferences-user",
		Email:     "original@example.test",
		Name:      "Original Provider Name",
		Username:  "preferences-original",
		Nickname:  "Stored Nickname",
		LoadMode:  "full",
		Avatar:    "data:image/png;base64,aA==",
		Locale:    "en",
		CreatedAt: time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := storage.UpsertUser(context.Background(), user); err != nil {
		test.Fatal(err)
	}
	return storage, user
}

func TestAccountPreferencesNicknameUpdatePreservesRenamedIdentity(test *testing.T) {
	for _, scenario := range []string{"stale-provider-fields", "forged-provider-fields"} {
		test.Run(scenario, func(test *testing.T) {
			ctx := context.Background()
			storage, original := accountPreferencesFSUser(test)
			identity := NewIdentityService(nil, storage)
			repository, err := identity.CreateRepository(ctx, original, "AccountPreferences")
			if err != nil {
				test.Fatal(err)
			}

			current := original
			current.Email = "latest@example.test"
			current.Name = "Latest Provider Name"
			current.Nickname = "Latest Nickname"
			current.LoadMode = "memory"
			current.Avatar = "data:image/png;base64,ag=="
			current.Locale = "ko"
			if err := storage.UpsertUser(ctx, current); err != nil {
				test.Fatal(err)
			}
			nextUsername := "preferences-current"
			renamed, err := identity.UpdateProfile(ctx, current, &nextUsername, nil, nil, nil, nil)
			if err != nil {
				test.Fatalf("rename current user: %v", err)
			}
			current.Username = nextUsername
			if renamed != current {
				test.Fatalf("rename returned %+v, want %+v", renamed, current)
			}
			persistedRename, err := storage.GetUser(ctx, current.ID)
			if err != nil || persistedRename != current {
				test.Fatalf("rename was not persisted: user=%+v err=%v", persistedRename, err)
			}

			supplied := original
			if scenario == "forged-provider-fields" {
				supplied.Email = "forged@example.test"
				supplied.Name = "Forged Provider Name"
				supplied.CreatedAt = time.Date(1999, time.January, 1, 0, 0, 0, 0, time.UTC)
			}
			nickname := "  Chosen Nickname  "
			updated, err := identity.UpdateProfile(ctx, supplied, nil, &nickname, nil, nil, nil)
			if err != nil {
				test.Fatalf("nickname-only update with stale caller: %v", err)
			}
			expected := current
			expected.Nickname = "Chosen Nickname"
			if updated != expected {
				test.Errorf("nickname-only result replaced authoritative fields: got=%+v want=%+v", updated, expected)
			}
			persisted, err := storage.GetUser(ctx, current.ID)
			if err != nil {
				test.Fatal(err)
			}
			if persisted != expected {
				test.Errorf("nickname-only update overwrote current account: got=%+v want=%+v", persisted, expected)
			}
			byUsername, err := storage.GetUserByUsername(ctx, nextUsername)
			if err != nil || byUsername != expected {
				test.Errorf("current username no longer resolves authoritative user: user=%+v err=%v", byUsername, err)
			}
			if previous, err := storage.GetUserByUsername(ctx, original.Username); !errors.Is(err, domain.ErrNotFound) {
				test.Errorf("nickname-only update resurrected old username: user=%+v err=%v", previous, err)
			}

			for _, handle := range []string{original.Username, nextUsername} {
				namespace, err := storage.GetNamespaceBySlug(ctx, handle)
				if err != nil || namespace.ID != repository.OwnerNamespaceID || namespace.Kind != domain.NamespaceUser || namespace.UserID != current.ID || namespace.Slug != nextUsername {
					test.Errorf("namespace %q lost canonical rename: namespace=%+v err=%v", handle, namespace, err)
				}
				resolved, err := identity.ReadableRepository(ctx, handle, repository.Slug, current.ID)
				if err != nil || resolved.ID != repository.ID || resolved.OwnerID != current.ID || resolved.OwnerNamespaceID != repository.OwnerNamespaceID || resolved.OwnerUsername != nextUsername {
					test.Errorf("repository URL %q lost stable identity: repository=%+v err=%v", handle, resolved, err)
				}
			}
			profile, repositories, err := identity.PublicUser(ctx, nextUsername, current.ID)
			if err != nil || profile != expected || len(repositories) != 1 || repositories[0].ID != repository.ID {
				test.Errorf("current personal profile lost account or repository mapping: user=%+v repositories=%+v err=%v", profile, repositories, err)
			}
		})
	}
}

func TestAccountPreferencesAuthenticatePreservesStoredPreferences(test *testing.T) {
	ctx := context.Background()
	storage, stored := accountPreferencesFSUser(test)
	provider := domain.User{
		ID:    stored.ID,
		Email: "fresh-provider@example.test",
		Name:  "Fresh Provider Name",
	}
	identity := NewIdentityService(organizationTestVerifier{user: provider}, storage)
	authenticated, err := identity.Authenticate(ctx, "fresh-provider-token")
	if err != nil {
		test.Fatal(err)
	}
	expected := stored
	expected.Email = provider.Email
	expected.Name = provider.Name
	if authenticated != expected {
		test.Errorf("authentication did not preserve preferences while refreshing provider identity: got=%+v want=%+v", authenticated, expected)
	}
	persisted, err := storage.GetUser(ctx, stored.ID)
	if err != nil {
		test.Fatal(err)
	}
	if persisted != expected {
		test.Errorf("authentication overwrote stored preferences: got=%+v want=%+v", persisted, expected)
	}
	namespace, err := storage.GetNamespaceBySlug(ctx, stored.Username)
	if err != nil || namespace.Kind != domain.NamespaceUser || namespace.UserID != stored.ID || namespace.Slug != stored.Username {
		test.Errorf("authentication changed personal namespace identity: namespace=%+v err=%v", namespace, err)
	}
}

type accountPreferencesReadFailureStore struct {
	*store.FSStore
	readError error
	upserts   int
}

func (storage *accountPreferencesReadFailureStore) GetUser(context.Context, string) (domain.User, error) {
	return domain.User{}, storage.readError
}

func (storage *accountPreferencesReadFailureStore) UpsertUser(ctx context.Context, user domain.User) error {
	storage.upserts++
	return storage.FSStore.UpsertUser(ctx, user)
}

func TestAccountPreferencesAuthenticateFailsClosedOnUserReadError(test *testing.T) {
	ctx := context.Background()
	storage, stored := accountPreferencesFSUser(test)
	readError := errors.New("account user read unavailable")
	failing := &accountPreferencesReadFailureStore{FSStore: storage, readError: readError}
	provider := domain.User{ID: stored.ID, Email: "fresh-provider@example.test", Name: "Fresh Provider Name"}
	identity := NewIdentityService(organizationTestVerifier{user: provider}, failing)
	authenticated, err := identity.Authenticate(ctx, "fresh-provider-token")
	if !errors.Is(err, readError) {
		test.Errorf("authentication ignored existing-user read failure: user=%+v err=%v", authenticated, err)
	}
	if authenticated != (domain.User{}) {
		test.Errorf("authentication returned a user after read failure: %+v", authenticated)
	}
	if failing.upserts != 0 {
		test.Errorf("authentication attempted %d upserts after user read failure", failing.upserts)
	}
	persisted, err := storage.GetUser(ctx, stored.ID)
	if err != nil || persisted != stored {
		test.Errorf("read failure changed persisted account: user=%+v err=%v", persisted, err)
	}
}
