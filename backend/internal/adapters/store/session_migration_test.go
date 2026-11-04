package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func seedSessionMigration(test *testing.T, storage outbound.RepositoryStore) domain.Session {
	test.Helper()
	ctx := context.Background()
	user := domain.User{ID: domain.NewID("migration-user-"), Username: domain.NewID("migration-")}
	if err := storage.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	session := domain.Session{Token: domain.NewID("sess_"), UserID: user.ID, Kind: "web", Label: "migration-device", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC()}
	if err := storage.CreateSession(ctx, session); err != nil {
		test.Fatal(err)
	}
	stored, err := storage.GetSession(ctx, session.Token)
	if err != nil {
		test.Fatal(err)
	}
	return stored
}

func requireSessionRevoked(test *testing.T, storage outbound.RepositoryStore, token string) {
	test.Helper()
	ctx := context.Background()
	for _, key := range []string{token, domain.HashToken(token)} {
		if _, err := storage.GetSession(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			test.Fatalf("revoked session still stored: %v", err)
		}
	}
	if _, err := storage.MigrateSession(ctx, token); !errors.Is(err, domain.ErrNotFound) {
		test.Fatalf("revoked session migrated: %v", err)
	}
}

func checkConcurrentSessionMigration(test *testing.T, storage, peer outbound.RepositoryStore) {
	test.Helper()
	session := seedSessionMigration(test, storage)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 24)
	for index := 0; index < cap(results); index++ {
		current := storage
		if index%2 == 0 {
			current = peer
		}
		go func() {
			<-start
			migrated, err := current.MigrateSession(ctx, session.Token)
			if err == nil && (migrated.CredentialID != session.CredentialID || migrated.Token != domain.HashToken(session.Token) || migrated.UserID != session.UserID || !migrated.CreatedAt.Equal(session.CreatedAt)) {
				err = errors.New("concurrent migration changed identity")
			}
			results <- err
		}()
	}
	close(start)
	for index := 0; index < cap(results); index++ {
		if err := <-results; err != nil {
			test.Error(err)
		}
	}
	if _, err := storage.GetSession(ctx, session.Token); !errors.Is(err, domain.ErrNotFound) {
		test.Fatalf("legacy session remains after concurrent migration: %v", err)
	}
	if err := peer.DeleteSession(ctx, session.Token); err != nil {
		test.Fatal(err)
	}
	requireSessionRevoked(test, storage, session.Token)
}

func TestFSSessionMigrationConcurrent(test *testing.T) {
	directory := test.TempDir()
	checkConcurrentSessionMigration(test, NewFSStore(directory), NewFSStore(directory))
}

func checkSessionRevocationRemovesExistingAliases(test *testing.T, storage outbound.RepositoryStore) {
	test.Helper()
	ctx := context.Background()
	legacy := seedSessionMigration(test, storage)
	hashed := legacy
	hashed.Token = domain.HashToken(legacy.Token)
	if err := storage.CreateSession(ctx, hashed); err != nil {
		test.Fatal(err)
	}
	if err := storage.DeleteSession(ctx, hashed.Token); err != nil {
		test.Fatal(err)
	}
	requireSessionRevoked(test, storage, legacy.Token)
}

func TestFSSessionRevocationRemovesExistingAliases(test *testing.T) {
	checkSessionRevocationRemovesExistingAliases(test, NewFSStore(test.TempDir()))
}

func TestFSSessionMigrationRevocationConcurrent(test *testing.T) {
	directory := test.TempDir()
	storage, peer := NewFSStore(directory), NewFSStore(directory)
	ctx := context.Background()
	for index := 0; index < 32; index++ {
		session := seedSessionMigration(test, storage)
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			if _, err := storage.MigrateSession(ctx, session.Token); err != nil && !errors.Is(err, domain.ErrNotFound) {
				test.Error(err)
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			if err := peer.DeleteSession(ctx, session.Token); err != nil {
				test.Error(err)
			}
		}()
		close(start)
		workers.Wait()
		requireSessionRevoked(test, storage, session.Token)
	}
}

func TestFSSessionMigrationRejectsInvalidKeys(test *testing.T) {
	storage := NewFSStore(test.TempDir())
	for _, token := range []string{"../outside", "", domain.HashToken("session")} {
		if _, err := storage.MigrateSession(context.Background(), token); !errors.Is(err, domain.ErrValidation) {
			test.Fatalf("invalid migration key accepted: %v", err)
		}
	}
}
