//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func seedDocViewBoundaryPG(t *testing.T, fixture docViewPGFixture, document domain.CIRDocument) domain.ContentHash {
	t.Helper()
	manifest, chunks, err := domain.ConversationManifestForCIR(document)
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
	if _, _, err := fixture.storage.PutChunks(fixture.ctx, fixture.repo, chunks); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, hash, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2) ON CONFLICT DO NOTHING`, fixture.repo, hash); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{ID: hash, RepoID: fixture.repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestPGDocViewBoundaryResumesWithoutForegroundPrefixReplay(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	ctx, repo := fixture.ctx, fixture.repo
	shared := domain.DocRangePrefixBatchPages*domain.DocRangePageEvents + 37
	document := domain.CIRDocument{Envelope: fixture.cir.Envelope}
	for ordinal := 0; ordinal < shared+40; ordinal++ {
		event := historyMessage(domain.RoleUser, "synthetic resumable prefix "+string(repo))
		event.Seq = ordinal
		document.Events = append(document.Events, event)
	}
	hash := seedDocViewBoundaryPG(t, fixture, document)
	previous := document
	previous.Events = append([]domain.CIREvent(nil), document.Events...)
	previous.Events[shared] = historyMessage(domain.RoleAssistant, "different exact event, not semantic deduplication")
	previous.Events[shared].Seq = shared
	base := seedDocViewBoundaryPG(t, fixture, previous)
	before, err := fixture.storage.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	observed := &docViewPGStore{PostgresStore: fixture.storage}
	fixture.service.blobs = observed
	read := func(ctx context.Context) (domain.DocViewPage, error) {
		return fixture.service.ReadDocViewEvents(ctx, repo, hash, base, -1, 20, "chat")
	}
	if _, err := read(outbound.WithDocReadOnly(ctx)); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		t.Fatal("read-only lookup created boundary", err)
	}
	if _, err := fixture.storage.ClaimDocRangeJob(ctx, time.Now().UTC(), docJobLease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("read-only lookup queued a job", err)
	}
	if err := fixture.storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := read(bound); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("borrowed read escaped its transaction to enqueue", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.storage.ClaimDocRangeJob(ctx, time.Now().UTC(), docJobLease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("borrowed read queued a job", err)
	}
	if _, err := read(ctx); !errors.Is(err, domain.ErrDocRangeIndexPending) || observed.pages != 0 {
		t.Fatal("foreground traversed prefix", err, observed.opens, observed.pages)
	}
	if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if partial, err := read(ctx); !errors.Is(err, domain.ErrDocRangeIndexPending) || partial.Hash != "" || partial.Inherited != 0 || len(partial.Events) != 0 {
		t.Fatal("one batch exceeded its budget or exposed partial inheritance", partial, err)
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
	for attempt := 0; attempt < 3; attempt++ {
		page, err := read(ctx)
		if err != nil || page.Offset != shared || page.Inherited != shared || page.Next != shared+20 || len(page.Events) != 20 {
			t.Fatal("completed boundary not reused", page.Offset, page.Inherited, page.Next, len(page.Events), err)
		}
		for offset, event := range page.Events {
			if event.Index != shared+offset || !reflect.DeepEqual(event.Event, document.Events[shared+offset]) {
				t.Fatal("prefix preparation changed event order", event.Index)
			}
		}
	}
	if observed.pages != 3 || observed.fallbackPages != 0 {
		t.Fatal("repeated view replayed prefix", observed.pages, observed.fallbackPages)
	}
	after, err := peer.RepositoryRevision(ctx, repo)
	if err != nil || before != after {
		t.Fatal("boundary work altered repository facts", before, after, err)
	}
	if _, err := peer.ClaimDocRangeJob(ctx, time.Now().UTC(), docJobLease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("completed boundary left queue work", err)
	}
	if _, err := fixture.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, base); err != nil {
		t.Fatal(err)
	}
	if _, err := read(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cached boundary bypassed revoked base ownership", err)
	}
}

func TestPGDocViewBoundarySourceVersionInvalidationAndSameDocument(t *testing.T) {
	fixture := newDocViewPGFixture(t)
	ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
	if err := fixture.storage.EnsureDocRangeIndex(ctx, repo, hash); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, hash, -1, 20, "chat")
	if err != nil || page.Inherited != len(fixture.cir.Events) || page.Offset != page.Total || len(page.Events) != 0 || page.Next != -1 {
		t.Fatal("identical source should need no comparison job", page, err)
	}
	previous := fixture.cir
	previous.Events = previous.Events[:123]
	base := seedDocViewBoundaryPG(t, fixture, previous)
	read := func() (domain.DocViewPage, error) {
		return fixture.service.ReadDocViewEvents(ctx, repo, hash, base, -1, 20, "chat")
	}
	if _, err := read(); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal(err)
	}
	if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	page, err = read()
	if err != nil || page.Inherited != 123 {
		t.Fatal("short prefix did not complete", page.Inherited, err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, base); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"root", "boundary"} {
		if _, err := read(); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			t.Fatal("stale proof reused", stage, err)
		}
		if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
			t.Fatal("failed to rebuild", stage, err)
		}
	}
	page, err = read()
	if err != nil || page.Inherited != 123 || len(page.Events) != 20 || !reflect.DeepEqual(page.Events[0].Event, fixture.cir.Events[123]) {
		t.Fatal("rebuilt prefix changed conversation", page.Inherited, err)
	}
}

type boundaryReceiptRaceStore struct {
	*store.PostgresStore
	fixture              docViewPGFixture
	test                 *testing.T
	mode                 string
	base                 domain.ContentHash
	calls                int
	invalidated, rebuilt bool
}

func (storage *boundaryReceiptRaceStore) EnqueueDocRangeBoundary(ctx context.Context, repo, hash, base domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.calls++
	storage.base = base
	job, err := storage.PostgresStore.EnqueueDocRangeBoundary(ctx, repo, hash, base, now)
	if err != nil || storage.calls != 1 {
		return job, err
	}
	claim, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), docJobLease)
	if err != nil || claim.DocHash != hash || claim.BaseHash != base {
		storage.test.Fatal("race fixture claimed wrong work", claim, err)
	}
	if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
		return job, err
	}
	job, err = storage.PostgresStore.EnqueueDocRangeBoundary(ctx, repo, hash, base, now)
	if err != nil || job.State != "completed" {
		storage.test.Fatal("race fixture did not complete", job, err)
	}
	if storage.mode == "late-generation" {
		return job, err
	}
	if storage.mode == "generation" {
		_, err = storage.fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, base)
	} else {
		_, err = storage.fixture.pool.Exec(ctx, `DELETE FROM doc_range_boundaries_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, repo, hash, base)
	}
	return job, err
}

