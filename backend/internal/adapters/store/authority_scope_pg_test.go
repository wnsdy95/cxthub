//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func authorityScopePG(test *testing.T) (*processAdmissionPGFixture, *PostgresStore) {
	test.Helper()
	fixture, storage := migrationVerificationStorePG(test)
	if _, err := storage.ApplyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal(err)
	}
	return fixture, storage
}

func authorityScopePeerPG(test *testing.T, storage *PostgresStore, isolation string, trace pgx.QueryTracer) *PostgresStore {
	test.Helper()
	configuration := storage.pool.Config().Copy()
	configuration.MaxConns, configuration.MinConns, configuration.MinIdleConns = 1, 0, 0
	configuration.ConnConfig.RuntimeParams["application_name"] = domain.NewID("authority_scope_")
	if isolation != "" {
		configuration.ConnConfig.RuntimeParams["default_transaction_isolation"] = isolation
	}
	configuration.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(test.Context(), configuration)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}
}

func authorityScopePIDPG(test *testing.T, ctx context.Context, storage *PostgresStore) uint32 {
	test.Helper()
	var backendPID uint32
	if err := storage.pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		test.Fatal(err)
	}
	return backendPID
}

func authorityScopeRepositoryPG(test *testing.T, ctx context.Context, storage *PostgresStore) (domain.Repository, domain.ContentHash) {
	test.Helper()
	owner := domain.User{ID: domain.NewID("user_"), Email: "authority@example.test", Name: "Synthetic authority owner", Username: domain.NewID("authority")}
	if err := storage.UpsertUser(ctx, owner); err != nil {
		test.Fatal(err)
	}
	record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: owner.Username, Name: "Synthetic authority scope", Slug: "scope", Visibility: domain.VisibilityPrivate}
	if err := storage.CreateRepository(ctx, record); err != nil {
		test.Fatal(err)
	}
	repo := domain.HashContent([]byte(record.ID))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
		test.Fatal(err)
	}
	record, err := storage.GetRepository(ctx, record.ID)
	if err != nil {
		test.Fatal(err)
	}
	return record, repo
}

func authorityScopeRunPG(test *testing.T, parent context.Context, operation func(context.Context) error) (context.Context, context.CancelFunc, <-chan error) {
	test.Helper()
	work, cancel := context.WithTimeout(parent, 5*time.Second)
	completed := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		completed <- operation(work)
	}()
	test.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			test.Error("authority scope operation did not join after cancellation")
		}
	})
	return work, cancel, completed
}

func authorityScopeDonePG(test *testing.T, ctx context.Context, completed <-chan error) {
	test.Helper()
	select {
	case err := <-completed:
		if err != nil {
			test.Fatal(err)
		}
	case <-ctx.Done():
		test.Fatal("authority scope operation did not complete within its finite barrier", ctx.Err())
	}
}

func authorityScopeReadyPG(test *testing.T, ctx context.Context, ready <-chan struct{}, completed <-chan error) {
	test.Helper()
	select {
	case <-ready:
	case err := <-completed:
		test.Fatal("authority scope holder exited before reaching its barrier", err)
	case <-ctx.Done():
		test.Fatal("authority scope holder did not reach its barrier", ctx.Err())
	}
}

