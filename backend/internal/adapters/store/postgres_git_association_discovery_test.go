//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func gitDiscoveryPGFixture(t *testing.T, storage *PostgresStore) (context.Context, domain.GitAssociationDiscovery, []domain.HistoryEvent) {
	t.Helper()
	ctx, repo, now := headReceiptFixture(t, storage)
	capture := initializationPGSnapshot(t, storage, repo.ID, "discovery capture")
	ordinary := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: string(repo.ID), BranchID: "feature-identity", Branch: "feature", Kind: "position",
		Source: capture.ID, Target: capture.ID, GitAfter: strings.Repeat("a", 40), WorktreeID: strings.Repeat("e", 32), CreatedAt: now}
	if err := storage.ApplyHistoryEvent(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	var publications []domain.HistoryEvent
	for number := 1; number <= 3; number++ {
		publication := ordinary
		publication.Kind, publication.ID = "publish", fmt.Sprintf("%032x", number)
		if err := storage.ApplyHistoryEvent(ctx, publication); err != nil {
			t.Fatal(err)
		}
		publications = append(publications, publication)
	}
	return ctx, domain.GitAssociationDiscovery{RepoID: repo.ID, GitOrigin: repo.GitRemoteURL}, publications
}

func gitDiscoveryPGPage(discovery domain.GitAssociationDiscovery) domain.GitAssociationDiscovery {
	discovery.Version++
	discovery.Cursor, discovery.Epoch, discovery.Through, discovery.Mode = "next-page", "12345678-1234-1234-1234-123456789abc", 20, "baseline"
	return discovery
}

func saveGitDiscoveryPG(ctx context.Context, storage *PostgresStore, next domain.GitAssociationDiscovery, expected int64, publications []domain.HistoryEvent) error {
	return storage.WithinRepository(ctx, next.RepoID, func(bound context.Context) error {
		return storage.SaveGitAssociationDiscovery(bound, next, expected, publications)
	})
}

func TestPGGitAssociationDiscoveryReplayResetAndRevision(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
	if _, err := storage.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing checkpoint invented", err)
	}
	before, err := storage.RepositoryRevision(ctx, discovery.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	next := gitDiscoveryPGPage(discovery)
	batch := slices.Clone(publications)
	slices.Reverse(batch)
	if err := saveGitDiscoveryPG(ctx, storage, next, 0, batch); err != nil {
		t.Fatal(err)
	}
	if got, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || !reflect.DeepEqual(got, next) {
		t.Fatal("checkpoint not durable", got, err)
	}
	if rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 101); err != nil || !reflect.DeepEqual(rows, publications) {
		t.Fatal("original publication handoff changed", rows, err)
	}
	for _, retry := range []domain.GitAssociationDiscovery{next, func() domain.GitAssociationDiscovery {
		changed := next
		changed.Cursor = "different-page"
		return changed
	}()} {
		if err := saveGitDiscoveryPG(ctx, storage, retry, 0, nil); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("lost response replay bypassed caller CAS", err)
		}
	}
	final := domain.GitAssociationDiscovery{RepoID: discovery.RepoID, GitOrigin: discovery.GitOrigin, Version: 2,
		Checkpoint: &domain.CatalogCheckpoint{Version: domain.CatalogVersion, RepoID: discovery.RepoID, Epoch: next.Epoch, Sequence: next.Through}}
	if err := saveGitDiscoveryPG(ctx, storage, final, 1, publications); err != nil {
		t.Fatal("immutable handoff replay failed", err)
	}
	reset, err := final.Reset()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveGitDiscoveryPG(ctx, storage, reset, final.Version, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || !reflect.DeepEqual(got, reset) {
		t.Fatal("checkpoint reset failed", got, err)
	}
	if rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 101); err != nil || !reflect.DeepEqual(rows, publications) {
		t.Fatal("reset lost or duplicated durable obligations", rows, err)
	}
	if after, err := peer.RepositoryRevision(ctx, discovery.RepoID); err != nil || after != before {
		t.Fatal("discovery touched public revisions", before, after, err)
	}
}

