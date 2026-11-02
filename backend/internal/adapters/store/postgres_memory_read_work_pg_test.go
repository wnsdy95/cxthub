//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadWorkPG(test *testing.T) (*PostgresStore, context.Context, domain.MemoryReadTarget) {
	test.Helper()
	storage, ctx, repo := catalogPG(test)
	snapshot := initializationPGSnapshot(test, storage, repo, "memory demand")
	target := domain.MemoryReadTarget{Version: domain.MemoryReadWorkVersion, Rules: domain.MemoryReadRules, RepoID: repo,
		Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot.ID, CodeCommit: fmt.Sprintf("%040x", 1)}}
	if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
		test.Fatal(err)
	}
	return storage, ctx, target
}

func memoryReadWorkRequestPG(test *testing.T, storage *PostgresStore, ctx context.Context, target domain.MemoryReadTarget) domain.MemoryReadWork {
	test.Helper()
	work, err := storage.RequestMemoryReadWork(ctx, target)
	if err != nil {
		test.Fatal(err)
	}
	return work
}

func memoryReadWorkClaimPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) domain.MemoryReadWork {
	test.Helper()
	work, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{repo}, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	return work
}

func memoryReadWorkStatusPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.MemoryReadWork) domain.MemoryReadWork {
	test.Helper()
	work, err := storage.ReadMemoryReadWork(ctx, job.Target.RepoID, job.ID)
	if err != nil {
		test.Fatal(err)
	}
	return work
}

func memoryReadWorkExpirePG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.MemoryReadWork) {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET lease_until=clock_timestamp()-interval '1 second',
 next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID))
}

func memoryReadWorkOutputPG(test *testing.T) (*PostgresStore, context.Context, domain.MemoryReadWork, domain.MemoryReadQualification, domain.MemoryReadBundle) {
	test.Helper()
	storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
	memoryReadComparisonRecordPG(test, storage, ctx, job, output)
	return storage, ctx, job, qualification, output
}

func memoryReadWorkOutputWithoutComparisonPG(test *testing.T) (*PostgresStore, context.Context, domain.MemoryReadWork, domain.MemoryReadQualification, domain.MemoryReadBundle) {
	test.Helper()
	storage, ctx, target := memoryReadWorkPG(test)
	digest := memoryCaptureTestDigest()
	digest.SnapshotID = target.Selection.SnapshotID
	hash, err := storage.PutMemory(ctx, target.RepoID, digest)
	if err != nil {
		test.Fatal(err)
	}
	object := memoryWitnessPGCapture(test, storage, ctx, target.RepoID, hash)
	capture, output := memoryReadPGFixture(test, storage, ctx, target.RepoID, 130)
	capture.Header.Selection = target.Selection
	capture = memoryBundlePGSeal(test, ctx, capture.Header, capture.Parts)
	header := output.Header
	header.CaptureID, header.View.Selection = capture.ID, target.Selection
	header.Dependencies = []domain.MemoryReadDependency{{Hash: hash, Available: true}}
	var items []domain.EffectiveMemoryItem
	for ordinal, chunk := range output.Chunks {
		decoded, err := domain.DecodeMemoryReadChunk(ctx, output.Header, ordinal, chunk.Bytes)
		if err != nil {
			test.Fatal(err)
		}
		items = append(items, decoded...)
	}
	output = memoryReadPGSeal(test, ctx, header, items)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, output)
	qualification := domain.MemoryReadQualification{Version: domain.MemoryReadQualificationVersion, ReadID: output.ID,
		CaptureID: capture.ID, Source: output.Header.Source, Witnesses: []domain.MemoryObjectWitness{object.Witness}}
	memoryReadWorkRequestPG(test, storage, ctx, target)
	job := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	return storage, ctx, job, qualification, output
}

func TestPGMemoryReadWorkRequestRestartQuotaAndScope(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	initial := memoryReadWorkRequestPG(test, storage, ctx, target)
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			actual, err := storage.RequestMemoryReadWork(ctx, target)
			if err == nil && !reflect.DeepEqual(actual, initial) {
				err = errors.New("coalescing changed intent")
			}
			failures <- err
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			test.Fatal(err)
		}
	}
	for ordinal := 2; ordinal <= domain.MaxPendingMemoryReadWork; ordinal++ {
		next := target
		next.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal)
		memoryReadWorkRequestPG(test, storage, ctx, next)
	}
	next := target
	next.Selection.CodeCommit = fmt.Sprintf("%040x", domain.MaxPendingMemoryReadWork+1)
	if _, err := storage.RequestMemoryReadWork(ctx, next); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("pending cap", err)
	}
	if got := memoryReadWorkRequestPG(test, storage, ctx, target); !reflect.DeepEqual(got, initial) {
		test.Fatal("at-cap coalescing changed intent")
	}
	foreign := memorySourceRepoPG(test, ctx, storage, "foreign demand")
	if _, err := storage.RequestMemoryReadWork(ctx, domain.MemoryReadTarget{Version: target.Version, Rules: target.Rules, RepoID: foreign, Selection: target.Selection}); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("scope default enabled", err)
	}
	if err := storage.ConfigureMemoryReadWork(ctx, foreign, true); err != nil {
		test.Fatal(err)
	}
	foreignTarget := target
	foreignTarget.RepoID = foreign
	if _, err := storage.RequestMemoryReadWork(ctx, foreignTarget); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("foreign snapshot accepted", err)
	}
	if _, err := storage.ReadMemoryReadWork(ctx, foreign, initial.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("foreign status", err)
	}
	storage.Close()
	peer, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	if got := memoryReadWorkStatusPG(test, peer, ctx, initial); !reflect.DeepEqual(got, initial) {
		test.Fatal("restart lost intent")
	}
	if got := memoryReadWorkClaimPG(test, peer, ctx, target.RepoID); got.ID != initial.ID || got.Version <= initial.Version || got.Attempts != 1 {
		test.Fatal("restart claim", got)
	}
}

