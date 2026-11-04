//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.UserPreferencesStore = (*PostgresStore)(nil)

type userPreferencesPauseKeyPG struct{}

type userPreferencesPausePG struct {
	ready   chan context.Context
	release chan struct{}
}

func (pause *userPreferencesPausePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ")
	if strings.Contains(statement, "update users") {
		return context.WithValue(ctx, userPreferencesPauseKeyPG{}, true)
	}
	return ctx
}

func (pause *userPreferencesPausePG) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(userPreferencesPauseKeyPG{}) != true || data.Err != nil {
		return
	}
	select {
	case pause.ready <- ctx:
	case <-ctx.Done():
		return
	}
	select {
	case <-pause.release:
	case <-ctx.Done():
	}
}

func userPreferencesStringPG(value string) *string {
	return &value
}

func userPreferencesUserPG(test *testing.T, ctx context.Context, storage *PostgresStore) domain.User {
	test.Helper()
	user := domain.User{
		ID: domain.NewID("user_"), Email: "stored@example.test", Name: "Stored identity name",
		Username: domain.NewID("preferences"), Nickname: "Stored alias", LoadMode: "reconstructed",
		Avatar: "data:image/png;base64,AQID", Locale: "en",
	}
	if err := storage.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	stored, err := storage.GetUser(ctx, user.ID)
	if err != nil || stored.CreatedAt.IsZero() {
		test.Fatal("could not read the stored identity baseline", stored, err)
	}
	return stored
}

func userPreferencesRunPG(test *testing.T, parent context.Context, storage *PostgresStore, userID string, patch domain.UserPreferencesPatch) (context.Context, context.CancelFunc, <-chan error, <-chan domain.User) {
	test.Helper()
	users := make(chan domain.User, 1)
	work, cancel, completed := authorityScopeRunPG(test, parent, func(ctx context.Context) error {
		var preferences outbound.UserPreferencesStore = storage
		user, err := preferences.UpdateUserPreferences(ctx, userID, patch)
		users <- user
		return err
	})
	return work, cancel, completed, users
}

func userPreferencesPausedPG(test *testing.T, ctx context.Context, pause *userPreferencesPausePG, completed <-chan error) context.Context {
	test.Helper()
	select {
	case bound := <-pause.ready:
		return bound
	case err := <-completed:
		test.Fatal("preference method exited before its real UPDATE completed", err)
	case <-ctx.Done():
		test.Fatal("preference method did not reach the post-UPDATE barrier", ctx.Err())
	}
	return nil
}

func TestPGUserPreferencesPartialFieldsPreserveStoredIdentity(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	for _, field := range []string{"nickname", "load-mode", "avatar", "locale", "clear-nickname", "clear-load-mode", "clear-avatar", "clear-locale", "unchanged"} {
		test.Run(field, func(test *testing.T) {
			before := userPreferencesUserPG(test, fixture.ctx, storage)
			expected := before
			patch := domain.UserPreferencesPatch{}
			switch field {
			case "nickname":
				patch.Nickname = userPreferencesStringPG("  New display alias  ")
				expected.Nickname = "New display alias"
			case "load-mode":
				patch.LoadMode = userPreferencesStringPG("  memory  ")
				expected.LoadMode = "memory"
			case "avatar":
				patch.Avatar = userPreferencesStringPG("data:image/webp;base64,BAUG")
				expected.Avatar = "data:image/webp;base64,BAUG"
			case "locale":
				patch.Locale = userPreferencesStringPG("  ko  ")
				expected.Locale = "ko"
			case "clear-nickname":
				patch.Nickname = userPreferencesStringPG("")
				expected.Nickname = ""
			case "clear-load-mode":
				patch.LoadMode = userPreferencesStringPG("")
				expected.LoadMode = ""
			case "clear-avatar":
				patch.Avatar = userPreferencesStringPG("")
				expected.Avatar = ""
			case "clear-locale":
				patch.Locale = userPreferencesStringPG("")
				expected.Locale = ""
			}
			var preferences outbound.UserPreferencesStore = storage
			updated, err := preferences.UpdateUserPreferences(fixture.ctx, before.ID, patch)
			if err != nil || !reflect.DeepEqual(updated, expected) {
				test.Fatal("partial preferences changed omitted preferences or stored identity", updated, expected, err)
			}
			stored, err := storage.GetUser(fixture.ctx, before.ID)
			if err != nil || !reflect.DeepEqual(stored, expected) {
				test.Fatal("returned preferences did not match the committed user", stored, expected, err)
			}
		})
	}
}

