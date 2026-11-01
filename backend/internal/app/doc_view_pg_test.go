//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docViewPGStore struct {
	*store.PostgresStore
	opens, pages, fallbackPages int
}

type docViewPGRead struct {
	outbound.DocRangeGenerationRead
	store *docViewPGStore
}

func (storage *docViewPGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.opens++
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	return &docViewPGRead{DocRangeGenerationRead: read, store: storage}, err
}

func (read *docViewPGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	read.store.pages++
	return read.DocRangeGenerationRead.ReadPage(ctx, position)
}

func (storage *docViewPGStore) DocRangePage(ctx context.Context, repo domain.ContentHash, header domain.DocRangeIndex, position int) ([]domain.DocRangeEntry, error) {
	storage.fallbackPages++
	return storage.PostgresStore.DocRangePage(ctx, repo, header, position)
}

type docViewPGFixture struct {
	service  *Service
	storage  *store.PostgresStore
	pool     *pgxpool.Pool
	ctx      context.Context
	repo     domain.ContentHash
	hash     domain.ContentHash
	cir      domain.CIRDocument
	manifest domain.ConversationManifest
}

func newDocViewPGFixture(t *testing.T) docViewPGFixture {
	t.Helper()
	service, storage, repo := collaborationPG(t)
	storage.ConfigureLegacyByteRangePublication(true)
	ctx := inbound.WithDocumentIdentities(systemTestContext(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}}
	for position := 0; position < 200; position++ {
		event := historyMessage(domain.RoleUser, "synthetic pinned viewer page "+string(repo))
		event.Seq = position
		cir.Events = append(cir.Events, event)
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.PutChunks(ctx, repo, bodies); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, `DELETE FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo); err != nil {
			t.Error(err)
		}
	})
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, hash, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, repo, hash); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	return docViewPGFixture{service, storage, pool, ctx, repo, hash, cir, manifest}
}

func TestPGDocViewPinnedReaderUsesActualSnapshotAndChecksSelectedBytes(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	service, storage, ctx, repo, hash := fixture.service, fixture.storage, fixture.ctx, fixture.repo, fixture.hash
	if err := storage.EnsureDocRangeIndex(ctx, repo, hash); err != nil {
		t.Fatal(err)
	}
	observed := &docViewPGStore{PostgresStore: storage}
	service.blobs = observed
	page, err := service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat")
	if err != nil || len(page.Events) != 20 || page.Next != 140 {
		t.Fatal("page failed", err)
	}
	for offset, event := range page.Events {
		if event.Index != offset+120 || !reflect.DeepEqual(event.Event, fixture.cir.Events[offset+120]) {
			t.Fatal("page content changed", offset)
		}
	}
	if observed.opens != 1 || observed.pages != 2 || observed.fallbackPages != 0 {
		t.Fatal("application did not use pinned range reads", observed.opens, observed.pages, observed.fallbackPages)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("corrupt selected bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat"); err == nil {
		t.Fatal("pinned range reader bypassed current chunk verification")
	}
}

func TestPGDocViewPendingIndexSurvivesServiceRestart(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
	before, err := fixture.storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			t.Fatal("missing index was synchronously built", err)
		}
	}
	if _, err := fixture.storage.DocRangeIndex(ctx, repo, hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		t.Fatal("viewer published derived index", err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	restarted := NewService(peer, peer, nil, nil, peer)
	if err := restarted.ProcessDocRangeJobs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat")
	if err != nil || len(page.Events) != 20 || page.Next != 140 {
		t.Fatal("queued page did not recover", err)
	}
	for offset, event := range page.Events {
		if event.Index != offset+120 || !reflect.DeepEqual(event.Event, fixture.cir.Events[offset+120]) {
			t.Fatal("backfill changed source order", offset)
		}
	}
	after, err := peer.RepositoryRevision(ctx, repo)
	if err != nil || before != after {
		t.Fatal("derived work published graph activity", before, after, err)
	}
}

func TestPGDocViewQueuedSourceFailureIsTerminal(t *testing.T) {
	for _, failure := range []string{"corrupt chunk", "revoked source"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newDocViewPGFixture(t)
			ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
			if _, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 0, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				t.Fatal("missing durable request", err)
			}
			want, reason := domain.ErrIntegrity, "invalid_document"
			if failure == "corrupt chunk" {
				if _, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("damaged source")); err != nil {
					t.Fatal(err)
				}
			} else {
				want, reason = domain.ErrNotFound, "source_unavailable"
				if _, err := fixture.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, hash); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err == nil {
				t.Fatal("invalid source was published")
			}
			if _, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 0, 20, "chat"); !errors.Is(err, want) {
				t.Fatal("source failure remained pending", err)
			}
			if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
				t.Fatal("terminal source was claimed again", err)
			}
			var state, recorded string
			var attempts, indexes int
			if err := fixture.pool.QueryRow(ctx, `SELECT state,reason,attempts,(SELECT count(*) FROM doc_range_indexes_v1 WHERE hash=$2)
 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, repo, hash).Scan(&state, &recorded, &attempts, &indexes); err != nil || state != "rejected" || recorded != reason || attempts != 1 || indexes != 0 {
				t.Fatal("invalid terminal receipt", state, recorded, attempts, indexes, err)
			}
			if failure == "corrupt chunk" {
				var raw []byte
				if err := fixture.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, fixture.manifest.Chunks[0].Hash).Scan(&raw); err != nil || string(raw) != "damaged source" {
					t.Fatal("worker rewrote source", err)
				}
			}
		})
	}
}