func TestPGMemoryReadWorkParallelClaimsAndConfiguredRepos(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	for ordinal := range 12 {
		next := target
		next.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal+1)
		memoryReadWorkRequestPG(test, storage, ctx, next)
	}
	for _, repos := range [][]domain.ContentHash{nil, {domain.HashContent([]byte("not configured"))}} {
		if _, err := storage.ClaimMemoryReadWork(ctx, repos, time.Minute); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("unconfigured claim", err)
		}
	}
	for _, lease := range []time.Duration{0, -1, time.Nanosecond, 6 * time.Minute} {
		if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, lease); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("invalid lease", err)
		}
	}
	if _, err := storage.ClaimMemoryReadWork(ctx, make([]domain.ContentHash, domain.MaxMemoryReadWorkerRepos+1), time.Minute); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("unbounded repo scope", err)
	}
	type result struct {
		job domain.MemoryReadWork
		err error
	}
	results := make(chan result, 12)
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			job, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, time.Minute)
			results <- result{job, err}
		}()
	}
	group.Wait()
	close(results)
	seen := make(map[domain.ContentHash]bool)
	for actual := range results {
		if actual.err != nil || seen[actual.job.ID] || actual.job.State != "running" || actual.job.Attempts != 1 {
			test.Fatal("parallel claim", actual, seen)
		}
		seen[actual.job.ID] = true
	}
	if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("live lease claimed again", err)
	}
}