func TestPGUserPreferencesConcurrentPartialWritesAndNonKeyLock(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	before := userPreferencesUserPG(test, fixture.ctx, storage)
	other := userPreferencesUserPG(test, fixture.ctx, storage)
	_, unrelatedRepo := authorityScopeRepositoryPG(test, fixture.ctx, storage)
	pause := &userPreferencesPausePG{ready: make(chan context.Context, 1), release: make(chan struct{}, 1)}
	defer close(pause.release)
	first := authorityScopePeerPG(test, storage, "", pause)
	second := authorityScopePeerPG(test, storage, "", nil)
	unrelated := authorityScopePeerPG(test, storage, "", nil)
	wide := authorityScopePeerPG(test, storage, "", nil)
	firstPID := authorityScopePIDPG(test, fixture.ctx, first)
	secondPID := authorityScopePIDPG(test, fixture.ctx, second)
	widePID := authorityScopePIDPG(test, fixture.ctx, wide)
	held, _, firstDone, firstUsers := userPreferencesRunPG(test, fixture.ctx, first, before.ID, domain.UserPreferencesPatch{Nickname: userPreferencesStringPG("Concurrent alias")})
	bound := userPreferencesPausedPG(test, held, pause, firstDone)
	transaction, present := bound.Value(repositoryTxKey{}).(*repositoryTx)
	if !present || transaction.owner != first || transaction.profileUser != before.ID || transaction.identity || transaction.readOnly || transaction.repo != "" || transaction.repository != "" {
		test.Fatal("preference method did not own a private user-scoped transaction binding")
	}
	for _, attempt := range []func() (domain.User, error){
		func() (domain.User, error) {
			return first.UpdateUserPreferences(bound, other.ID, domain.UserPreferencesPatch{Locale: userPreferencesStringPG("ko")})
		},
		func() (domain.User, error) {
			return second.UpdateUserPreferences(bound, before.ID, domain.UserPreferencesPatch{Locale: userPreferencesStringPG("ko")})
		},
	} {
		user, err := attempt()
		if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(user, domain.User{}) {
			test.Fatal("private profile binding allowed a different user or foreign owner", user, err)
		}
	}
	work, _, secondDone, secondUsers := userPreferencesRunPG(test, fixture.ctx, second, before.ID, domain.UserPreferencesPatch{Locale: userPreferencesStringPG("ko")})
	authorityScopeWaitPG(test, work, fixture.observer, firstPID, secondPID, "users", secondDone)
	otherWork, _, otherDone, otherUsers := userPreferencesRunPG(test, fixture.ctx, unrelated, other.ID, domain.UserPreferencesPatch{LoadMode: userPreferencesStringPG("full")})
	authorityScopeDonePG(test, otherWork, otherDone)
	otherExpected := other
	otherExpected.LoadMode = "full"
	if updated := <-otherUsers; !reflect.DeepEqual(updated, otherExpected) {
		test.Fatal("unrelated user preferences did not progress independently", updated, otherExpected)
	}
	repositoryWork, _, repositoryDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
		return unrelated.PutSecretsEnvelope(ctx, unrelatedRepo, []byte(`{"synthetic":"unrelated profile"}`))
	})
	authorityScopeDonePG(test, repositoryWork, repositoryDone)
	fkRecord := domain.Repository{ID: domain.NewID("ws_"), OwnerID: before.ID, OwnerUsername: before.Username, Name: "Synthetic user FK compatibility", Slug: "profile-fk"}
	fkWork, _, fkDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
		return unrelated.CreateRepository(ctx, fkRecord)
	})
	authorityScopeDonePG(test, fkWork, fkDone)
	if held.Err() != nil {
		test.Fatal("positive progress happened only after the preference holder expired", held.Err())
	}
	select {
	case err := <-firstDone:
		test.Fatal("preference holder exited before independent work committed", err)
	default:
	}
	if _, err := storage.GetRepository(held, fkRecord.ID); err != nil {
		test.Fatal("user-FK KEY SHARE reference did not commit under the non-key preference lock", err)
	}
	if _, err := storage.GetSecretsEnvelope(held, unrelatedRepo); err != nil {
		test.Fatal("unrelated repository write was not durable under the held user row", err)
	}
	wideWork, _, wideDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
		return wide.WithinIdentity(ctx, func(context.Context) error { return nil })
	})
	authorityScopeWaitPG(test, wideWork, fixture.observer, firstPID, widePID, "pg_advisory_xact_lock", wideDone)
	pause.release <- struct{}{}
	authorityScopeDonePG(test, held, firstDone)
	authorityScopeDonePG(test, work, secondDone)
	authorityScopeDonePG(test, wideWork, wideDone)
	firstResult, secondResult := <-firstUsers, <-secondUsers
	firstExpected := before
	firstExpected.Nickname = "Concurrent alias"
	if !reflect.DeepEqual(firstResult, firstExpected) {
		test.Fatal("first partial write returned an uncommitted or stale identity", firstResult, firstExpected)
	}
	expected := firstExpected
	expected.Locale = "ko"
	if !reflect.DeepEqual(secondResult, expected) {
		test.Fatal("serialized partial write discarded the first preference", secondResult, expected)
	}
	stored, err := storage.GetUser(fixture.ctx, before.ID)
	if err != nil || !reflect.DeepEqual(stored, expected) {
		test.Fatal("concurrent partial preferences failed to compose", stored, expected, err)
	}
}

