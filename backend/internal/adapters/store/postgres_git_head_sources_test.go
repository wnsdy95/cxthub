//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func gitHeadSourceObservationPG(repo domain.Repo, branch, head string) domain.GitRefObservation {
	observation := headReceiptObservation(repo, head)
	observation.Ref = "refs/heads/" + branch
	return observation.WithID()
}

func assertGitHeadSourcesUnpublishedPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.Repo, claim domain.GitHeadScan) {
	t.Helper()
	for _, table := range []string{"git_ref_observations", "git_scan_jobs", "git_head_receipts"} {
		var count int
		if err := storage.db(ctx).QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", repo.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("partial source/page publication", table, count, err)
		}
	}
	if got, err := storage.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL); err != nil || !reflect.DeepEqual(got, claim) {
		t.Fatal("failed finish changed checkpoint", got, claim, err)
	}
}

func TestPGGitHeadSourcesAtomicPageAndDestinationSeparation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo, now := headReceiptFixture(t, storage)
	capture := initializationPGSnapshot(t, storage, repo.ID, "unchanged natural context ref")
	ref := domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: capture.ID}
	if err := storage.CompareAndSwapRef(ctx, repo.ID, ref, ""); err != nil {
		t.Fatal(err)
	}
	beforeRef, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	page := []domain.GitRefObservation{gitHeadSourceObservationPG(repo, "main", "e")}
	var sources []domain.GitRefObservation
	for index := 7; index >= 0; index-- {
		sources = append(sources, gitHeadSourceObservationPG(repo, fmt.Sprintf("source-%d", index), fmt.Sprintf("%x", index+1)))
	}
	expected, err := domain.NewGitHeadReceiptWithSources(claim, page, sources, true)
	if err != nil || expected == nil {
		t.Fatal(expected, err)
	}
	beforeRevision, err := storage.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo.ID, func(bound context.Context) error {
		if err := storage.FinishGitHeadScanWithSources(bound, claim, page, sources, true, now); err != nil {
			return err
		}
		got, err := storage.GetGitHeadReceipt(bound, repo.ID, repo.GitRemoteURL, claim.Version)
		if err != nil || !reflect.DeepEqual(got, *expected) || len(got.Heads) != 1 || len(got.PriorSourceHeads) != 8 {
			t.Fatal("receipt merged source pre-reads into paged heads", got, err)
		}
		for _, observations := range [][]domain.GitRefObservation{page, sources} {
			for _, observation := range observations {
				stored, err := storage.GetPRRefObservation(bound, repo.ID, repo.GitRemoteURL, observation.ID)
				if err != nil || !reflect.DeepEqual(stored, observation) {
					t.Fatal("transaction cannot read original observation", stored, err)
				}
				job := domain.NewGitScan(repo.ID, repo.GitRemoteURL, observation.After, now)
				if _, err := storage.GetGitScan(bound, repo.ID, job.ID); err != nil {
					t.Fatal("source or destination code work missing", err)
				}
			}
		}
		if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("receipt visible before outer commit", err)
		}
		head := gitContextHeadForReceiptPG(got, "main", "main-identity", strings.Repeat("1", 32))
		forged := head
		forged.Destination.DestinationBranchID, forged.Destination.DestinationBranch = "source-identity", "source-7"
		forged.Destination.DestinationHead, forged.Destination.ObservationID = sources[0].After, sources[0].ID
		forged.Destination = forged.Destination.WithID()
		forged = forged.WithID()
		if forged.Destination.MatchesReceipt(got) {
			t.Fatal("source pre-read became destination evidence")
		}
		if err := storage.PutGitContextHeads(bound, got, []domain.GitContextHead{forged}); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("source-only destination persisted", err)
		}
		return storage.PutGitContextHeads(bound, got, []domain.GitContextHead{head})
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); err != nil || !reflect.DeepEqual(got, *expected) {
		t.Fatal("committed read receipt changed", got, err)
	}
	if got, err := peer.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL); err != nil || !reflect.DeepEqual(got, claim.Finish(true, now)) {
		t.Fatal("source pre-reads changed paged checkpoint semantics", got, err)
	}
	if got, err := peer.GetRef(ctx, repo.ID, domain.RefBranch, "main"); err != nil || got != beforeRef {
		t.Fatal("provider observation moved natural context ref", got, beforeRef, err)
	}
	if after, err := peer.RepositoryRevision(ctx, repo.ID); err != nil || after != beforeRevision {
		t.Fatal("receipt alone advanced public revision", after, beforeRevision, err)
	}
	if err := storage.FinishGitHeadScanWithSources(ctx, claim, page, sources, true, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("receipt replay bypassed completed claim CAS", err)
	}
}