func (storage *boundaryReceiptRaceStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocRangeIndex(ctx, repo, hash)
	if err == nil && ready && storage.mode == "late-generation" && storage.calls == 1 && hash == storage.base && !storage.invalidated {
		_, err = storage.fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash)
		storage.invalidated = true
	}
	return ready, err
}

func (storage *boundaryReceiptRaceStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) && storage.mode == "late-generation" && hash == storage.base && !storage.rebuilt {
		if _, repairErr := storage.PostgresStore.EnqueueDocRangeJob(storage.fixture.ctx, repo, hash, time.Now().UTC()); repairErr != nil {
			storage.test.Fatal("race fixture enqueue failed", repairErr)
		}
		claim, repairErr := storage.PostgresStore.ClaimDocRangeJob(storage.fixture.ctx, time.Now().UTC(), docJobLease)
		if repairErr != nil || claim.DocHash != hash || claim.BaseHash != "" {
			storage.test.Fatal("race fixture claimed wrong generation", claim, repairErr)
		}
		if repairErr := storage.PostgresStore.BuildDocRangeJob(storage.fixture.ctx, claim); repairErr != nil {
			storage.test.Fatal("race fixture rebuild failed", repairErr)
		}
		storage.rebuilt = true
	}
	return read, err
}

