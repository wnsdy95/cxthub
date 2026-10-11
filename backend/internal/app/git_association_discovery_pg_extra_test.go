//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type associationCatalogExtraPG struct {
	*associationCatalogPG
	afterRead func(context.Context, domain.CatalogRequest, domain.CatalogPage) (domain.CatalogPage, error)
	saves     atomic.Int32
}

func (storage *associationCatalogExtraPG) CatalogChanges(ctx context.Context, repo domain.ContentHash, request domain.CatalogRequest) (domain.CatalogPage, error) {
	page, err := storage.associationCatalogPG.CatalogChanges(ctx, repo, request)
	if err != nil {
		return domain.CatalogPage{}, err
	}
	return storage.afterRead(ctx, request, page)
}

func (storage *associationCatalogExtraPG) SaveGitAssociationDiscovery(ctx context.Context, next domain.GitAssociationDiscovery, expected int64, publications []domain.HistoryEvent) error {
	storage.saves.Add(1)
	return storage.associationCatalogPG.SaveGitAssociationDiscovery(ctx, next, expected, publications)
}

type associationDiscoveryResultPG struct {
	progress bool
	err      error
}

func assertAssociationDiscoveryNoHandoffPG(t *testing.T, ctx context.Context, storage *associationCatalogPG, repo domain.Repo) {
	t.Helper()
	if _, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("failed catalog read persisted a checkpoint", err)
	}
	publications, err := storage.ListGitAssociationPublications(ctx, repo.ID, repo.GitRemoteURL, "", 101)
	if err != nil || len(publications) != 0 {
		t.Fatal("failed catalog read handed off publications", publications, err)
	}
}