func TestPGUserPreferencesWaitsForWideIdentityAndKeepsLatestIdentity(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	before := userPreferencesUserPG(test, fixture.ctx, storage)
	holder := authorityScopePeerPG(test, storage, "", nil)
	writer := authorityScopePeerPG(test, storage, "", nil)
	holderPID := authorityScopePIDPG(test, fixture.ctx, holder)
	writerPID := authorityScopePIDPG(test, fixture.ctx, writer)
	ready, release := make(chan struct{}), make(chan struct{}, 1)
	defer close(release)
	latest := before
	latest.Name = "Latest authenticated name"
	latest.Email = "latest@example.test"
	latest.Username = domain.NewID("renamed")
	latest.LoadMode = "full"
	held, _, holderDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
		return holder.WithinIdentity(ctx, func(bound context.Context) error {
			if err := holder.UpsertUser(bound, latest); err != nil {
				return err
			}
			close(ready)
			select {
			case <-release:
				return nil
			case <-bound.Done():
				return bound.Err()
			}
		})
	})
	authorityScopeReadyPG(test, held, ready, holderDone)
	work, _, writerDone, users := userPreferencesRunPG(test, fixture.ctx, writer, before.ID, domain.UserPreferencesPatch{Nickname: userPreferencesStringPG("Updated after identity")})
	authorityScopeWaitPG(test, work, fixture.observer, holderPID, writerPID, "pg_advisory_xact_lock", writerDone)
	release <- struct{}{}
	authorityScopeDonePG(test, held, holderDone)
	authorityScopeDonePG(test, work, writerDone)
	expected := latest
	expected.Nickname = "Updated after identity"
	updated := <-users
	if !reflect.DeepEqual(updated, expected) {
		test.Fatal("profile method overwrote identity or preferences committed during its wait", updated, expected)
	}
	stored, err := storage.GetUser(fixture.ctx, before.ID)
	if err != nil || !reflect.DeepEqual(stored, expected) {
		test.Fatal("post-wait profile response did not match storage", stored, expected, err)
	}
}