func TestPGDocViewBoundaryReceiptInvalidationRemainsRecoverable(t *testing.T) {
	for _, name := range []string{"boundary", "generation", "late-generation"} {
		t.Run(name, func(t *testing.T) {
			fixture := newDocViewPGFixture(t)
			ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
			if err := fixture.storage.EnsureDocRangeIndex(ctx, repo, hash); err != nil {
				t.Fatal(err)
			}
			previous := fixture.cir
			previous.Events = previous.Events[:123]
			base := seedDocViewBoundaryPG(t, fixture, previous)
			storage := &boundaryReceiptRaceStore{PostgresStore: fixture.storage, fixture: fixture, test: t, mode: name}
			fixture.service.blobs = storage
			read := func() (domain.DocViewPage, error) {
				return fixture.service.ReadDocViewEvents(ctx, repo, hash, base, -1, 20, "chat")
			}
			if page, err := read(); !errors.Is(err, domain.ErrDocRangeIndexPending) || page.Hash != "" {
				t.Fatal("invalidation became terminal or exposed stale data", page, err)
			}
			var active int
			if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND state IN ('waiting','running','retrying')`, repo).Scan(&active); err != nil || active == 0 {
				t.Fatal("retry hint had no durable recovery work", active, err)
			}
			if name == "late-generation" && (!storage.invalidated || !storage.rebuilt || storage.calls != 2) {
				t.Fatal("late invalidation did not requeue boundary", storage.invalidated, storage.rebuilt, storage.calls)
			}
			for attempt := 0; attempt < 3; attempt++ {
				if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
					t.Fatal(err)
				}
				page, err := read()
				if errors.Is(err, domain.ErrDocRangeIndexPending) {
					continue
				}
				if err != nil || page.Inherited != 123 || page.Offset != 123 || len(page.Events) != 20 {
					t.Fatal("invalidation did not recover", page, err)
				}
				return
			}
			t.Fatal("bounded recovery did not finish")
		})
	}
}

type generationReadinessRaceStore struct {
	*store.PostgresStore
	fixture docViewPGFixture
	calls   int
}

func (storage *generationReadinessRaceStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocRangeIndex(ctx, repo, hash)
	storage.calls++
	if err == nil && ready && storage.calls == 1 {
		_, err = storage.fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash)
	}
	return ready, err
}

func TestPGDocViewReadinessInvalidationRepairsPageAndFragment(t *testing.T) {
	for _, kind := range []string{"page", "fragment"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newDocViewPGFixture(t)
			ctx, repo, hash := fixture.ctx, fixture.repo, fixture.hash
			if err := fixture.storage.EnsureDocRangeIndex(ctx, repo, hash); err != nil {
				t.Fatal(err)
			}
			storage := &generationReadinessRaceStore{PostgresStore: fixture.storage, fixture: fixture}
			fixture.service.blobs = storage
			read := func() (domain.CIREvent, error) {
				if kind == "page" {
					page, err := fixture.service.ReadDocViewEvents(ctx, repo, hash, "", -1, 20, "chat")
					if err != nil {
						if page.Hash != "" || len(page.Events) != 0 {
							t.Fatal("invalidated page exposed contents", page)
						}
						return domain.CIREvent{}, err
					}
					if len(page.Events) != 20 {
						t.Fatal("incomplete recovered page", len(page.Events))
					}
					return page.Events[0].Event, nil
				}
				fragment, err := fixture.service.ReadDocViewFragment(ctx, repo, hash, 0, 0, 64<<10)
				if err != nil {
					if fragment.JSON != "" {
						t.Fatal("invalidated fragment exposed contents", fragment)
					}
					return domain.CIREvent{}, err
				}
				var event domain.CIREvent
				if fragment.Next != -1 || json.Unmarshal([]byte(fragment.JSON), &event) != nil {
					t.Fatal("incomplete recovered fragment", fragment)
				}
				return event, nil
			}
			if _, err := read(); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				t.Fatal("generation race became terminal", err)
			}
			var waiting int
			if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='waiting'`, repo, hash).Scan(&waiting); err != nil || waiting != 1 {
				t.Fatal("repair was not durable", waiting, err)
			}
			if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if event, err := read(); err != nil || !reflect.DeepEqual(event, fixture.cir.Events[0]) {
				t.Fatal("recovered content differs", event, err)
			}
		})
	}
}