func TestPGGitHeadSourcesLegacyCompatibility(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, useSourceMethod := range []bool{false, true} {
		t.Run(fmt.Sprint(useSourceMethod), func(t *testing.T) {
			ctx, repo, now := headReceiptFixture(t, storage)
			claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			page := []domain.GitRefObservation{headReceiptObservation(repo, "a")}
			expected, err := domain.NewGitHeadReceipt(claim, page, true)
			if err != nil {
				t.Fatal(err)
			}
			legacy := struct {
				EncodingVersion int                           `json:"encoding_version"`
				ID              string                        `json:"id"`
				RepoID          domain.ContentHash            `json:"repo_id"`
				GitOrigin       string                        `json:"git_origin"`
				Version         int64                         `json:"version,string"`
				Page            int                           `json:"page"`
				Provider        domain.GitProviderObservation `json:"provider"`
				Heads           []domain.GitHeadReceiptHead   `json:"heads"`
				More            bool                          `json:"more"`
			}{1, "", expected.RepoID, expected.GitOrigin, expected.Version, expected.Page, expected.Provider, expected.Heads, expected.More}
			legacyRaw, err := json.Marshal(legacy)
			if err != nil || expected.ID != strings.TrimPrefix(string(domain.HashContent(legacyRaw)), "sha256:") {
				t.Fatal("old versioned identity changed", expected, err)
			}
			finish := func(claim domain.GitHeadScan, page []domain.GitRefObservation, more bool) error {
				if useSourceMethod {
					return storage.FinishGitHeadScanWithSources(ctx, claim, page, nil, more, now)
				}
				return storage.FinishGitHeadScan(ctx, claim, page, more, now)
			}
			if err := finish(claim, page, true); err != nil {
				t.Fatal(err)
			}
			got, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version)
			if err != nil || !reflect.DeepEqual(got, *expected) {
				t.Fatal("legacy receipt changed", got, err)
			}
			raw, err := json.Marshal(got)
			if err != nil || bytes.Contains(raw, []byte("prior_source_heads")) {
				t.Fatal("new field changed old JSON", string(raw), err)
			}
			claim, err = storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			unqualified := page[0]
			unqualified.Provider = nil
			unqualified = unqualified.WithID()
			if err := finish(claim, []domain.GitRefObservation{unqualified}, true); err != nil {
				t.Fatal(err)
			}
			if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("unqualified page produced receipt", err)
			}
			claim, err = storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(claim, nil, false); err != nil {
				t.Fatal(err)
			}
			if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("empty page invented receipt", err)
			}
		})
	}
	t.Run("empty-page-with-source", func(t *testing.T) {
		ctx, repo, now := headReceiptFixture(t, storage)
		claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		source := gitHeadSourceObservationPG(repo, "source", "a")
		if err := storage.FinishGitHeadScanWithSources(ctx, claim, nil, []domain.GitRefObservation{source}, false, now); err != nil {
			t.Fatal(err)
		}
		if _, err := peer.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, source.ID); err != nil {
			t.Fatal("empty destination page lost source observation", err)
		}
		if _, err := peer.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, claim.Version); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("source pre-read invented destination/absence receipt", err)
		}
	})
}