func TestPGUserPreferencesRollbackAfterRealUpdateReturnsZero(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	before := userPreferencesUserPG(test, fixture.ctx, storage)
	pause := &userPreferencesPausePG{ready: make(chan context.Context, 1), release: make(chan struct{}, 1)}
	defer close(pause.release)
	writer := authorityScopePeerPG(test, storage, "", pause)
	work, cancel, completed, users := userPreferencesRunPG(test, fixture.ctx, writer, before.ID, domain.UserPreferencesPatch{Nickname: userPreferencesStringPG("Must roll back"), Locale: userPreferencesStringPG("ko")})
	userPreferencesPausedPG(test, work, pause, completed)
	visible, err := storage.GetUser(work, before.ID)
	if err != nil || !reflect.DeepEqual(visible, before) {
		test.Fatal("profile mutation became visible before its outer commit", visible, before, err)
	}
	cancel()
	select {
	case err := <-completed:
		if err == nil {
			test.Fatal("cancelled profile UPDATE was acknowledged")
		}
	case <-fixture.ctx.Done():
		test.Fatal("cancelled profile method did not release its transaction", fixture.ctx.Err())
	}
	if updated := <-users; !reflect.DeepEqual(updated, domain.User{}) {
		test.Fatal("rolled-back profile UPDATE exposed a success-shaped user", updated)
	}
	stored, err := storage.GetUser(fixture.ctx, before.ID)
	if err != nil || !reflect.DeepEqual(stored, before) {
		test.Fatal("cancelled profile UPDATE leaked a preference mutation", stored, before, err)
	}
	updated, err := storage.UpdateUserPreferences(fixture.ctx, before.ID, domain.UserPreferencesPatch{Locale: userPreferencesStringPG("ko")})
	expected := before
	expected.Locale = "ko"
	if err != nil || !reflect.DeepEqual(updated, expected) {
		test.Fatal("rollback retained a user lock or poisoned later preference updates", updated, expected, err)
	}
}

func TestPGUserPreferencesInvalidDomainDataReturnsZeroWithoutWrites(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	before := userPreferencesUserPG(test, fixture.ctx, storage)
	for _, scenario := range []struct {
		name  string
		id    string
		patch domain.UserPreferencesPatch
		want  error
	}{
		{name: "empty-id", patch: domain.UserPreferencesPatch{}, want: domain.ErrValidation},
		{name: "control-id", id: "invalid\x00user", want: domain.ErrValidation},
		{name: "oversized-id", id: strings.Repeat("user", 129), want: domain.ErrValidation},
		{name: "missing-user", id: domain.NewID("user_"), want: domain.ErrNotFound},
		{name: "unknown-load-mode", id: before.ID, patch: domain.UserPreferencesPatch{LoadMode: userPreferencesStringPG("unsupported")}, want: domain.ErrValidation},
		{name: "unknown-locale", id: before.ID, patch: domain.UserPreferencesPatch{Locale: userPreferencesStringPG("unsupported")}, want: domain.ErrValidation},
		{name: "remote-avatar", id: before.ID, patch: domain.UserPreferencesPatch{Avatar: userPreferencesStringPG("https://example.test/tracking.png")}, want: domain.ErrValidation},
		{name: "active-avatar", id: before.ID, patch: domain.UserPreferencesPatch{Avatar: userPreferencesStringPG("data:image/svg+xml;base64,PHN2Zy8+")}, want: domain.ErrValidation},
		{name: "malformed-avatar", id: before.ID, patch: domain.UserPreferencesPatch{Avatar: userPreferencesStringPG("data:image/png;base64,not-base64")}, want: domain.ErrValidation},
		{name: "oversized-avatar", id: before.ID, patch: domain.UserPreferencesPatch{Avatar: userPreferencesStringPG("data:image/png;base64," + strings.Repeat("A", 700_001))}, want: domain.ErrValidation},
		{name: "mixed-valid-and-invalid", id: before.ID, patch: domain.UserPreferencesPatch{Nickname: userPreferencesStringPG("Must not persist"), Locale: userPreferencesStringPG("unsupported")}, want: domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			updated, err := storage.UpdateUserPreferences(fixture.ctx, scenario.id, scenario.patch)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(updated, domain.User{}) {
				test.Fatal("invalid profile input was not rejected with zero output", updated, err, scenario.want)
			}
			stored, err := storage.GetUser(fixture.ctx, before.ID)
			if err != nil || !reflect.DeepEqual(stored, before) {
				test.Fatal("rejected profile input changed the stored user", stored, before, err)
			}
		})
	}
	if _, err := storage.pool.Exec(fixture.ctx, `UPDATE users SET avatar=$2 WHERE id=$1`, before.ID, "data:image/svg+xml;base64,PHN2Zy8+"); err != nil {
		test.Fatal(err)
	}
	var poisonedBefore string
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT row_to_json(users)::text FROM users WHERE id=$1`, before.ID).Scan(&poisonedBefore); err != nil {
		test.Fatal(err)
	}
	updated, err := storage.UpdateUserPreferences(fixture.ctx, before.ID, domain.UserPreferencesPatch{Nickname: userPreferencesStringPG("Must not repair invalid identity")})
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(updated, domain.User{}) {
		test.Fatal("invalid stored domain data escaped preference validation", updated, err)
	}
	var poisonedAfter string
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT row_to_json(users)::text FROM users WHERE id=$1`, before.ID).Scan(&poisonedAfter); err != nil || poisonedAfter != poisonedBefore {
		test.Fatal("failed stored-domain validation leaked a partial preference write", poisonedBefore, poisonedAfter, err)
	}
}