func TestPGGitAssociationDiscoveryConcurrentCAS(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
			if existing {
				discovery.Version = 1
				if err := saveGitDiscoveryPG(ctx, storage, discovery, 0, nil); err != nil {
					t.Fatal(err)
				}
			}
			bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			type result struct {
				next        domain.GitAssociationDiscovery
				publication domain.HistoryEvent
				err         error
			}
			results := make(chan result, 2)
			for index, writer := range []*PostgresStore{storage, peer} {
				go func() {
					next := gitDiscoveryPGPage(discovery)
					next.Cursor = fmt.Sprint("page-", index)
					publication := publications[index]
					err := saveGitDiscoveryPG(bounded, writer, next, discovery.Version, []domain.HistoryEvent{publication})
					results <- result{next, publication, err}
				}()
			}
			accepted := 0
			var winner result
			for range 2 {
				select {
				case got := <-results:
					if got.err == nil {
						accepted++
						winner = got
					} else if !errors.Is(got.err, domain.ErrConflict) {
						t.Fatal(got.err)
					}
				case <-bounded.Done():
					t.Fatal(bounded.Err())
				}
			}
			if accepted != 1 {
				t.Fatal("CAS accepted multiple writers", accepted)
			}
			if got, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || !reflect.DeepEqual(got, winner.next) {
				t.Fatal("losing CAS changed checkpoint", got, err)
			}
			if rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, []domain.HistoryEvent{winner.publication}) {
				t.Fatal("losing CAS leaked queue entries", rows, err)
			}
		})
	}
}

func TestPGGitAssociationDiscoveryRequiresAcceptedOriginalAtomically(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"missing", "changed", "wrong-kind", "foreign", "duplicate", "too-many"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
			next := gitDiscoveryPGPage(discovery)
			batch := slices.Clone(publications[:2])
			want := domain.ErrValidation
			switch scenario {
			case "missing":
				batch[1].ID = strings.Repeat("a", 32)
				want = domain.ErrNotFound
			case "changed":
				batch[1].GitAfter = strings.Repeat("b", 40)
				want = domain.ErrConflict
			case "wrong-kind":
				batch[1].Kind = "position"
			case "foreign":
				batch[1].RepoID = string(domain.HashContent([]byte("foreign")))
				want = domain.ErrIntegrity
			case "duplicate":
				batch[1] = batch[0]
			case "too-many":
				batch = make([]domain.HistoryEvent, domain.MaxCatalogLimit+1)
			}
			if err := storage.WithinRepository(ctx, discovery.RepoID, func(bound context.Context) error {
				if err := storage.SaveGitAssociationDiscovery(bound, next, 0, batch); !errors.Is(err, want) {
					t.Fatal("unaccepted publication handed off", err, want)
				}
				if _, err := storage.GetGitAssociationDiscovery(bound, discovery.RepoID, discovery.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("invalid second publication advanced checkpoint", err)
				}
				if rows, err := storage.ListGitAssociationPublications(bound, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || rows == nil || len(rows) != 0 {
					t.Fatal("invalid second publication leaked first handoff", rows, err)
				}
				return storage.SaveGitAssociationDiscovery(bound, next, 0, publications[:1])
			}); err != nil {
				t.Fatal("savepoint did not recover", err)
			}
			if rows, err := storage.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, publications[:1]) {
				t.Fatal("recovered transaction handoff mismatch", rows, err)
			}
		})
	}
}

func TestPGGitAssociationDiscoveryOuterRollbackAndCancellation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
			next := gitDiscoveryPGPage(discovery)
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			abort := errors.New("abort discovery and handoff")
			err := storage.WithinRepository(work, discovery.RepoID, func(bound context.Context) error {
				if err := storage.SaveGitAssociationDiscovery(bound, next, 0, publications); err != nil {
					return err
				}
				if got, err := storage.GetGitAssociationDiscovery(bound, discovery.RepoID, discovery.GitOrigin); err != nil || !reflect.DeepEqual(got, next) {
					t.Fatal("transaction cannot read checkpoint", got, err)
				}
				if _, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("uncommitted checkpoint visible", err)
				}
				if rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || len(rows) != 0 {
					t.Fatal("uncommitted handoff visible", rows, err)
				}
				if canceled {
					cancel()
					return work.Err()
				}
				return abort
			})
			want := abort
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatal("outer abort lost", err)
			}
			if _, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("checkpoint escaped outer rollback", err)
			}
			if rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || len(rows) != 0 {
				t.Fatal("queue escaped outer rollback", rows, err)
			}
		})
	}
}

