//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitAdmissionFencePG struct {
	*admissionBatchPG
	beforeFinish      func(context.Context, domain.GitHeadScan) error
	fenceContext      context.Context
	fenceGeneration   domain.RepositoryMetadataGeneration
	writeReads        int
	finishCalls       int
	sourceFinishCalls int
	finished          bool
	failure           error
}

func (storage *gitAdmissionFencePG) ReadRepositoryMetadataGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryMetadataGeneration, error) {
	if !storage.InReadOnlyTransaction(ctx) && storage.finished {
		storage.failure = errors.New("generation revalidated after head-scan writes")
		return domain.RepositoryMetadataGeneration{}, storage.failure
	}
	generation, err := storage.PostgresStore.ReadRepositoryMetadataGeneration(ctx, repo)
	if err == nil && !storage.InReadOnlyTransaction(ctx) {
		storage.writeReads++
		storage.fenceContext, storage.fenceGeneration = ctx, generation
	}
	return generation, err
}

func (storage *gitAdmissionFencePG) checkFence(ctx context.Context, claim domain.GitHeadScan) error {
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) || storage.fenceContext != ctx || storage.writeReads != 1 || storage.fenceGeneration.RepoID != claim.RepoID {
		storage.failure = fmt.Errorf("head scan reached finish without one full-generation fence in the same write: reads=%d generation=%+v", storage.writeReads, storage.fenceGeneration)
		return storage.failure
	}
	storage.failure = storage.beforeFinish(ctx, claim)
	return storage.failure
}

func (storage *gitAdmissionFencePG) FinishGitHeadScan(ctx context.Context, claim domain.GitHeadScan, observations []domain.GitRefObservation, more bool, now time.Time) error {
	storage.finishCalls++
	if err := storage.checkFence(ctx, claim); err != nil {
		return err
	}
	err := storage.PostgresStore.FinishGitHeadScan(ctx, claim, observations, more, now)
	storage.finished = err == nil
	return err
}

func (storage *gitAdmissionFencePG) FinishGitHeadScanWithSources(ctx context.Context, claim domain.GitHeadScan, observations, sources []domain.GitRefObservation, more bool, now time.Time) error {
	storage.sourceFinishCalls++
	if err := storage.checkFence(ctx, claim); err != nil {
		return err
	}
	err := storage.PostgresStore.FinishGitHeadScanWithSources(ctx, claim, observations, sources, more, now)
	storage.finished = err == nil
	return err
}