func TestPGUserPreferencesForeignReadOnlyAndIncompatibleTransactionsReject(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	before := userPreferencesUserPG(test, fixture.ctx, storage)
	record, repo := authorityScopeRepositoryPG(test, fixture.ctx, storage)
	foreign := authorityScopePeerPG(test, storage, "", nil)
	patch := domain.UserPreferencesPatch{Nickname: userPreferencesStringPG("Must not escape transaction")}
	for _, outer := range []struct {
		name  string
		enter func(context.Context, func(context.Context) error) error
	}{
		{name: "read-only", enter: storage.WithinReadSnapshot},
		{name: "metadata", enter: func(ctx context.Context, callback func(context.Context) error) error {
			return storage.WithinRepositoryMetadata(ctx, record.ID, callback)
		}},
		{name: "repository", enter: func(ctx context.Context, callback func(context.Context) error) error {
			return storage.WithinRepository(ctx, repo, callback)
		}},
	} {
		test.Run(outer.name, func(test *testing.T) {
			if err := outer.enter(fixture.ctx, func(bound context.Context) error {
				for _, current := range []*PostgresStore{storage, foreign} {
					updated, err := current.UpdateUserPreferences(bound, before.ID, patch)
					if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(updated, domain.User{}) {
						return fmt.Errorf("incompatible profile context returned user=%+v error=%v", updated, err)
					}
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			stored, err := storage.GetUser(fixture.ctx, before.ID)
			if err != nil || !reflect.DeepEqual(stored, before) {
				test.Fatal("rejected transaction escaped into a private profile write", stored, before, err)
			}
		})
	}
	rollback := errors.New("synthetic outer identity rollback")
	err := storage.WithinIdentity(fixture.ctx, func(bound context.Context) error {
		updated, err := storage.UpdateUserPreferences(bound, before.ID, patch)
		expected := before
		expected.Nickname = *patch.Nickname
		if err != nil || !reflect.DeepEqual(updated, expected) {
			return fmt.Errorf("same-owner identity preferences did not join their transaction: %+v %v", updated, err)
		}
		if visible, err := foreign.GetUser(fixture.ctx, before.ID); err != nil || !reflect.DeepEqual(visible, before) {
			return fmt.Errorf("nested preferences committed before their outer identity transaction: %+v %v", visible, err)
		}
		if updated, err := foreign.UpdateUserPreferences(bound, before.ID, patch); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(updated, domain.User{}) {
			return fmt.Errorf("foreign identity scope escaped its existing transaction: %+v %v", updated, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		test.Fatal("nested identity preferences lost their outer rollback", err)
	}
	stored, err := storage.GetUser(fixture.ctx, before.ID)
	if err != nil || !reflect.DeepEqual(stored, before) {
		test.Fatal("nested identity rollback leaked a partial preference write", stored, before, err)
	}
}
