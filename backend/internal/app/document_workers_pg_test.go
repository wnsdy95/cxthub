//go:build postgres

package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentWorkersPGPorts struct {
	*store.PostgresStore
	observing bool
	claims    [4]int
	reads     int
}

func (storage *documentWorkersPGPorts) observe(ctx context.Context) error {
	if !storage.observing {
		return nil
	}
	if _, system := inbound.RepositoryActor(ctx); !system {
		return domain.ErrUnauthorized
	}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1); err != nil {
		return err
	}
	bound, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		return err
	}
	defer release()
	if bound != ctx {
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *documentWorkersPGPorts) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	storage.claims[0]++
	if err := storage.observe(ctx); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	return storage.PostgresStore.ClaimDocJob(ctx, repo, now, lease)
}

func (storage *documentWorkersPGPorts) ClaimDocRangeJob(ctx context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
	storage.claims[1]++
	if err := storage.observe(ctx); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	return storage.PostgresStore.ClaimDocRangeJob(ctx, now, lease)
}

func (storage *documentWorkersPGPorts) ClaimDocFindFields(ctx context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
	storage.claims[2]++
	if err := storage.observe(ctx); err != nil {
		return domain.DocRangeIndexJob{}, err
	}
	return storage.PostgresStore.ClaimDocFindFields(ctx, now, lease)
}

func (storage *documentWorkersPGPorts) ClaimDocDisplay(ctx context.Context, now time.Time, lease time.Duration) (domain.DocDisplayJob, error) {
	storage.claims[3]++
	if err := storage.observe(ctx); err != nil {
		return domain.DocDisplayJob{}, err
	}
	return storage.PostgresStore.ClaimDocDisplay(ctx, now, lease)
}

func (storage *documentWorkersPGPorts) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if err := storage.observe(ctx); err != nil {
		return err
	}
	return storage.PostgresStore.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if storage.observing {
			storage.reads++
			if !storage.InReadOnlyTransaction(bound) {
				return domain.ErrConflict
			}
		}
		return read(bound)
	})
}