func TestPGGitAssociationDiscoveryConcurrentCatalogReadsCAS(t *testing.T) {
	scans, worker, storage, repo, publications, reader := associationDiscoveryPGFixture(t, 2)
	ctx, cancel := context.WithTimeout(systemTestContext(), 15*time.Second)
	defer cancel()
	if progress, err := scans.discoverGitAssociations(ctx, repo); err != nil || !progress {
		t.Fatal("initial discovery failed", progress, err)
	}
	previous, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
	if err != nil || previous.Checkpoint == nil || previous.Cursor != "" {
		t.Fatal("initial discovery did not complete", previous, err)
	}
	latePublications := []domain.HistoryEvent{}
	for index, publication := range publications {
		publication.ID = fmt.Sprintf("%032x", index+40)
		if err := worker.core.RecordHistory(ctx, publication); err != nil {
			t.Fatal(err)
		}
		latePublications = append(latePublications, publication)
	}
	revision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	type catalogRead struct {
		request domain.CatalogRequest
		page    domain.CatalogPage
	}
	reads := make(chan catalogRead, 2)
	release := make(chan struct{})
	proxy := &associationCatalogExtraPG{associationCatalogPG: storage}
	proxy.afterRead = func(work context.Context, request domain.CatalogRequest, page domain.CatalogPage) (domain.CatalogPage, error) {
		reads <- catalogRead{request: request, page: page}
		select {
		case <-release:
			return page, nil
		case <-work.Done():
			return domain.CatalogPage{}, work.Err()
		}
	}
	worker.core.meta = proxy
	results := make(chan associationDiscoveryResultPG, 2)
	for range 2 {
		concurrent, err := NewGitScans(worker.core, reader)
		if err != nil {
			t.Fatal(err)
		}
		concurrent.SetSourceAuthorizer(worker)
		go func() {
			progress, err := concurrent.discoverGitAssociations(ctx, repo)
			results <- associationDiscoveryResultPG{progress: progress, err: err}
		}()
	}
	var observed [2]catalogRead
	for index := range observed {
		select {
		case observed[index] = <-reads:
		case <-ctx.Done():
			t.Fatal("workers did not both reach the catalog barrier", ctx.Err())
		}
	}
	for _, read := range observed {
		if !reflect.DeepEqual(read.request, previous.Request()) || read.page.Mode != "delta" || read.page.Checkpoint == nil {
			t.Fatal("workers did not read the same complete checkpoint", previous, read)
		}
		queued, err := gitAssociationPublications(read.page)
		if err != nil || !reflect.DeepEqual(queued, latePublications) {
			t.Fatal("catalog page lost accepted publications", queued, err)
		}
	}
	if !reflect.DeepEqual(observed[0].page, observed[1].page) || proxy.saves.Load() != 0 {
		t.Fatal("catalog reads were not synchronized before the handoff")
	}
	close(release)
	winners, conflicts := 0, 0
	for range 2 {
		select {
		case result := <-results:
			switch {
			case result.progress && result.err == nil:
				winners++
			case !result.progress && errors.Is(result.err, domain.ErrConflict):
				conflicts++
			default:
				t.Fatal("unexpected discovery result", result.progress, result.err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent handoffs did not finish", ctx.Err())
		}
	}
	if winners != 1 || conflicts != 1 || proxy.saves.Load() != 2 {
		t.Fatal("checkpoint CAS did not choose exactly one worker", winners, conflicts, proxy.saves.Load())
	}
	expected, err := previous.Advance(observed[0].page)
	if err != nil {
		t.Fatal(err)
	}
	current, err := storage.GetGitAssociationDiscovery(ctx, repo.ID, repo.GitRemoteURL)
	if err != nil || !reflect.DeepEqual(current, expected) {
		t.Fatal("losing worker changed the committed checkpoint", current, expected, err)
	}
	queued, err := storage.ListGitAssociationPublications(ctx, repo.ID, repo.GitRemoteURL, "", 101)
	if err != nil || !reflect.DeepEqual(queued, append(publications, latePublications...)) {
		t.Fatal("concurrent discovery lost or duplicated queued publications", queued, err)
	}
	afterRevision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil || afterRevision != revision || len(reader.headReads) != 0 {
		t.Fatal("discovery altered public revisions or read provider heads", revision, afterRevision, reader.headReads, err)
	}
}

func TestPGGitAssociationDiscoveryCanceledCatalogReadNoHandoff(t *testing.T) {
	scans, worker, storage, repo, _, _ := associationDiscoveryPGFixture(t, 2)
	ctx, cancel := context.WithTimeout(systemTestContext(), 15*time.Second)
	defer cancel()
	revision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	readReady := make(chan struct{}, 1)
	proxy := &associationCatalogExtraPG{associationCatalogPG: storage}
	proxy.afterRead = func(work context.Context, _ domain.CatalogRequest, _ domain.CatalogPage) (domain.CatalogPage, error) {
		readReady <- struct{}{}
		<-work.Done()
		return domain.CatalogPage{}, work.Err()
	}
	worker.core.meta = proxy
	readContext, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	results := make(chan associationDiscoveryResultPG, 1)
	go func() {
		progress, err := scans.discoverGitAssociations(readContext, repo)
		results <- associationDiscoveryResultPG{progress: progress, err: err}
	}()
	select {
	case <-readReady:
	case <-ctx.Done():
		t.Fatal("catalog read did not reach cancellation barrier", ctx.Err())
	}
	cancelRead()
	select {
	case result := <-results:
		if result.progress || !errors.Is(result.err, context.Canceled) {
			t.Fatal("canceled read reported progress", result.progress, result.err)
		}
	case <-ctx.Done():
		t.Fatal("canceled discovery did not finish", ctx.Err())
	}
	if proxy.saves.Load() != 0 {
		t.Fatal("canceled catalog read attempted a handoff", proxy.saves.Load())
	}
	assertAssociationDiscoveryNoHandoffPG(t, ctx, storage, repo)
	afterRevision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil || afterRevision != revision {
		t.Fatal("canceled discovery changed the public revision", revision, afterRevision, err)
	}
}

func TestPGGitAssociationDiscoveryMalformedCatalogStillReconcilesHeads(t *testing.T) {
	for _, scenario := range []string{"invalid-json", "publication-key", "checkpoint-scope"} {
		t.Run(scenario, func(t *testing.T) {
			scans, worker, storage, repo, publications, reader := associationDiscoveryPGFixture(t, 2)
			ctx, cancel := context.WithTimeout(systemTestContext(), 15*time.Second)
			defer cancel()
			proxy := &associationCatalogExtraPG{associationCatalogPG: storage}
			var mutations atomic.Int32
			proxy.afterRead = func(_ context.Context, _ domain.CatalogRequest, page domain.CatalogPage) (domain.CatalogPage, error) {
				if scenario == "checkpoint-scope" {
					if page.Checkpoint == nil {
						return domain.CatalogPage{}, errors.New("missing catalog checkpoint")
					}
					page.Checkpoint.RepoID = hh("foreign discovery checkpoint")
					mutations.Add(1)
					return page, nil
				}
				for index, entry := range page.Entries {
					if entry.Kind != "history" || entry.Key != publications[1].ID {
						continue
					}
					if scenario == "invalid-json" {
						page.Entries[index].Value = json.RawMessage(`{"id":`)
					} else {
						page.Entries[index].Key = strings.Repeat("f", 32)
					}
					mutations.Add(1)
					return page, nil
				}
				return domain.CatalogPage{}, errors.New("missing accepted publication in catalog")
			}
			worker.core.meta = proxy
			if progress, err := scans.discoverGitAssociations(ctx, repo); progress || !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("malformed catalog page was accepted", progress, err)
			}
			assertAssociationDiscoveryNoHandoffPG(t, ctx, storage, repo)
			if err := scans.Reconcile(ctx); err != nil {
				t.Fatal("malformed catalog blocked reconciliation", err)
			}
			assertAssociationDiscoveryNoHandoffPG(t, ctx, storage, repo)
			if mutations.Load() != 2 || proxy.saves.Load() != 0 {
				t.Fatal("malformed page did not fail before the handoff", mutations.Load(), proxy.saves.Load())
			}
			receipts, err := storage.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10)
			if err != nil || len(receipts) != 1 || !reflect.DeepEqual(reader.headReads, []int{1}) {
				t.Fatal("malformed catalog prevented qualified head ingestion", receipts, reader.headReads, err)
			}
		})
	}
}