func TestPGGitAssociationDiscoveryTransactionAndPolicyBoundary(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
	next := gitDiscoveryPGPage(discovery)
	reject := func(bound context.Context, writer *PostgresStore) {
		t.Helper()
		if err := writer.SaveGitAssociationDiscovery(bound, next, 0, publications); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("incompatible transaction saved discovery", err)
		}
	}
	reject(ctx, storage)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error { reject(bound, storage); return nil }); err != nil {
		t.Fatal(err)
	}
	foreign := domain.HashContent([]byte(t.Name() + " foreign"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: foreign}); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, foreign, func(bound context.Context) error { reject(bound, storage); return nil }); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := storage.WithinRepository(ctx, discovery.RepoID, func(bound context.Context) error {
		escaped = bound
		reject(bound, peer)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reject(escaped, storage)
	if err := storage.RequireDocumentIdentity(ctx, discovery.RepoID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := saveGitDiscoveryPG(ctx, storage, next, 0, publications); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("policy change bypassed", err)
	}
	if _, err := storage.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("rejected writes leaked checkpoint", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []func() error{
		func() error { return storage.SaveGitAssociationDiscovery(canceled, next, 0, publications) },
		func() error {
			_, err := storage.GetGitAssociationDiscovery(canceled, discovery.RepoID, discovery.GitOrigin)
			return err
		},
		func() error {
			_, err := storage.ListGitAssociationPublications(canceled, discovery.RepoID, discovery.GitOrigin, "", 10)
			return err
		},
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation ignored", err)
		}
	}
}

func TestPGGitAssociationDiscoveryOriginIsolationAndPagination(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
	next := gitDiscoveryPGPage(discovery)
	if err := saveGitDiscoveryPG(ctx, storage, next, 0, publications); err != nil {
		t.Fatal(err)
	}
	other := discovery
	other.GitOrigin += "-other"
	other.Version = 1
	if err := saveGitDiscoveryPG(ctx, storage, other, 0, publications[:1]); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("wrong current origin saved", err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, discovery.RepoID, other.GitOrigin); err != nil {
		t.Fatal(err)
	}
	if err := saveGitDiscoveryPG(ctx, storage, other, 0, publications[:1]); err != nil {
		t.Fatal(err)
	}
	stale := next
	stale.Version++
	if err := saveGitDiscoveryPG(ctx, storage, stale, next.Version, nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("old origin continued after repository moved", err)
	}
	if got, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || !reflect.DeepEqual(got, next) {
		t.Fatal("old-origin checkpoint lost", got, err)
	}
	if got, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, other.GitOrigin); err != nil || !reflect.DeepEqual(got, other) {
		t.Fatal("origin checkpoints mixed", got, err)
	}
	var listed []domain.HistoryEvent
	for after := ""; ; {
		rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, after, 1)
		if err != nil || rows == nil || len(rows) > 1 {
			t.Fatal(rows, err)
		}
		if len(rows) == 0 {
			break
		}
		if rows[0].ID <= after {
			t.Fatal("publication cursor did not advance")
		}
		listed = append(listed, rows[0])
		after = rows[0].ID
	}
	if !reflect.DeepEqual(listed, publications) {
		t.Fatal("pagination lost original publication", listed)
	}
	if rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, other.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, publications[:1]) {
		t.Fatal("origin queue leaked", rows, err)
	}
	foreign := domain.HashContent([]byte("foreign repo"))
	if rows, err := peer.ListGitAssociationPublications(ctx, foreign, discovery.GitOrigin, "", 10); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("repository queue leaked", rows, err)
	}
	if _, err := peer.GetGitAssociationDiscovery(ctx, foreign, discovery.GitOrigin); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("repository checkpoint leaked", err)
	}
}