func TestPGDocumentWorkersWithoutServiceExecuteAllFourOwners(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	dsn := isolatedDisplayMaintenanceProcessDSN(test, ctx)
	actual, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(actual.Close)
	if _, err := actual.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	storage := &documentWorkersPGPorts{PostgresStore: actual}
	workers, err := app.NewDocumentWorkers(app.DocumentWorkersConfig{
		Metadata: storage, Blobs: storage, FindFields: storage, Display: storage,
		Verifier: &domain.CanonicalDocVerifier{}, Profile: documentquery.VerifiedRoots,
	})
	if err != nil {
		test.Fatal(err)
	}
	system := inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), app.ReadDocumentIdentities(storage, storage))
	system = outbound.WithDocumentIdentityCompatibility(system, inbound.DocumentIdentities(system), app.ReadDocumentIdentities(storage, storage))
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(system, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(system, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{
			{Seq: 0, Kind: domain.EventReasoning, RedactedSummary: strings.Repeat("field ", (128<<10)/6+1)},
			{Seq: 1, Kind: domain.EventToolResult, CallID: "standalone", Output: "published🙂 value"},
		},
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal(err)
	}
	root, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := storage.PutChunks(system, repo, bodies); err != nil {
		test.Fatal(err)
	}
	job, err := domain.NewDocFinalizationJobForRepresentation(repo, domain.DocumentRepresentation{
		Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: root,
	}, time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocJob(system, job); err != nil {
		test.Fatal(err)
	}
	if have, err := storage.HasDocs(system, repo, []domain.ContentHash{hash}); err != nil || len(have) != 0 {
		test.Fatal("acceptance published before actual owner execution", have, err)
	}
	storage.observing = true
	if err := workers.Finalization.Process(ctx, 1); err != nil {
		test.Fatal("standalone finalization", err)
	}
	storage.observing = false
	completed, err := storage.GetDocJob(system, repo, job.ID)
	if err != nil || completed.State != "completed" {
		test.Fatal("durable finalization receipt", completed, err)
	}
	if err := storage.PutSnapshot(system, domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash,
		DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	connectionConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("isolated fixture connection configuration", err)
	}
	connection, err := pgx.ConnectConfig(ctx, connectionConfig.ConnConfig)
	if err != nil {
		test.Fatal(err)
	}
	removed, removeErr := connection.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, hash)
	closeErr := connection.Close(ctx)
	if removeErr != nil || closeErr != nil || removed.RowsAffected() != 1 {
		test.Fatal("discard only the owned rebuildable range generation", removeErr, closeErr)
	}
	queued, err := storage.EnqueueDocRangeJob(system, repo, hash, time.Now().UTC())
	if err != nil || queued.State != "waiting" {
		test.Fatal("range work was not durably accepted", queued, err)
	}
	storage.observing = true
	if err := workers.Range.Process(ctx, 1); err != nil {
		test.Fatal("standalone range", err)
	}
	storage.observing = false
	completedRange, err := storage.EnqueueDocRangeJob(system, repo, hash, time.Now().UTC())
	if err != nil || completedRange.State != "completed" {
		test.Fatal("range owner did not publish its accepted generation", completedRange, err)
	}
	var fieldsEntry domain.DocRangeEntry
	var target domain.DocDisplayTarget
	if err := storage.WithinReadSnapshot(system, func(read context.Context) error {
		generation, err := storage.OpenDocRangeGeneration(read, repo, hash)
		if err != nil {
			return err
		}
		entries, err := generation.ReadPage(read, 0)
		if err != nil {
			return err
		}
		if len(entries) != 2 || generation.Descriptor().Identity() != domain.DocumentIdentityRootV1 {
			return domain.ErrIntegrity
		}
		fieldsEntry = entries[0]
		target = domain.DocDisplayTarget{RepoID: repo, DocHash: hash, Binding: generation.Binding(), Event: entries[1].DocEventIndex}
		return target.Validate()
	}); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocFindFields(system, repo, hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocDisplay(system, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	storage.observing = true
	if err := workers.FindFields.Process(ctx, 1); err != nil {
		test.Fatal("standalone find fields", err)
	}
	if processed, err := workers.Display.Process(ctx, 1); err != nil || processed != 1 {
		test.Fatal("standalone display", processed, err)
	}
	storage.observing = false
	for owner, claims := range storage.claims {
		if claims != 1 {
			test.Fatal("owner bypassed admitted durable claim", owner, claims)
		}
	}
	if storage.reads < 4 {
		test.Fatal("owners bypassed pinned repository source reads", storage.reads)
	}
	reader, err := documentquery.NewRawReader(documentquery.RawConfig{
		Profile: documentquery.VerifiedRoots, Snapshots: storage, Legacy: storage, Index: storage, Verified: storage,
	})
	if err != nil {
		test.Fatal(err)
	}
	expectedCanonical, err := domain.CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(system, func(read context.Context) error {
		fields, err := storage.ReadDocFindFields(read, repo, hash, fieldsEntry.Hash, fieldsEntry.Length)
		if err != nil {
			return err
		}
		if fields.Validate(fieldsEntry.Hash, fieldsEntry.Length) != nil || fields.Summary == nil {
			return fmt.Errorf("%w: persisted find fields", domain.ErrIntegrity)
		}
		generation, err := storage.ReadDocDisplayGeneration(read, target)
		if err != nil {
			return err
		}
		page, err := storage.ReadDocDisplayPage(read, target, 0)
		if err != nil {
			return err
		}
		if generation.Validate() != nil || page.Validate(generation.Source) != nil || page.Text != document.Events[1].Output {
			return fmt.Errorf("%w: persisted display text", domain.ErrIntegrity)
		}
		published, err := reader.MaterializedDocumentRead(read, repo, hash)
		if err != nil {
			return err
		}
		if published.DocumentRef() != job.DocumentRef() {
			return fmt.Errorf("%w: published document identity", domain.ErrIntegrity)
		}
		actualCanonical, err := domain.CanonicalBytes(published.CIR)
		if err != nil || !bytes.Equal(actualCanonical, expectedCanonical) {
			return fmt.Errorf("%w: exact published canonical document: %v", domain.ErrIntegrity, err)
		}
		foreign := domain.HashContent([]byte("foreign " + test.Name()))
		if _, err := reader.MaterializedDocumentRead(read, foreign, hash); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("published document escaped repository ownership", err)
		}
		foreignTarget := target
		foreignTarget.RepoID = foreign
		if _, err := storage.ReadDocDisplayGeneration(read, foreignTarget); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("published display escaped repository ownership", err)
		}
		return nil
	}); err != nil {
		test.Fatal("actual published fields, display, and original root read", err)
	}
}