func gitAdmissionFenceFixturePG(t *testing.T, withSources bool) (*GitScans, *gitAdmissionFencePG, *pgx.Conn, domain.Repo, []domain.HistoryEvent) {
	t.Helper()
	scans, fixture, original, paged, publications := admissionBatchReconcileFixture(t, 2)
	ctx, cancel := context.WithTimeout(systemTestContext(), 15*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(fixture.dsn)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := pgx.ConnectConfig(ctx, config.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := peer.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	repo := fixture.repo
	for _, commit := range original.trees {
		payload, err := json.Marshal(commit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := peer.Exec(ctx, `INSERT INTO git_commit_trees(repo_id,origin,commit_oid,payload) VALUES($1,$2,$3,$4::jsonb)`, repo.ID, repo.GitRemoteURL, commit.Commit, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	original.trees = nil
	storage := &gitAdmissionFencePG{admissionBatchPG: original}
	fixture.worker.core.meta = storage
	if withSources {
		paged.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/main", Commit: strings.Repeat("b", 40)}}
		reader := &admissionSourceReader{admissionBatchReader: paged, sources: map[string]outbound.GitHead{
			"feature": {Ref: "refs/heads/feature", Commit: fixture.frozen.input.SourceHead},
		}}
		scans, err = NewGitScans(fixture.worker.core, reader)
		if err != nil {
			t.Fatal(err)
		}
		scans.SetSourceAuthorizer(fixture.worker)
	}
	return scans, storage, peer, repo, publications
}

func gitAdmissionFenceTryAncestryWritePG(ctx context.Context, peer *pgx.Conn, repo domain.Repo) error {
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	transaction, err := peer.Begin(bounded)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = transaction.Rollback(cleanup)
	}()
	if _, err := transaction.Exec(bounded, `SET LOCAL lock_timeout='100ms'`); err != nil {
		return err
	}
	changed, err := transaction.Exec(bounded, `UPDATE git_commit_trees SET payload=jsonb_set(payload,'{parents}','[]'::jsonb) WHERE repo_id=$1 AND origin=$2 AND commit_oid=$3`, repo.ID, repo.GitRemoteURL, strings.Repeat("b", 40))
	if err != nil {
		return err
	}
	if changed.RowsAffected() != 1 {
		return fmt.Errorf("ancestry mutation touched %d rows instead of the isolated fixture", changed.RowsAffected())
	}
	return nil
}

func TestPGGitAdmissionFenceBeforeHeadScanWrites(t *testing.T) {
	for _, withSources := range []bool{false, true} {
		name := "finish"
		if withSources {
			name = "finish-with-sources"
		}
		t.Run(name, func(t *testing.T) {
			scans, storage, peer, repo, publications := gitAdmissionFenceFixturePG(t, withSources)
			ctx, cancel := context.WithTimeout(systemTestContext(), 15*time.Second)
			defer cancel()
			if err := gitAdmissionFenceTryAncestryWritePG(ctx, peer, repo); err != nil {
				t.Fatal("unfenced fixture mutation must succeed", err)
			}
			blocked := 0
			storage.beforeFinish = func(write context.Context, claim domain.GitHeadScan) error {
				if claim.RepoID != repo.ID || claim.GitOrigin != repo.GitRemoteURL {
					return errors.New("finish escaped the isolated repository")
				}
				current := domain.RepositoryMetadataGeneration{RepoID: repo.ID}
				if err := peer.QueryRow(write, `SELECT epoch::text,head_seq FROM repository_catalog_state WHERE repo_id=$1`, repo.ID).Scan(&current.Epoch, &current.Sequence); err != nil {
					return err
				}
				if current != storage.fenceGeneration {
					return fmt.Errorf("fence did not capture full committed generation: got=%+v want=%+v", storage.fenceGeneration, current)
				}
				err := gitAdmissionFenceTryAncestryWritePG(write, peer, repo)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "55P03" {
					return fmt.Errorf("raw ancestry mutation crossed the pre-finish fence: %v", err)
				}
				blocked++
				return nil
			}
			if err := scans.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if storage.failure != nil {
				t.Fatal(storage.failure)
			}
			wantFinish, wantSources := 1, 0
			if withSources {
				wantFinish, wantSources = 0, 1
			}
			if storage.finishCalls != wantFinish || storage.sourceFinishCalls != wantSources || storage.writeReads != 1 || blocked != 1 || !storage.finished {
				t.Fatalf("ordered composition not exercised: finish=%d sources=%d fence=%d blocked=%d completed=%v", storage.finishCalls, storage.sourceFinishCalls, storage.writeReads, blocked, storage.finished)
			}
			admissions, err := storage.ListGitContributionAdmissions(ctx, repo.ID, repo.GitRemoteURL, "", 10)
			if err != nil || len(admissions) != len(publications) {
				t.Fatalf("own finish writes invalidated the validated batch: admissions=%d want=%d err=%v", len(admissions), len(publications), err)
			}
			seen := map[string]bool{}
			for _, admission := range admissions {
				receipt, err := storage.GetGitHeadReceipt(ctx, repo.ID, repo.GitRemoteURL, admission.Destination.ReadVersion)
				if err != nil || admission.Validate() != nil || !admission.MatchesBaseline(receipt) || (len(receipt.PriorSourceHeads) > 0) != withSources || seen[admission.PublicationID] {
					t.Fatalf("admission lost its ordered provider receipt: %+v %v", admission, err)
				}
				seen[admission.PublicationID] = true
			}
			for _, publication := range publications {
				if !seen[publication.ID] {
					t.Fatal("validated publication was not admitted", publication.ID)
				}
			}
			generation, err := storage.PostgresStore.ReadRepositoryMetadataGeneration(ctx, repo.ID)
			if err != nil || generation.Epoch != storage.fenceGeneration.Epoch || generation.Sequence <= storage.fenceGeneration.Sequence {
				t.Fatal("finish did not advance the full generation", generation, storage.fenceGeneration, err)
			}
			if err := gitAdmissionFenceTryAncestryWritePG(ctx, peer, repo); err != nil {
				t.Fatal("committed reconciliation retained the fence", err)
			}
		})
	}
}