func TestPGDocViewMissingGenerationRequeuesCompletedReceipt(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
	previous := int64(-1)
	for cycle := 0; cycle < 3; cycle++ {
		if cycle == 1 {
			if _, err := fixture.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, hash); err != nil {
				t.Fatal(err)
			}
		} else if cycle == 2 {
			if _, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil {
				t.Fatal(err)
			}
		}
		if cycle > 0 {
			if _, err := fixture.storage.DocRangeIndex(ctx, repo, hash); err != nil {
				t.Fatal("v1 fixture must still be valid", err)
			}
		}
		if _, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			t.Fatal("v1 index or old completion bypassed generation rebuild", cycle, err)
		}
		if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
			t.Fatal(err)
		}
		for _, view := range []string{"chat", "prompts", "all", "raw"} {
			page, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, view)
			if err != nil || page.Next != 140 || len(page.Events) != 20 {
				t.Fatal("generation did not recover", cycle, view, page.Next, len(page.Events), err)
			}
			for offset, event := range page.Events {
				if event.Deferred || event.Index != offset+120 || !reflect.DeepEqual(event.Event, fixture.cir.Events[offset+120]) {
					t.Fatal("rebuilt generation changed source", cycle, view, offset)
				}
			}
		}
		var state string
		var attempts int
		var version int64
		if err := fixture.pool.QueryRow(ctx, `SELECT state,attempts,version FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, repo, hash).Scan(&state, &attempts, &version); err != nil || state != "completed" || attempts != 1 || version <= previous {
			t.Fatal("completion was not fenced and rebuilt", state, attempts, version, err)
		}
		previous = version
	}
}

func TestPGDocViewCorruptGenerationIsNotRebuiltOrReadThroughV1(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
	if err := fixture.storage.EnsureDocRangeIndex(ctx, repo, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2 WHERE doc_hash=$1`, hash, []byte(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.DocRangeIndex(ctx, repo, hash); err != nil {
		t.Fatal("v1 fixture must remain valid", err)
	}
	if _, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 0, 20, "chat"); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("malformed present generation did not fail closed", err)
	}
	var queued int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2`, repo, hash).Scan(&queued); err != nil || queued != 0 {
		t.Fatal("corrupt present generation was silently queued for repair", queued, err)
	}
}

func TestPGDocViewOlderWriterInvalidatesLaterPublication(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
	mutator, err := fixture.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer mutator.Rollback(ctx)
	var original []byte
	if err := mutator.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, hash).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal("initial request must queue", err)
	}
	if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := mutator.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil {
		t.Fatal(err)
	}
	if err := mutator.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal("later publication survived older snapshot mutation", err)
	}
	if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", 120, 20, "chat")
	if err != nil || len(page.Events) != 20 || page.Next != 140 {
		t.Fatal("source fence could not recover", err)
	}
	for offset, event := range page.Events {
		if !reflect.DeepEqual(event.Event, fixture.cir.Events[offset+120]) {
			t.Fatal("recovery changed event", offset)
		}
	}
	var current []byte
	if err := fixture.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, hash).Scan(&current); err != nil || !reflect.DeepEqual(current, original) {
		t.Fatal("recovery changed original bytes", err)
	}
}
