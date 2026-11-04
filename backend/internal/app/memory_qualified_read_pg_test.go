//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func qualifiedMemoryFixturePG(test *testing.T) (*Service, *store.PostgresStore, *pgxpool.Pool, domain.ContentHash, domain.EffectiveMemoryRequest, *PreparedMemoryRead) {
	test.Helper()
	service, storage, repo := collaborationPG(test)
	ctx := systemTestContext()
	connection, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(connection.Close)
	snapshot := collaborationSnapshot(test, storage, repo, "qualified memory")
	digest := capturedMemoryDigestPG(snapshot, "qualified source")
	digest.Summary = strings.Repeat("verified source "+string(repo), 900)
	hash, err := service.PutMemoryDigestCAS(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, MemoryHash: hash, CodeCommit: effectiveOID(3)}, Limit: 1}
	prepared, err := service.PrepareMemoryRead(ctx, repo, request, nil)
	if err != nil {
		test.Fatal("prepare", err)
	}
	if len(prepared.qualification.Witnesses) != 1 || len(prepared.qualification.Witnesses[0].Components) == 0 {
		test.Fatal("fixture requires a witnessed chunked memory")
	}
	return service, storage, connection, repo, request, prepared
}

func TestPGQualifiedMemoryRestartCurrentChecksAndRecertification(test *testing.T) {
	service, storage, connection, repo, request, prepared := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	identifier, err := service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour))
	if err != nil {
		test.Fatal("publish", err)
	}
	if duplicate, err := service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(2*time.Hour)); err != nil || duplicate != identifier {
		test.Fatal("duplicate publication changed receipt", err)
	}
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewService(reopened, reopened, auth.NewTeamTokenAuth(), gitengine.NewEngine(reopened), reopened)
	for page := 0; ; page++ {
		want, err := service.QueryEffectiveMemory(ctx, repo, request)
		if err != nil {
			test.Fatal(err)
		}
		actual, err := restarted.QueryQualifiedMemory(ctx, repo, identifier, request)
		if err != nil || !reflect.DeepEqual(actual, want) {
			test.Fatalf("qualified page differs after restart: %v\ngot=%+v\nwant=%+v", err, actual, want)
		}
		if actual.NextCursor == "" {
			break
		}
		if page > 30 || len(actual.Items) == 0 {
			test.Fatal("qualified cursor failed to advance")
		}
		request.Cursor = actual.NextCursor
	}
	request.Cursor = ""
	denied := inbound.WithDocumentReadAuthorizer(ctx, func(context.Context, domain.ContentHash) (context.Context, error) {
		return nil, domain.ErrForbidden
	})
	if page, err := restarted.QueryQualifiedMemory(denied, repo, identifier, request); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) {
		test.Fatal("receipt bypassed revoked access", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		_, err := service.QueryQualifiedMemory(bound, repo, identifier, request)
		if !errors.Is(err, domain.ErrConflict) {
			test.Fatal("current query borrowed an existing snapshot", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	component := prepared.qualification.Witnesses[0].Components[0].Hash
	if _, err := connection.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, component); err != nil {
		test.Fatal(err)
	}
	if _, err := service.QueryQualifiedMemory(ctx, repo, identifier, request); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("same-generation component rewrite reused old receipt", err)
	}
	fresh, err := service.PrepareMemoryRead(ctx, repo, request, nil)
	if err != nil || fresh.output.ID != prepared.output.ID || fresh.input.ID != prepared.input.ID {
		test.Fatal("physical-only rewrite changed logical result", err)
	}
	newID, err := service.PublishPreparedMemoryRead(ctx, fresh, time.Now().Add(time.Hour))
	if err != nil || newID == identifier {
		test.Fatal("fresh byte verification did not produce distinct receipt", err)
	}
	if _, err := restarted.QueryQualifiedMemory(ctx, repo, newID, request); err != nil {
		test.Fatal("fresh receipt unreadable", err)
	}
	if _, err := connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2 AND kind='memory_chunk'`, repo, component); err != nil {
		test.Fatal(err)
	}
	if _, err := restarted.QueryQualifiedMemory(ctx, repo, newID, request); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("current read survived component grant removal", err)
	}
	if _, err := ReadMemoryReadPage(ctx, reopened, repo, prepared.output.ID, request); err != nil {
		test.Fatal("current rejection destroyed historical output", err)
	}
}

func TestPGQualifiedMemoryStalePreparationNeverPublishesCurrentReceipt(test *testing.T) {
	for _, mode := range []string{"bytes", "graph", "ownership", "readonly", "canceled"} {
		test.Run(mode, func(test *testing.T) {
			service, _, connection, repo, _, prepared := qualifiedMemoryFixturePG(test)
			ctx, cancel := context.WithCancel(systemTestContext())
			defer cancel()
			wanted := domain.ErrConflict
			var err error
			switch mode {
			case "bytes":
				_, err = connection.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, prepared.qualification.Witnesses[0].Hash)
			case "graph":
				_, err = connection.Exec(ctx, `UPDATE snapshots SET message=message || ' changed' WHERE repo_id=$1`, repo)
			case "ownership":
				_, err = connection.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory'`, repo)
			case "readonly":
				ctx = outbound.WithDocReadOnly(ctx)
			case "canceled":
				cancel()
				wanted = context.Canceled
			}
			if err != nil {
				test.Fatal(err)
			}
			identifier, err := service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour))
			if !errors.Is(err, wanted) || identifier != "" {
				test.Fatal("failed preparation published qualification", identifier, err)
			}
			var count int
			if err := connection.QueryRow(systemTestContext(), `SELECT count(*) FROM memory_read_qualifications WHERE repo_id=$1`, repo).Scan(&count); err != nil || count != 0 {
				test.Fatal("failed publication leaked receipt", count, err)
			}
		})
	}
}

