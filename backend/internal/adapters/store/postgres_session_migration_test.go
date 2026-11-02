//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func waitSessionMigrationBlocked(test *testing.T, ctx context.Context, storage *PostgresStore, blocker uint32, done <-chan error) {
	test.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := storage.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			test.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case err := <-done:
			test.Fatalf("operation crossed held session transition: %v", err)
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestPGSessionMigrationRevocationOrders(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	peer, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	for _, order := range []string{"migration-first", "revocation-first", "migration-rollback", "revocation-rollback", "migration-first-hash", "revocation-first-hash", "migration-rollback-hash", "revocation-rollback-hash"} {
		test.Run(order, func(test *testing.T) {
			session := seedSessionMigration(test, storage)
			mode := strings.TrimSuffix(order, "-hash")
			revocationKey := session.Token
			if strings.HasSuffix(order, "-hash") {
				revocationKey = domain.HashToken(session.Token)
			}
			tx, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(tx)
			bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: storage})
			migrationFirst := mode == "migration-first" || mode == "migration-rollback"
			if migrationFirst {
				if _, err := storage.MigrateSession(bound, session.Token); err != nil {
					test.Fatal(err)
				}
			} else if err := storage.DeleteSession(bound, revocationKey); err != nil {
				test.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if migrationFirst {
					done <- peer.DeleteSession(ctx, revocationKey)
					return
				}
				_, err := peer.MigrateSession(ctx, session.Token)
				done <- err
			}()
			waitSessionMigrationBlocked(test, ctx, storage, tx.Conn().PgConn().PID(), done)
			if mode == "migration-rollback" || mode == "revocation-rollback" {
				err = tx.Rollback(ctx)
			} else {
				err = tx.Commit(ctx)
			}
			if err != nil {
				test.Fatal(err)
			}
			select {
			case err = <-done:
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			if mode == "revocation-first" {
				if !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("deleted legacy row migrated: %v", err)
				}
			} else if err != nil {
				test.Fatal(err)
			}
			if mode == "revocation-rollback" {
				migrated, err := storage.GetSession(ctx, domain.HashToken(session.Token))
				if err != nil || migrated.CredentialID != session.CredentialID {
					test.Fatalf("rolled-back revocation lost credential identity: %v", err)
				}
				if err := storage.DeleteSession(ctx, session.Token); err != nil {
					test.Fatal(err)
				}
			}
			requireSessionRevoked(test, storage, session.Token)
		})
	}
}

func TestPGSessionMigrationConcurrent(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	peer, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	checkConcurrentSessionMigration(test, storage, peer)
}

func TestPGSessionRevocationRemovesExistingAliases(test *testing.T) {
	storage, _ := chunkReusePG(test)
	checkSessionRevocationRemovesExistingAliases(test, storage)
}

func TestPGSessionCapabilityKeyAndIndexedRevocation(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	for _, token := range []string{"sess_" + strings.Repeat("a", 32), "sess_cli_" + strings.Repeat("b", 32), domain.HashToken("fixture"), "", `sess_\invalid`} {
		var actual *string
		if err := storage.pool.QueryRow(ctx, `SELECT `+sessionCapabilitySQL+` FROM (VALUES($1::text)) AS entry(token)`, token).Scan(&actual); err != nil {
			test.Fatal(err)
		}
		if domain.ValidateStoredSessionToken(token) != nil {
			if actual != nil {
				test.Fatal("invalid legacy token produced a capability key")
			}
			continue
		}
		expected := token
		if !strings.HasPrefix(token, "tkh_") {
			expected = domain.HashToken(token)
		}
		if actual == nil || *actual != expected {
			test.Fatal("database capability key differs from Go token hash")
		}
	}
	session := seedSessionMigration(test, storage)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO sessions(token,user_id,expires_at,kind)
 SELECT 'tkh_' || repeat(md5($1 || ordinal::text),2),$1,clock_timestamp()+interval '1 hour','web'
 FROM generate_series(1,4096) AS ordinal`, session.UserID); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM sessions WHERE user_id=$1`, session.UserID); err != nil {
			test.Error(err)
		}
	})
	if _, err := storage.pool.Exec(ctx, `ANALYZE sessions`); err != nil {
		test.Fatal(err)
	}
	var plan string
	if err := storage.pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) DELETE FROM sessions WHERE `+sessionCapabilitySQL+`=$1`, domain.HashToken(session.Token)).Scan(&plan); err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(plan, "sessions_capability_hash_idx") || strings.Contains(plan, `"Node Type": "Seq Scan"`) {
		test.Fatalf("revocation does not use its canonical-key index: %s", plan)
	}
}

func TestPGSessionMigrationRollbackPreservesIdentity(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	session := seedSessionMigration(test, storage)
	rollback := errors.New("rollback migration")
	err := storage.WithinIdentity(ctx, func(ctx context.Context) error {
		if _, err := storage.MigrateSession(ctx, session.Token); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		test.Fatal(err)
	}
	stored, err := storage.GetSession(ctx, session.Token)
	if err != nil || stored.CredentialID != session.CredentialID {
		test.Fatalf("rollback changed legacy credential: %v", err)
	}
	if _, err := storage.GetSession(ctx, domain.HashToken(session.Token)); !errors.Is(err, domain.ErrNotFound) {
		test.Fatalf("migration escaped transaction rollback: %v", err)
	}
}
