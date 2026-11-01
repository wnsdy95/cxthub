//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGAccountPreferencesStaleSnapshotPreservesCurrentIdentity(test *testing.T) {
	_, storage, _ := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	fixture := makeTeamFixture(test, storage)
	personal, err := fixture.identity.CreateRepository(ctx, fixture.owner, "Personal")
	if err != nil {
		test.Fatal(err)
	}
	stale, err := storage.GetUser(ctx, fixture.owner.ID)
	if err != nil {
		test.Fatal(err)
	}
	username := "renamed-" + domain.NewID("")[:12]
	renamed, err := fixture.identity.UpdateProfile(ctx, stale, &username, nil, nil, nil, nil)
	if err != nil {
		test.Fatal(err)
	}
	provider := domain.User{ID: stale.ID, Name: "Current provider name", Email: "current-" + domain.NewID("")[:12] + "@example.test"}
	current, err := NewIdentityService(organizationTestVerifier{user: provider}, storage).Authenticate(ctx, "verified")
	if err != nil {
		test.Fatal(err)
	}
	if current.Username != renamed.Username {
		test.Fatal("authentication restored an obsolete username")
	}
	nickname := "Fresh preference"
	updated, err := fixture.identity.UpdateProfile(ctx, stale, nil, &nickname, nil, nil, nil)
	if err != nil {
		test.Fatal(err)
	}
	expected := current
	expected.Nickname = nickname
	if !reflect.DeepEqual(updated, expected) {
		test.Fatalf("stale preference snapshot replaced current identity: got=%+v want=%+v", updated, expected)
	}
	persisted, err := storage.GetUser(ctx, stale.ID)
	if err != nil || !reflect.DeepEqual(persisted, expected) {
		test.Fatalf("persisted identity was replaced: %+v %v", persisted, err)
	}
	for _, slug := range []string{stale.Username, username} {
		namespace, err := storage.GetNamespaceBySlug(ctx, slug)
		if err != nil || namespace.ID != personal.OwnerNamespaceID || namespace.Slug != username || namespace.UserID != stale.ID {
			test.Fatalf("preference update restored or replaced namespace %q: %+v %v", slug, namespace, err)
		}
	}
	currentRepository, err := storage.GetRepository(ctx, personal.ID)
	if err != nil || currentRepository.OwnerUsername != username || currentRepository.OwnerNamespaceID != personal.OwnerNamespaceID {
		test.Fatalf("preference update changed personal repository identity: %+v %v", currentRepository, err)
	}
}

type accountPreferencesVerifierPG struct {
	storage *store.PostgresStore
	user    domain.User
	ready   chan struct{}
	release <-chan struct{}
}

func (provider accountPreferencesVerifierPG) Verify(ctx context.Context, _ string) (domain.User, error) {
	if provider.storage.InTransaction(ctx) {
		return domain.User{}, errors.New("provider verification ran inside a storage transaction")
	}
	close(provider.ready)
	select {
	case <-provider.release:
		return provider.user, nil
	case <-ctx.Done():
		return domain.User{}, ctx.Err()
	}
}

func TestPGAccountPreferencesLoginPreservesConcurrentPreferences(test *testing.T) {
	_, storage, _ := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	fixture := makeTeamFixture(test, storage)
	stale, err := storage.GetUser(ctx, fixture.owner.ID)
	if err != nil {
		test.Fatal(err)
	}
	release := make(chan struct{})
	provider := accountPreferencesVerifierPG{
		storage: storage,
		user:    domain.User{ID: stale.ID, Name: "Verified display name", Email: "verified-" + domain.NewID("")[:12] + "@example.test"},
		ready:   make(chan struct{}),
		release: release,
	}
	identity := NewIdentityService(provider, storage)
	done := make(chan error, 1)
	var logged domain.User
	var session domain.Session
	go func() {
		var loginErr error
		logged, session, loginErr = identity.Login(ctx, "verified", "preferences-test")
		done <- loginErr
	}()
	authorityScopeAwait(test, ctx, provider.ready, done)
	nickname, loadMode := "Preferred alias", "memory"
	avatar, locale := "data:image/png;base64,aA==", "ko"
	preferences, err := fixture.identity.UpdateProfile(ctx, stale, nil, &nickname, &loadMode, &avatar, &locale)
	if err != nil {
		test.Fatalf("provider verification blocked a preference write: %v", err)
	}
	close(release)
	if err := authorityScopeDone(test, ctx, done); err != nil {
		test.Fatal(err)
	}
	expected := preferences
	expected.Name, expected.Email = provider.user.Name, provider.user.Email
	if !reflect.DeepEqual(logged, expected) {
		test.Fatalf("login discarded preferences: got=%+v want=%+v", logged, expected)
	}
	if session.UserID != stale.ID || session.Token == "" {
		test.Fatalf("login did not issue the expected user's session: %+v", session)
	}
	resolved, err := identity.ResolveSession(ctx, session.Token)
	if err != nil || !reflect.DeepEqual(resolved, expected) {
		test.Fatalf("session lost persisted preferences: %+v %v", resolved, err)
	}
}