func TestPGQualifiedMemoryReauthorizesAfterAdmission(test *testing.T) {
	for _, mode := range []string{"read", "nested read"} {
		test.Run(mode, func(test *testing.T) {
			service, storage, _, repo, request, prepared := qualifiedMemoryFixturePG(test)
			ctx := systemTestContext()
			fixture := makeTeamFixture(test, storage)
			if err := storage.AddMember(ctx, domain.Membership{RepositoryID: fixture.repository.ID, UserID: fixture.member.ID, Role: domain.RoleViewer}); err != nil {
				test.Fatal(err)
			}
			identifier, err := service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour))
			if err != nil {
				test.Fatal(err)
			}
			calls := 0
			authorize := func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
				calls++
				if requested != repo || calls > 2 || (calls == 2 && !storage.InReadOnlyTransaction(bound)) {
					return bound, domain.ErrIntegrity
				}
				if _, allowed := fixture.identity.RoleOf(bound, fixture.repository.ID, fixture.member.ID); !allowed {
					return bound, domain.ErrForbidden
				}
				if calls == 1 {
					if err := storage.WithinIdentity(bound, func(writing context.Context) error {
						return storage.RemoveMember(writing, fixture.repository.ID, fixture.member.ID)
					}); err != nil {
						return bound, err
					}
				}
				return bound, nil
			}
			ctx = inbound.WithRepositoryActor(ctx, fixture.member.ID)
			ctx = inbound.WithDocumentReadAuthorizer(ctx, authorize)
			ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, authorize)
			query := func(bound context.Context) error {
				page, err := service.QueryQualifiedMemory(bound, repo, identifier, request)
				if !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) {
					test.Fatal("revoked read returned a page")
				}
				return err
			}
			switch mode {
			case "read":
				err = query(ctx)
			case "nested read":
				err = service.WithMemoryReadResources(ctx, repo, query)
			}
			if !errors.Is(err, domain.ErrForbidden) || calls != 2 {
				test.Fatal("admission authorization was reused across snapshot boundary", calls, err)
			}
		})
	}
}

func TestPGQualifiedMemoryWorkerAndTransportGates(test *testing.T) {
	service, storage, _, repo, request, prepared := qualifiedMemoryFixturePG(test)
	allow := func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
		if storage.InTransaction(bound) || requested != repo {
			return bound, domain.ErrIntegrity
		}
		return bound, nil
	}
	for _, ctx := range []context.Context{context.Background(), inbound.WithRepositoryActor(context.Background(), "user"),
		inbound.WithDocumentReadAuthorizer(systemTestContext(), allow)} {
		if _, err := service.PrepareMemoryRead(ctx, repo, request, nil); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("non-worker prepared derived memory", err)
		}
		if _, err := service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("non-worker published derived memory", err)
		}
	}
	identifier, err := service.PublishPreparedMemoryRead(systemTestContext(), prepared, time.Now().Add(time.Hour))
	if err != nil {
		test.Fatal(err)
	}
	for _, ctx := range []context.Context{context.Background(), inbound.WithRepositoryActor(context.Background(), "user"),
		inbound.WithDocumentReadAuthorizer(systemTestContext(), allow)} {
		page, err := service.QueryQualifiedMemory(ctx, repo, identifier, request)
		if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(page, domain.EffectiveMemoryPage{}) {
			test.Fatal("missing snapshot authorization fell back to admission or system actor", err)
		}
	}
}

func TestPGQualifiedMemoryRootIdentityRoundTrip(test *testing.T) {
	service, _, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := inbound.WithDocumentIdentities(systemTestContext(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if _, err := connection.Exec(ctx, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	prepared, err := service.PrepareMemoryRead(ctx, repo, request, nil)
	if err != nil {
		test.Fatal("root-capable preparation", err)
	}
	identifier, err := service.PublishPreparedMemoryRead(ctx, prepared, time.Now().Add(time.Hour))
	if err != nil {
		test.Fatal("root-capable publication", err)
	}
	storage, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer storage.Close()
	restarted := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if _, err := restarted.QueryQualifiedMemory(ctx, repo, identifier, request); err != nil {
		test.Fatal("root-capable restart read", err)
	}
	if _, err := restarted.QueryQualifiedMemory(systemTestContext(), repo, identifier, request); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("legacy-only peer read root-required repository", err)
	}
	if _, err := restarted.PublishPreparedMemoryRead(systemTestContext(), prepared, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("legacy-only peer published qualification", err)
	}
}