func TestPGMemoryReadWorkReclaimBackoffExhaustionAndSource(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	memoryReadWorkRequestPG(test, storage, ctx, target)
	job := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if err := storage.RenewMemoryReadWork(ctx, job, 2*time.Minute); err != nil {
		test.Fatal(err)
	}
	if actual := memoryReadWorkStatusPG(test, storage, ctx, job); !actual.LeaseUntil.After(job.LeaseUntil) {
		test.Fatal("renew did not extend")
	}
	if err := storage.FailMemoryReadWork(ctx, job, "database password leaked", true); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("raw error accepted", err)
	}
	for attempt := 1; attempt <= domain.MaxMemoryReadWorkAttempts; attempt++ {
		if job.Attempts != attempt {
			test.Fatal("attempt counter", job)
		}
		if err := storage.FailMemoryReadWork(ctx, job, "temporary_failure", true); err != nil {
			test.Fatal(err)
		}
		status := memoryReadWorkStatusPG(test, storage, ctx, job)
		if attempt == domain.MaxMemoryReadWorkAttempts {
			if status.State != "rejected" || status.Reason != "attempts_exhausted" {
				test.Fatal("exhaustion", status)
			}
			break
		}
		if status.State != "retrying" || status.NextAttempt.Sub(status.UpdatedAt) < time.Duration(1<<(attempt-1))*time.Second-time.Millisecond {
			test.Fatal("backoff", status)
		}
		if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("backoff ignored", err)
		}
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(target.RepoID))
		job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	}
	if status := memoryReadWorkRequestPG(test, storage, ctx, target); status.State != "rejected" || status.Attempts != 6 {
		test.Fatal("rejected demand reset attempts", status)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='new source' WHERE repo_id=$1`, string(target.RepoID))
	requeued := memoryReadWorkRequestPG(test, storage, ctx, target)
	if requeued.Attempts != 0 || requeued.Source == job.Source || requeued.Version <= job.Version {
		test.Fatal("new source failed to reset", requeued)
	}
	for _, action := range []func() error{
		func() error { return storage.RenewMemoryReadWork(ctx, job, time.Minute) },
		func() error { return storage.FailMemoryReadWork(ctx, job, "temporary_failure", true) },
	} {
		if err := action(); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("old fence accepted", err)
		}
	}
	job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	for attempt := 1; attempt < 6; attempt++ {
		prior := job
		memoryReadWorkExpirePG(test, storage, ctx, job)
		job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
		if job.Attempts != attempt+1 || job.Version <= prior.Version {
			test.Fatal("reclaim fence", job)
		}
		if err := storage.FailMemoryReadWork(ctx, prior, "temporary_failure", true); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("reclaimed fence accepted", err)
		}
	}
	memoryReadWorkExpirePG(test, storage, ctx, job)
	next := target
	next.Content = "claims"
	waiting := memoryReadWorkRequestPG(test, storage, ctx, next)
	if got := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID); got.ID != waiting.ID {
		test.Fatal("exhausted lease blocked next candidate", got)
	}
	if status := memoryReadWorkStatusPG(test, storage, ctx, job); status.State != "rejected" || status.Reason != "attempts_exhausted" {
		test.Fatal("expired exhaustion not durable", status)
	}
}

func TestPGMemoryReadWorkClaimRefreshAndDisabledFence(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	initial := memoryReadWorkRequestPG(test, storage, ctx, target)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='claim refresh' WHERE repo_id=$1`, string(target.RepoID))
	job := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if job.Source == initial.Source || job.Attempts != 1 {
		test.Fatal("claim did not refresh source", job)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='failure refresh' WHERE repo_id=$1`, string(target.RepoID))
	if err := storage.FailMemoryReadWork(ctx, job, "invalid_source", false); err != nil {
		test.Fatal(err)
	}
	if status := memoryReadWorkStatusPG(test, storage, ctx, job); status.State != "retrying" || status.Reason != "source_changed" {
		test.Fatal("changed source became terminal", status)
	}
	memoryReadWorkRequestPG(test, storage, ctx, target)
	job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if err := storage.ConfigureMemoryReadWork(ctx, target.RepoID, false); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("disabled claim", err)
	}
	if _, err := storage.RequestMemoryReadWork(ctx, target); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("disabled request", err)
	}
	if err := storage.ConfigureMemoryReadWork(ctx, target.RepoID, true); err != nil {
		test.Fatal(err)
	}
	if err := storage.RenewMemoryReadWork(ctx, job, time.Minute); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("reenable resurrected old worker", err)
	}
	if status := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID); status.Version <= job.Version {
		test.Fatal("disable did not fence")
	}
}

func TestPGMemoryReadWorkRetainedCapAndEvictionFence(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); err != nil {
		test.Fatal(err)
	}
	active := job.Target
	active.Content = "prompt"
	waiting := memoryReadWorkRequestPG(test, storage, ctx, active)
	for ordinal := 2; ordinal <= domain.MaxMemoryReadTargets; ordinal++ {
		target := job.Target
		target.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal)
		work := memoryReadWorkRequestPG(test, storage, ctx, target)
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state='rejected',reason='invalid_source' WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(work.ID))
	}
	if _, err := storage.ReadMemoryReadWork(ctx, job.Target.RepoID, job.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("oldest terminal not trimmed", err)
	}
	if got := memoryReadWorkStatusPG(test, storage, ctx, waiting); got.State != "waiting" {
		test.Fatal("active intent evicted")
	}
	if _, err := storage.ReadMemoryReadHeader(ctx, job.Target.RepoID, output.ID); err != nil {
		test.Fatal("eviction destroyed retained result", err)
	}
	if memoryQualificationCountPG(test, storage, ctx, job.Target.RepoID) != 1 {
		test.Fatal("eviction destroyed qualification")
	}
	recreated := memoryReadWorkRequestPG(test, storage, ctx, job.Target)
	if recreated.Version <= job.Version {
		test.Fatal("eviction reused old fence")
	}
	if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("evicted worker republished", err)
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, string(job.Target.RepoID)).Scan(&count); err != nil || count != domain.MaxMemoryReadTargets {
		test.Fatal("retained cap", count, err)
	}
}

func TestPGMemoryReadWorkOwnershipAndCompatibility(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	check := func(bound context.Context) error {
		for _, candidate := range []*PostgresStore{storage, {pool: storage.pool}} {
			if err := candidate.ConfigureMemoryReadWork(bound, job.Target.RepoID, true); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed configure", err)
			}
			if _, err := candidate.RequestMemoryReadWork(bound, job.Target); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed request", err)
			}
			if _, err := candidate.ClaimMemoryReadWork(bound, []domain.ContentHash{job.Target.RepoID}, time.Minute); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed claim", err)
			}
			if err := candidate.RenewMemoryReadWork(bound, job, time.Minute); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed renew", err)
			}
			if err := candidate.FailMemoryReadWork(bound, job, "temporary_failure", true); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed fail", err)
			}
			if _, err := candidate.FinishMemoryReadWork(bound, job, qualification); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed finish", err)
			}
		}
		return nil
	}
	if err := storage.WithinReadSnapshot(ctx, check); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, job.Target.RepoID, check); err != nil {
		test.Fatal(err)
	}
	if err := check(outbound.WithDocReadOnly(ctx)); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, string(job.Target.RepoID), domain.DocumentIdentityRootV1)
	if _, err := storage.RequestMemoryReadWork(ctx, job.Target); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("identity compatibility bypassed", err)
	}
}