func TestPGGitHeadSourcesRollbackAndCancellation(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, repo, now := headReceiptFixture(t, storage)
			claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			page := []domain.GitRefObservation{headReceiptObservation(repo, "a")}
			sources := []domain.GitRefObservation{gitHeadSourceObservationPG(repo, "source", "b")}
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			abort := errors.New("abort source and page receipt")
			err = storage.WithinRepository(work, repo.ID, func(bound context.Context) error {
				if err := storage.FinishGitHeadScanWithSources(bound, claim, page, sources, true, now); err != nil {
					return err
				}
				if _, err := storage.GetGitHeadReceipt(bound, repo.ID, repo.GitRemoteURL, claim.Version); err != nil {
					return err
				}
				if _, err := peer.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, sources[0].ID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("source pre-read visible before commit", err)
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
				t.Fatal(err)
			}
			assertGitHeadSourcesUnpublishedPG(t, ctx, peer, repo, claim)
			if err := storage.FinishGitHeadScanWithSources(ctx, claim, page, sources, true, now); err != nil {
				t.Fatal("rollback prevented live claim retry", err)
			}
		})
	}
	t.Run("nested-checkpoint-failure", func(t *testing.T) {
		ctx, repo, now := headReceiptFixture(t, storage)
		claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		page := []domain.GitRefObservation{headReceiptObservation(repo, "a")}
		sources := []domain.GitRefObservation{gitHeadSourceObservationPG(repo, "source", "b")}
		if err := storage.WithinRepository(ctx, repo.ID, func(bound context.Context) error {
			if _, err := storage.db(bound).Exec(bound, `ALTER TABLE git_head_scans ADD CONSTRAINT fail_sources_checkpoint CHECK (payload->>'state'='running') NOT VALID`); err != nil {
				return err
			}
			if err := storage.FinishGitHeadScanWithSources(bound, claim, page, sources, true, now); err == nil {
				t.Fatal("injected checkpoint failure not returned")
			}
			assertGitHeadSourcesUnpublishedPG(t, bound, storage, repo, claim)
			if _, err := storage.db(bound).Exec(bound, `ALTER TABLE git_head_scans DROP CONSTRAINT fail_sources_checkpoint`); err != nil {
				return err
			}
			return storage.FinishGitHeadScanWithSources(bound, claim, page, sources, true, now)
		}); err != nil {
			t.Fatal("savepoint did not restore outer transaction", err)
		}
	})
}

func TestPGGitHeadSourcesValidationLeaseAndPolicy(t *testing.T) {
	storage, peer := prHistoryPGStores(t)
	ctx, repo, now := headReceiptFixture(t, storage)
	claim, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	page := []domain.GitRefObservation{headReceiptObservation(repo, "a")}
	source := gitHeadSourceObservationPG(repo, "source", "b")
	for _, mutation := range []func(*domain.GitRefObservation){
		func(observation *domain.GitRefObservation) {
			observation.Provider = &domain.GitProviderObservation{InstallationID: 790, RepositoryID: 91}
		},
		func(observation *domain.GitRefObservation) {
			observation.Provider = &domain.GitProviderObservation{InstallationID: 789, RepositoryID: 92}
		},
		func(observation *domain.GitRefObservation) { observation.Provider = nil },
		func(observation *domain.GitRefObservation) { observation.GitOrigin += "/other" },
		func(observation *domain.GitRefObservation) {
			observation.RepoID = domain.HashContent([]byte("foreign source"))
		},
	} {
		changed := source
		mutation(&changed)
		changed = changed.WithID()
		if err := storage.FinishGitHeadScanWithSources(ctx, claim, page, []domain.GitRefObservation{changed}, true, now); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("foreign or unqualified source accepted", err)
		}
		assertGitHeadSourcesUnpublishedPG(t, ctx, peer, repo, claim)
	}
	if err := storage.FinishGitHeadScanWithSources(ctx, claim, page, []domain.GitRefObservation{source, source}, true, now); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("duplicate source ref accepted", err)
	}
	tooMany := make([]domain.GitRefObservation, 9)
	for index := range tooMany {
		tooMany[index] = gitHeadSourceObservationPG(repo, fmt.Sprintf("source-%d", index), "b")
	}
	if err := storage.FinishGitHeadScanWithSources(ctx, claim, page, tooMany, true, now); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("source bound exceeded", err)
	}
	if err := storage.FinishGitHeadScanWithSources(ctx, claim, page, []domain.GitRefObservation{source}, true, claim.LeaseUntil); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired source read published", err)
	}
	assertGitHeadSourcesUnpublishedPG(t, ctx, peer, repo, claim)
	replacement, err := storage.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishGitHeadScanWithSources(ctx, claim, page, []domain.GitRefObservation{source}, true, now); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale worker published prior source", err)
	}
	assertGitHeadSourcesUnpublishedPG(t, ctx, peer, repo, replacement)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.FinishGitHeadScanWithSources(canceled, replacement, page, []domain.GitRefObservation{source}, true, now.Add(2*time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo.ID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err := storage.FinishGitHeadScanWithSources(ctx, replacement, page, []domain.GitRefObservation{source}, true, now.Add(2*time.Minute)); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("source finish bypassed identity policy", err)
	}
	assertGitHeadSourcesUnpublishedPG(t, ctx, peer, repo, replacement)
	supported := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
	if err := storage.FinishGitHeadScanWithSources(compatible, replacement, page, []domain.GitRefObservation{source}, true, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
}