func TestPGAccountPreferencesConcurrentPatchesComposeWithoutBlockingOtherCustomer(test *testing.T) {
	_, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	observer := authorityScopeObserver(test, ctx)
	other := authorityScopeSeed(test, ctx, storage, observer, repo)
	fixture := makeTeamFixture(test, storage)
	stale, err := storage.GetUser(ctx, fixture.owner.ID)
	if err != nil {
		test.Fatal(err)
	}
	blocker, err := observer.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = blocker.Rollback(cleanup)
	}()
	blockerName := "preferences-blocker-" + domain.NewID("")
	if _, err := blocker.Exec(ctx, `SELECT set_config('application_name',$1,true)`, blockerName); err != nil {
		test.Fatal(err)
	}
	var locked string
	if err := blocker.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, stale.ID).Scan(&locked); err != nil {
		test.Fatal(err)
	}
	firstStore, firstName := authorityScopePeer(test, ctx)
	secondStore, secondName := authorityScopePeer(test, ctx)
	first, second := make(chan error, 1), make(chan error, 1)
	nickname, avatar := "Concurrent alias", "data:image/png;base64,aA=="
	loadMode, locale := "reconstructed", "en"
	go func() {
		_, err := NewIdentityService(nil, firstStore).UpdateProfile(ctx, stale, nil, &nickname, nil, &avatar, nil)
		first <- err
	}()
	authorityScopeBlocked(test, ctx, observer, firstName, blockerName, "users", first)
	go func() {
		_, err := NewIdentityService(nil, secondStore).UpdateProfile(ctx, stale, nil, nil, &loadMode, nil, &locale)
		second <- err
	}()
	authorityScopeBlocked(test, ctx, observer, secondName, blockerName, "users", second)
	progress := make(chan error, 1)
	go func() {
		service := NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
		progress <- authorityScopeWrite(ctx, service, other, "settings")
	}()
	if err := authorityScopeDone(test, ctx, progress); err != nil {
		test.Fatalf("held preference transactions blocked another customer's settings write: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := authorityScopeDone(test, ctx, first); err != nil {
		test.Fatal(err)
	}
	if err := authorityScopeDone(test, ctx, second); err != nil {
		test.Fatal(err)
	}
	current, err := storage.GetUser(ctx, stale.ID)
	if err != nil {
		test.Fatal(err)
	}
	expected := stale
	expected.Nickname, expected.Avatar = nickname, avatar
	expected.LoadMode, expected.Locale = loadMode, locale
	if !reflect.DeepEqual(current, expected) {
		test.Fatalf("concurrent preference patches did not compose: got=%+v want=%+v", current, expected)
	}
}

func TestPGAccountPreferencesInvalidAndCanceledUpdatesReturnZero(test *testing.T) {
	_, storage, _ := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	fixture := makeTeamFixture(test, storage)
	before, err := storage.GetUser(ctx, fixture.owner.ID)
	if err != nil {
		test.Fatal(err)
	}
	for _, field := range []string{"load-mode", "avatar", "locale", "username", "user-id", "missing-user", "canceled"} {
		test.Run(field, func(test *testing.T) {
			user := before
			nickname, invalid := "Must not be stored", "invalid"
			var username, loadMode, avatar, locale *string
			work := ctx
			want := domain.ErrValidation
			switch field {
			case "load-mode":
				loadMode = &invalid
			case "avatar":
				avatar = &invalid
			case "locale":
				locale = &invalid
			case "username":
				empty := ""
				username = &empty
			case "user-id":
				user.ID = "invalid\nuser"
			case "missing-user":
				user.ID = domain.NewID("user_")
				want = domain.ErrNotFound
			case "canceled":
				canceled, stop := context.WithCancel(ctx)
				stop()
				work, want = canceled, context.Canceled
			}
			updated, err := fixture.identity.UpdateProfile(work, user, username, &nickname, loadMode, avatar, locale)
			if !errors.Is(err, want) || !reflect.DeepEqual(updated, domain.User{}) {
				test.Fatalf("failed profile update exposed a result: %+v %v, want zero and %v", updated, err, want)
			}
			current, err := storage.GetUser(ctx, before.ID)
			if err != nil || !reflect.DeepEqual(current, before) {
				test.Fatalf("failed profile update changed persisted state: %+v %v", current, err)
			}
			if field == "missing-user" {
				if _, err := storage.GetUser(ctx, user.ID); !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("preference patch created a missing user: %v", err)
				}
			}
		})
	}
}