func TestPGGitAssociationDiscoveryValidationAndImmutableQueue(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
	next := gitDiscoveryPGPage(discovery)
	for _, expected := range []int64{-1, 1, math.MaxInt64} {
		if err := saveGitDiscoveryPG(ctx, storage, next, expected, publications); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("invalid CAS generation accepted", expected, err)
		}
	}
	missing := next
	missing.Version = 2
	if err := saveGitDiscoveryPG(ctx, storage, missing, 1, nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("missing checkpoint accepted nonzero expected version", err)
	}
	invalid := next
	invalid.Mode = "other"
	if err := saveGitDiscoveryPG(ctx, storage, invalid, 0, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("invalid checkpoint accepted", err)
	}
	for _, query := range []struct {
		repo          domain.ContentHash
		origin, after string
		limit         int
	}{
		{"invalid", discovery.GitOrigin, "", 1},
		{discovery.RepoID, "", "", 1},
		{discovery.RepoID, " bad", "", 1},
		{discovery.RepoID, discovery.GitOrigin, "short", 1},
		{discovery.RepoID, discovery.GitOrigin, strings.Repeat("A", 32), 1},
		{discovery.RepoID, discovery.GitOrigin, strings.Repeat("g", 32), 1},
		{discovery.RepoID, discovery.GitOrigin, "", 0},
		{discovery.RepoID, discovery.GitOrigin, "", 102},
	} {
		if _, err := storage.ListGitAssociationPublications(ctx, query.repo, query.origin, query.after, query.limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid queue query accepted", query, err)
		}
	}
	if err := saveGitDiscoveryPG(ctx, storage, next, 0, publications[:1]); err != nil {
		t.Fatal(err)
	}
	changed := publications[0]
	changed.GitAfter = strings.Repeat("b", 40)
	raw, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE context_history SET event=$3 WHERE repo_id=$1 AND id=$2`, discovery.RepoID, changed.ID, raw); err != nil {
		t.Fatal(err)
	}
	later := next
	later.Version++
	if err := saveGitDiscoveryPG(ctx, storage, later, next.Version, []domain.HistoryEvent{changed}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("accepted-row mutation overwrote frozen handoff", err)
	}
	if got, err := storage.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || !reflect.DeepEqual(got, next) {
		t.Fatal("immutable queue collision advanced checkpoint", got, err)
	}
	if rows, err := storage.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, publications[:1]) {
		t.Fatal("immutable queue changed", rows, err)
	}
}

func TestPGGitAssociationDiscoveryRejectsStoredScopeCorruption(t *testing.T) {
	storage, _ := prHistoryPGStores(t)
	for _, scenario := range []string{"checkpoint-repo", "checkpoint-origin", "checkpoint-version", "queue-repo", "queue-id", "queue-kind"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
			next := gitDiscoveryPGPage(discovery)
			if err := saveGitDiscoveryPG(ctx, storage, next, 0, publications[:1]); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(scenario, "checkpoint-") {
				corrupt := next
				switch scenario {
				case "checkpoint-repo":
					corrupt.RepoID = domain.HashContent([]byte("foreign"))
				case "checkpoint-origin":
					corrupt.GitOrigin += "-other"
				case "checkpoint-version":
					corrupt.Version++
				}
				raw, err := json.Marshal(corrupt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := storage.pool.Exec(ctx, `UPDATE git_association_discovery SET payload=$3 WHERE repo_id=$1 AND origin=$2`, discovery.RepoID, discovery.GitOrigin, raw); err != nil {
					t.Fatal(err)
				}
				if _, err := storage.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("corrupt checkpoint read", err)
				}
				later := next
				later.Version++
				if err := saveGitDiscoveryPG(ctx, storage, later, next.Version, publications[1:]); !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("save overwrote corrupt checkpoint", err)
				}
				if rows, err := storage.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, publications[:1]) {
					t.Fatal("corrupt checkpoint leaked new obligations", rows, err)
				}
				return
			}
			corrupt := publications[0]
			switch scenario {
			case "queue-repo":
				corrupt.RepoID = string(domain.HashContent([]byte("foreign")))
			case "queue-id":
				corrupt.ID = publications[1].ID
			case "queue-kind":
				corrupt.Kind = "position"
			}
			raw, err := json.Marshal(corrupt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE git_association_publications SET payload=$4 WHERE repo_id=$1 AND origin=$2 AND publication_id=$3`, discovery.RepoID, discovery.GitOrigin, publications[0].ID, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("corrupt queue publication read", err)
			}
		})
	}
}

func TestPGGitAssociationDiscoveryExistingCheckpointRollback(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, discovery, publications := gitDiscoveryPGFixture(t, storage)
	first := gitDiscoveryPGPage(discovery)
	if err := saveGitDiscoveryPG(ctx, storage, first, 0, publications[:1]); err != nil {
		t.Fatal(err)
	}
	next := first
	next.Version++
	next.Cursor = "another-page"
	abort := errors.New("abort updated discovery")
	if err := storage.WithinRepository(ctx, discovery.RepoID, func(bound context.Context) error {
		if err := storage.SaveGitAssociationDiscovery(bound, next, first.Version, publications[1:]); err != nil {
			return err
		}
		if rows, err := storage.ListGitAssociationPublications(bound, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, publications) {
			t.Fatal("transaction did not see pending handoff", rows, err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal("outer rollback lost", err)
	}
	if got, err := peer.GetGitAssociationDiscovery(ctx, discovery.RepoID, discovery.GitOrigin); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatal("rollback lost earlier checkpoint", got, err)
	}
	if rows, err := peer.ListGitAssociationPublications(ctx, discovery.RepoID, discovery.GitOrigin, "", 10); err != nil || !reflect.DeepEqual(rows, publications[:1]) {
		t.Fatal("rollback lost earlier obligations or leaked new ones", rows, err)
	}
}