func authorityScopeWaitPG(test *testing.T, ctx context.Context, observer *pgx.Conn, blockerPID, waiterPID uint32, statement string, completed <-chan error) {
	test.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE pid=$1 AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0
 AND ($2::bigint=0 OR $2::integer=ANY(pg_blocking_pids(pid)))
 AND query ILIKE '%' || $3 || '%')`, waiterPID, blockerPID, statement).Scan(&waiting); err != nil {
			test.Fatal("could not observe the actual authority lock wait", err)
		}
		if waiting {
			return
		}
		select {
		case err := <-completed:
			test.Fatal("operation bypassed the retained authority lock", err)
		case <-ctx.Done():
			test.Fatal("operation never reached the expected PostgreSQL lock wait", ctx.Err())
		case <-ticker.C:
		}
	}
}

func authorityScopeOperationPG(ctx context.Context, storage *PostgresStore, kind string, record domain.Repository, repo domain.ContentHash, finish func(context.Context) error) error {
	if kind == "metadata" {
		return storage.WithinRepositoryMetadata(ctx, record.ID, func(bound context.Context) error {
			current, err := storage.GetRepository(bound, record.ID)
			if err != nil {
				return err
			}
			current.Archived = true
			if err := storage.CreateRepository(bound, current); err != nil {
				return err
			}
			return finish(bound)
		})
	}
	return storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := finish(bound); err != nil {
			return err
		}
		return storage.PutSecretsEnvelope(bound, repo, []byte(`{"synthetic":"authority scope"}`))
	})
}

func TestPGAuthorityScopeMetadataAndRepositoryIsolation(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	for _, order := range [][2]string{{"metadata", "repository"}, {"repository", "metadata"}, {"metadata", "metadata"}} {
		for _, sameRepository := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s-before-%s/same=%t", order[0], order[1], sameRepository), func(test *testing.T) {
				heldRecord, heldRepo := authorityScopeRepositoryPG(test, fixture.ctx, storage)
				nextRecord, nextRepo := heldRecord, heldRepo
				if !sameRepository {
					nextRecord, nextRepo = authorityScopeRepositoryPG(test, fixture.ctx, storage)
				}
				holder := authorityScopePeerPG(test, storage, "", nil)
				writer := authorityScopePeerPG(test, storage, "", nil)
				holderPID := authorityScopePIDPG(test, fixture.ctx, holder)
				writerPID := authorityScopePIDPG(test, fixture.ctx, writer)
				ready, release := make(chan struct{}), make(chan struct{}, 1)
				defer close(release)
				held, _, holderDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
					return authorityScopeOperationPG(ctx, holder, order[0], heldRecord, heldRepo, func(bound context.Context) error {
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
				if order[0] == "metadata" {
					var sharedOnly bool
					if err := fixture.observer.QueryRow(held, `SELECT
 EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND granted AND mode='ShareLock')
 AND NOT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND mode='ExclusiveLock')`, holderPID).Scan(&sharedOnly); err != nil || !sharedOnly {
						test.Fatal("metadata scope did not retain only the shared global authority lock", sharedOnly, err)
					}
				}
				work, _, writerDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
					return authorityScopeOperationPG(ctx, writer, order[1], nextRecord, nextRepo, func(context.Context) error { return nil })
				})
				if sameRepository {
					authorityScopeWaitPG(test, work, fixture.observer, holderPID, writerPID, "repositories", writerDone)
				} else {
					authorityScopeDonePG(test, work, writerDone)
					if order[1] == "metadata" {
						current, err := storage.GetRepository(work, nextRecord.ID)
						if err != nil || !current.Archived {
							test.Fatal("unrelated metadata did not commit while the first scope remained held", current, err)
						}
					} else if _, err := storage.GetSecretsEnvelope(work, nextRepo); err != nil {
						test.Fatal("unrelated repository write did not commit while metadata remained held", err)
					}
				}
				release <- struct{}{}
				authorityScopeDonePG(test, held, holderDone)
				if sameRepository {
					authorityScopeDonePG(test, work, writerDone)
				}
			})
		}
	}
}

func TestPGAuthorityScopeWideIdentityStillExcludesMetadata(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	for _, identityFirst := range []bool{true, false} {
		test.Run(fmt.Sprintf("identity-first=%t", identityFirst), func(test *testing.T) {
			record, _ := authorityScopeRepositoryPG(test, fixture.ctx, storage)
			holder := authorityScopePeerPG(test, storage, "", nil)
			writer := authorityScopePeerPG(test, storage, "", nil)
			holderPID := authorityScopePIDPG(test, fixture.ctx, holder)
			writerPID := authorityScopePIDPG(test, fixture.ctx, writer)
			ready, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			operation := func(ctx context.Context, current *PostgresStore, identity bool, finish func(context.Context) error) error {
				if identity {
					return current.WithinIdentity(ctx, finish)
				}
				return current.WithinRepositoryMetadata(ctx, record.ID, finish)
			}
			held, _, holderDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
				return operation(ctx, holder, identityFirst, func(bound context.Context) error {
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
			work, _, writerDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
				return operation(ctx, writer, !identityFirst, func(context.Context) error { return nil })
			})
			authorityScopeWaitPG(test, work, fixture.observer, holderPID, writerPID, "pg_advisory_xact_lock", writerDone)
			release <- struct{}{}
			authorityScopeDonePG(test, held, holderDone)
			authorityScopeDonePG(test, work, writerDone)
		})
	}
}

func TestPGAuthorityScopeRowLockModesBeforeMutableWork(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	for _, scenario := range []string{"metadata-update", "repository-metadata-share", "repository-context-share"} {
		test.Run(scenario, func(test *testing.T) {
			record, repo := authorityScopeRepositoryPG(test, fixture.ctx, storage)
			holder := authorityScopePeerPG(test, storage, "", nil)
			probe := authorityScopePeerPG(test, storage, "", nil)
			holderPID := authorityScopePIDPG(test, fixture.ctx, holder)
			probePID := authorityScopePIDPG(test, fixture.ctx, probe)
			ready, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			held, _, holderDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
				finish := func(bound context.Context) error {
					close(ready)
					select {
					case <-release:
						return nil
					case <-bound.Done():
						return bound.Err()
					}
				}
				if scenario == "metadata-update" {
					return holder.WithinRepositoryMetadata(ctx, record.ID, finish)
				}
				return holder.WithinRepository(ctx, repo, finish)
			})
			authorityScopeReadyPG(test, held, ready, holderDone)
			statement, target := `SELECT id FROM repositories WHERE id=$1 FOR NO KEY UPDATE`, record.ID
			relation := "repositories"
			if scenario == "metadata-update" {
				statement = `SELECT id FROM repositories WHERE id=$1 FOR KEY SHARE`
			} else if scenario == "repository-context-share" {
				statement, target = `SELECT id FROM repos WHERE id=$1 FOR NO KEY UPDATE`, string(repo)
				relation = "repos"
			}
			work, _, probeDone := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
				transaction, err := probe.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				if err != nil {
					return err
				}
				defer rollbackPG(transaction)
				var observed string
				if err := transaction.QueryRow(ctx, statement, target).Scan(&observed); err != nil {
					return err
				}
				return transaction.Commit(ctx)
			})
			authorityScopeWaitPG(test, work, fixture.observer, holderPID, probePID, relation, probeDone)
			release <- struct{}{}
			authorityScopeDonePG(test, held, holderDone)
			authorityScopeDonePG(test, work, probeDone)
		})
	}
}

func TestPGAuthorityScopeMetadataRollbackAndContextRejection(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	record, repo := authorityScopeRepositoryPG(test, fixture.ctx, storage)
	other, _ := authorityScopeRepositoryPG(test, fixture.ctx, storage)
	foreign := authorityScopePeerPG(test, storage, "", nil)
	rollback := errors.New("synthetic metadata rollback")
	called := false
	forbidden := func(context.Context) error {
		called = true
		return nil
	}
	err := storage.WithinRepositoryMetadata(fixture.ctx, record.ID, func(bound context.Context) error {
		nested := false
		if err := storage.WithinRepositoryMetadata(bound, record.ID, func(inner context.Context) error {
			nested = inner == bound
			return nil
		}); err != nil || !nested {
			return fmt.Errorf("matching metadata scope did not join: %v", err)
		}
		for _, attempt := range []func() error{
			func() error { return storage.WithinRepositoryMetadata(bound, other.ID, forbidden) },
			func() error { return foreign.WithinRepositoryMetadata(bound, record.ID, forbidden) },
			func() error { return storage.WithinIdentity(bound, forbidden) },
			func() error { return storage.WithinRepository(bound, repo, forbidden) },
		} {
			if err := attempt(); !errors.Is(err, domain.ErrConflict) {
				return fmt.Errorf("incompatible metadata nesting was not rejected: %v", err)
			}
		}
		changed := record
		changed.Archived = true
		if err := storage.CreateRepository(bound, changed); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || called {
		test.Fatal("metadata rollback or nested rejection contract failed", err, called)
	}
	current, err := storage.GetRepository(fixture.ctx, record.ID)
	if err != nil || current != record {
		test.Fatal("metadata callback failure leaked a repository mutation", current, record, err)
	}
	if err := storage.WithinReadSnapshot(fixture.ctx, func(bound context.Context) error {
		for _, current := range []*PostgresStore{storage, foreign} {
			if err := current.WithinRepositoryMetadata(bound, record.ID, forbidden); !errors.Is(err, domain.ErrConflict) {
				return fmt.Errorf("read-only metadata scope was not rejected: %v", err)
			}
		}
		return nil
	}); err != nil || called {
		test.Fatal("read-only scope reached mutable metadata", err, called)
	}
	if err := storage.WithinRepository(fixture.ctx, repo, func(bound context.Context) error {
		if err := storage.WithinRepositoryMetadata(bound, record.ID, forbidden); !errors.Is(err, domain.ErrConflict) {
			return fmt.Errorf("repository scope escalated to metadata: %v", err)
		}
		return nil
	}); err != nil || called {
		test.Fatal(err, called)
	}
	if err := storage.WithinIdentity(fixture.ctx, func(bound context.Context) error {
		return storage.WithinRepositoryMetadata(bound, record.ID, func(inner context.Context) error {
			if inner != bound {
				return errors.New("broad identity metadata nesting escaped its existing transaction")
			}
			return nil
		})
	}); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepositoryMetadata(fixture.ctx, domain.NewID("ws_"), forbidden); !errors.Is(err, domain.ErrNotFound) || called {
		test.Fatal("missing metadata target reached its callback", err, called)
	}
	if err := storage.WithinRepositoryMetadata(fixture.ctx, record.ID, func(context.Context) error { return nil }); err != nil {
		test.Fatal("rollback retained the metadata lock", err)
	}
}

func TestPGAuthorityScopeLatestAuthorityAfterInheritedIsolationWait(test *testing.T) {
	fixture, storage := authorityScopePG(test)
	for _, isolation := range []string{"repeatable read", "serializable"} {
		for _, scenario := range []string{"identity-global", "metadata-global", "metadata-row"} {
			test.Run(isolation+"/"+scenario, func(test *testing.T) {
				record, _ := authorityScopeRepositoryPG(test, fixture.ctx, storage)
				member := domain.User{ID: domain.NewID("user_"), Email: "member@example.test"}
				if err := storage.UpsertUser(fixture.ctx, member); err != nil {
					test.Fatal(err)
				}
				if err := storage.AddMember(fixture.ctx, domain.Membership{RepositoryID: record.ID, UserID: member.ID, Role: domain.RoleMaintainer}); err != nil {
					test.Fatal(err)
				}
				reader := authorityScopePeerPG(test, storage, isolation, nil)
				readerPID := authorityScopePIDPG(test, fixture.ctx, reader)
				blocker, err := storage.pool.BeginTx(fixture.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(blocker)
				statement := "pg_advisory_xact_lock"
				if scenario == "metadata-row" {
					statement = "repositories"
					_, err = blocker.Exec(fixture.ctx, `SELECT id FROM repositories WHERE id=$1 FOR UPDATE`, record.ID)
				} else {
					_, err = blocker.Exec(fixture.ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cxt-identity-access',0))`)
				}
				if err != nil {
					test.Fatal(err)
				}
				work, _, completed := authorityScopeRunPG(test, fixture.ctx, func(ctx context.Context) error {
					read := func(bound context.Context) error {
						var actualIsolation string
						if err := reader.db(bound).QueryRow(bound, `SHOW transaction_isolation`).Scan(&actualIsolation); err != nil {
							return err
						}
						if actualIsolation != "read committed" {
							return fmt.Errorf("authority scope inherited stale-snapshot isolation %q", actualIsolation)
						}
						current, err := reader.GetRepository(bound, record.ID)
						if err != nil || !current.Archived {
							return fmt.Errorf("post-wait repository authority was stale: %+v %v", current, err)
						}
						members, err := reader.ListMembers(bound, record.ID)
						if err != nil || len(members) != 1 || members[0].UserID != member.ID || members[0].Role != domain.RoleViewer {
							return fmt.Errorf("post-wait member authority was stale: %+v %v", members, err)
						}
						return nil
					}
					if scenario == "identity-global" {
						return reader.WithinIdentity(ctx, read)
					}
					return reader.WithinRepositoryMetadata(ctx, record.ID, read)
				})
				authorityScopeWaitPG(test, work, fixture.observer, blocker.Conn().PgConn().PID(), readerPID, statement, completed)
				if _, err := blocker.Exec(work, `UPDATE repositories SET archived=true WHERE id=$1`, record.ID); err != nil {
					test.Fatal(err)
				}
				if _, err := blocker.Exec(work, `UPDATE memberships SET role=$3 WHERE repository_id=$1 AND user_id=$2`, record.ID, member.ID, string(domain.RoleViewer)); err != nil {
					test.Fatal(err)
				}
				if err := blocker.Commit(work); err != nil {
					test.Fatal(err)
				}
				authorityScopeDonePG(test, work, completed)
				var defaultIsolation string
				if err := reader.pool.QueryRow(work, `SHOW default_transaction_isolation`).Scan(&defaultIsolation); err != nil || defaultIsolation != isolation {
					test.Fatal("authority transaction changed the inherited session default", defaultIsolation, err)
				}
			})
		}
	}
}
