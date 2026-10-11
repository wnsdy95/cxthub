package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prHistoryTestStore interface {
	outbound.PRHistoryStore
	outbound.MetadataStore
	outbound.RepositoryDocumentIdentityStore
	RecordGitRefObservation(context.Context, domain.GitRefObservation, time.Time) error
	EnqueuePRJob(context.Context, domain.PRPromotionJob) (domain.PRPromotionJob, error)
}

func prHistoryFixture(t *testing.T, storage prHistoryTestStore) (context.Context, domain.PRHistoryCheckpoint, domain.PRParticipation) {
	t.Helper()
	ctx := context.Background()
	checkpoint := domain.PRHistoryCheckpoint{
		RepoID: domain.HashContent([]byte(t.Name())), GitOrigin: "https://github.com/example/history",
		InstallationID: 11, ProviderRepositoryID: 22, Version: 1,
		PR:          domain.PullRequestMerge{Number: 17, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)},
		NextAttempt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
	}
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: checkpoint.RepoID, GitRemoteURL: checkpoint.GitOrigin}); err != nil {
		t.Fatal(err)
	}
	participation := domain.PRParticipation{
		RepoID: checkpoint.RepoID, GitOrigin: checkpoint.GitOrigin, ProviderRepositoryID: checkpoint.ProviderRepositoryID,
		PullRequestID: 33, Number: checkpoint.PR.Number, MergeSHA: checkpoint.PR.MergeSHA,
		PublicationID: strings.Repeat("d", 32), Capture: domain.HashContent([]byte("synthetic capture")),
		BranchID: "synthetic-branch", OriginalHead: checkpoint.PR.HeadSHA, ObservationID: strings.Repeat("e", 64),
	}.WithID()
	return ctx, checkpoint, participation
}

func prHistoryObservation(checkpoint domain.PRHistoryCheckpoint, delivery string) domain.GitRefObservation {
	return domain.GitRefObservation{
		RepoID: checkpoint.RepoID, GitOrigin: checkpoint.GitOrigin, Delivery: delivery,
		Source: "github-pull-request", Ref: "refs/heads/" + checkpoint.PR.HeadBranch, After: checkpoint.PR.HeadSHA,
		PullRequest: &domain.GitPullRequestObservation{
			InstallationID: checkpoint.InstallationID, RepositoryID: checkpoint.ProviderRepositoryID, ID: 33,
			Number: checkpoint.PR.Number, Action: "opened", HeadRepositoryID: checkpoint.ProviderRepositoryID,
			BaseRepositoryID: checkpoint.ProviderRepositoryID, BaseRef: checkpoint.PR.BaseBranch,
		},
	}.WithID()
}

func checkPRHistoryCAS(t *testing.T, storage prHistoryTestStore, replica outbound.PRHistoryStore) {
	t.Helper()
	ctx, checkpoint, _ := prHistoryFixture(t, storage)
	if _, err := storage.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("missing checkpoint", err)
	}
	for _, expected := range []int64{-1, 1, math.MaxInt64} {
		if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, expected); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("invalid CAS accepted", expected, err)
		}
	}
	missing := checkpoint
	missing.Version = 2
	if err := storage.SavePRHistoryCheckpoint(ctx, missing, 1); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("update inserted a missing checkpoint", err)
	}
	for _, version := range []int64{1, 2} {
		var workers sync.WaitGroup
		results := make(chan error, 12)
		start := make(chan struct{})
		for index := range 12 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				next := checkpoint
				next.Version = version
				next.Cursor = fmt.Sprintf("provider-page-%d", index)
				writer := outbound.PRHistoryStore(storage)
				if index%2 == 0 {
					writer = replica
				}
				results <- writer.SavePRHistoryCheckpoint(ctx, next, version-1)
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		accepted := 0
		for err := range results {
			if err == nil {
				accepted++
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Fatal(err)
			}
		}
		if accepted != 1 {
			t.Fatalf("version %d accepted %d concurrent writers", version, accepted)
		}
	}
	current, err := replica.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
	if err != nil || current.Version != 2 {
		t.Fatal(current, err)
	}
	resolved := current
	resolved.Version++
	resolved.PullRequestID = 33
	resolved.MergedAt = checkpoint.NextAttempt.Add(-time.Hour)
	resolved.HistoryCheckedAt = checkpoint.NextAttempt
	resolved.Cursor = ""
	resolved.Complete = true
	resolved.EvidenceCursor = strings.Repeat("a", 64)
	resolved.MatchCursor = strings.Repeat("b", 64) + ":" + strings.Repeat("c", 32)
	if err := storage.SavePRHistoryCheckpoint(ctx, resolved, current.Version); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*domain.PRHistoryCheckpoint){
		"base":        func(next *domain.PRHistoryCheckpoint) { next.PR.BaseBranch = "other-base" },
		"head":        func(next *domain.PRHistoryCheckpoint) { next.PR.HeadBranch = "other-head" },
		"head-sha":    func(next *domain.PRHistoryCheckpoint) { next.PR.HeadSHA = strings.Repeat("c", 40) },
		"merge-sha":   func(next *domain.PRHistoryCheckpoint) { next.PR.MergeSHA = strings.Repeat("c", 40) },
		"resolved-id": func(next *domain.PRHistoryCheckpoint) { next.PullRequestID++ },
		"merged-at":   func(next *domain.PRHistoryCheckpoint) { next.MergedAt = next.MergedAt.Add(time.Second) },
		"clear-resolution": func(next *domain.PRHistoryCheckpoint) {
			next.PullRequestID = 0
			next.MergedAt = time.Time{}
			next.Complete = false
		},
	}
	for name, mutate := range mutations {
		next := resolved
		next.Version++
		mutate(&next)
		if err := storage.SavePRHistoryCheckpoint(ctx, next, resolved.Version); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("immutable %s accepted: %v", name, err)
		}
	}
	reset := resolved
	reset.Version++
	reset.InstallationID++
	reset.Cursor = "restart"
	reset.EvidenceCursor = ""
	reset.MatchCursor = ""
	reset.Complete = false
	reset.HistoryCheckedAt = time.Time{}
	reset.NextAttempt = reset.NextAttempt.Add(time.Minute)
	reset.MergedAt = reset.MergedAt.In(time.FixedZone("same-instant", 3600))
	if err := storage.SavePRHistoryCheckpoint(ctx, reset, resolved.Version); err != nil {
		t.Fatal("app-controlled reset or reinstall rejected", err)
	}
	if err := storage.SavePRHistoryCheckpoint(ctx, resolved, current.Version); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale reader rewound progress", err)
	}
	got, err := replica.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
	if err != nil || got.Version != reset.Version || got.InstallationID != reset.InstallationID || got.Cursor != reset.Cursor || !got.MergedAt.Equal(reset.MergedAt) || got.EvidenceCursor != "" || got.MatchCursor != "" || got.Complete || !got.HistoryCheckedAt.IsZero() {
		t.Fatal("reset not durable", got, err)
	}
	for _, changeScope := range []func(*domain.PRHistoryCheckpoint){
		func(next *domain.PRHistoryCheckpoint) { next.RepoID = domain.HashContent([]byte("foreign repo")) },
		func(next *domain.PRHistoryCheckpoint) { next.GitOrigin += "-other" },
		func(next *domain.PRHistoryCheckpoint) { next.ProviderRepositoryID++ },
		func(next *domain.PRHistoryCheckpoint) { next.PR.Number++ },
	} {
		other := checkpoint
		changeScope(&other)
		if _, err := storage.GetPRHistoryCheckpoint(ctx, other.RepoID, other.GitOrigin, other.ProviderRepositoryID, other.PR.Number); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("cross-scope checkpoint leak", err)
		}
		if _, err := storage.PutRepo(ctx, domain.Repo{ID: other.RepoID, GitRemoteURL: other.GitOrigin}); err != nil {
			t.Fatal(err)
		}
		if err := storage.SavePRHistoryCheckpoint(ctx, other, 0); err != nil {
			t.Fatal("independent checkpoint scope collided", err)
		}
	}
}

func checkPRParticipations(t *testing.T, storage prHistoryTestStore, replica outbound.PRHistoryStore) {
	t.Helper()
	ctx, checkpoint, participation := prHistoryFixture(t, storage)
	var workers sync.WaitGroup
	for index := range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			writer := outbound.PRHistoryStore(storage)
			if index%2 == 0 {
				writer = replica
			}
			if err := writer.PutPRParticipation(ctx, participation); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	second := participation
	second.ObservationID = strings.Repeat("f", 64)
	second = second.WithID()
	if second.ID == participation.ID {
		t.Fatal("witness omitted from identity")
	}
	if err := storage.PutPRParticipation(ctx, second); err != nil {
		t.Fatal(err)
	}
	corrupt := participation
	corrupt.OriginalHead = strings.Repeat("c", 40)
	if err := storage.PutPRParticipation(ctx, corrupt); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("mismatched content-addressed row accepted")
	}
	otherIncarnation := participation
	otherIncarnation.ProviderRepositoryID++
	otherIncarnation.PullRequestID++
	otherIncarnation = otherIncarnation.WithID()
	if err := storage.PutPRParticipation(ctx, otherIncarnation); err != nil {
		t.Fatal(err)
	}
	for _, changeScope := range []func(*domain.PRParticipation){
		func(next *domain.PRParticipation) { next.RepoID = domain.HashContent([]byte(t.Name() + "foreign")) },
		func(next *domain.PRParticipation) { next.GitOrigin += "-other" },
		func(next *domain.PRParticipation) { next.Number++ },
	} {
		other := participation
		changeScope(&other)
		other = other.WithID()
		if _, err := storage.PutRepo(ctx, domain.Repo{ID: other.RepoID, GitRemoteURL: other.GitOrigin}); err != nil {
			t.Fatal(err)
		}
		if err := storage.PutPRParticipation(ctx, other); err != nil {
			t.Fatal(err)
		}
		rows, err := replica.ListPRParticipations(ctx, other.RepoID, other.GitOrigin, other.Number)
		if err != nil || len(rows) != 1 || rows[0] != other {
			t.Fatal("foreign participation scope", rows, err)
		}
	}
	rows, err := replica.ListPRParticipations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
	if err != nil || len(rows) != 3 {
		t.Fatal("witnesses replaced or scope leaked", rows, err)
	}
	want := map[string]domain.PRParticipation{participation.ID: participation, second.ID: second, otherIncarnation.ID: otherIncarnation}
	previous := ""
	for _, row := range rows {
		if want[row.ID] != row || row.ID <= previous {
			t.Fatal("immutable evidence changed or unstable ordering", row)
		}
		previous = row.ID
	}
}

func checkPRObservationPaging(t *testing.T, storage prHistoryTestStore) {
	t.Helper()
	ctx, checkpoint, _ := prHistoryFixture(t, storage)
	want := map[string]bool{}
	for index := range 104 {
		observation := prHistoryObservation(checkpoint, fmt.Sprintf("delivery-%03d", index))
		if index%2 == 0 {
			observation.Source = "github-pr-reconciliation"
			observation.PullRequest.Action = "force-pushed"
			observation.PullRequest.ProviderEventID = observation.Delivery
			observation.PullRequest.OccurredAt = checkpoint.NextAttempt
			observation.Before = strings.Repeat("c", 40)
			observation.Forced = true
			observation = observation.WithID()
		}
		if err := storage.RecordGitRefObservation(ctx, observation, checkpoint.NextAttempt); err != nil {
			t.Fatal(err)
		}
		want[observation.ID] = true
	}
	for index, changeScope := range []func(*domain.PRHistoryCheckpoint){
		func(next *domain.PRHistoryCheckpoint) { next.RepoID = domain.HashContent([]byte(t.Name() + "foreign")) },
		func(next *domain.PRHistoryCheckpoint) { next.GitOrigin += "-other" },
		func(next *domain.PRHistoryCheckpoint) { next.ProviderRepositoryID++ },
		func(next *domain.PRHistoryCheckpoint) { next.PR.Number++ },
	} {
		other := checkpoint
		changeScope(&other)
		if _, err := storage.PutRepo(ctx, domain.Repo{ID: other.RepoID, GitRemoteURL: other.GitOrigin}); err != nil {
			t.Fatal(err)
		}
		if err := storage.RecordGitRefObservation(ctx, prHistoryObservation(other, fmt.Sprintf("foreign-%d", index)), checkpoint.NextAttempt); err != nil {
			t.Fatal(err)
		}
	}
	push := prHistoryObservation(checkpoint, "push-not-a-pr")
	push.Source = "github-push"
	push.PullRequest = nil
	push = push.WithID()
	if err := storage.RecordGitRefObservation(ctx, push, checkpoint.NextAttempt); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, 0, 102} {
		if _, err := storage.ListPRRefObservations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, "", limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("unbounded observation query", limit, err)
		}
	}
	if _, err := storage.ListPRRefObservations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, "not-an-id", 1); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("invalid cursor", err)
	}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 4; page++ {
		rows, err := storage.ListPRRefObservations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, cursor, 101)
		if err != nil {
			t.Fatal(err)
		}
		if page == 0 && len(rows) != 101 {
			t.Fatal("bounded first page", len(rows))
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if !want[row.ID] || seen[row.ID] || row.ID <= cursor {
				t.Fatal("observation leak, duplicate, or unstable order", row)
			}
			seen[row.ID] = true
			cursor = row.ID
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("lost observations: %d/%d", len(seen), len(want))
	}
	rows, err := storage.ListPRRefObservations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, "", 1)
	if err != nil || len(rows) != 1 || !seen[rows[0].ID] {
		t.Fatal("evidence cursor reset did not rescan", rows, err)
	}
}

func checkPRHistoryDocumentIdentity(t *testing.T, storage prHistoryTestStore) {
	t.Helper()
	ctx, checkpoint, participation := prHistoryFixture(t, storage)
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, checkpoint.RepoID, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	next := checkpoint
	next.Version++
	next.Cursor = "upgraded"
	if err := storage.SavePRHistoryCheckpoint(ctx, next, checkpoint.Version); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("checkpoint crossed identity requirement", err)
	}
	if err := storage.PutPRParticipation(ctx, participation); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("participation crossed identity requirement", err)
	}
	stored, err := storage.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
	if err != nil || !reflect.DeepEqual(stored, checkpoint) {
		t.Fatal("rejected write changed checkpoint", stored, err)
	}
	rows, err := storage.ListPRParticipations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
	if err != nil || len(rows) != 0 {
		t.Fatal("rejected write leaked evidence", rows, err)
	}
	identities := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	compatible := outbound.WithDocumentIdentityCompatibility(ctx, identities, identities)
	if err := storage.SavePRHistoryCheckpoint(compatible, next, checkpoint.Version); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRParticipation(compatible, participation); err != nil {
		t.Fatal(err)
	}
}

func checkPRHistoryJobScheduling(t *testing.T, storage prHistoryTestStore) {
	t.Helper()
	ctx, checkpoint, _ := prHistoryFixture(t, storage)
	jobOrigin := checkpoint.GitOrigin
	checkpoint.GitOrigin += ".git"
	now := checkpoint.NextAttempt
	oldJobs := map[string]bool{}
	for index := range 105 {
		next := checkpoint
		next.PR.Number += index
		created := now.Add(time.Duration(index-200) * time.Minute)
		state := []string{"completed", "attention", "waiting", "running", "retrying"}[index%5]
		job := domain.PRPromotionJob{
			ID: domain.PRPromotionID(next.RepoID, next.PR.Number), RepoID: next.RepoID, GitOrigin: jobOrigin,
			PR: next.PR, State: state, CreatedAt: created, UpdatedAt: created, NextAttempt: now.Add(24 * time.Hour),
		}
		if _, err := storage.EnqueuePRJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if index < 5 {
			oldJobs[job.ID] = true
		}
		switch index {
		case 0:
			next.Complete = true
			next.PullRequestID = 33
			next.MergedAt = now.Add(-time.Hour)
			next.HistoryCheckedAt = now
		case 1:
			next.GitOrigin = jobOrigin
			next.NextAttempt = now.Add(time.Hour)
		case 2:
			next.ProviderRepositoryID++
			next.NextAttempt = now.Add(time.Hour)
		case 3, 4:
			continue
		default:
			next.NextAttempt = now.Add(time.Hour)
		}
		if err := storage.SavePRHistoryCheckpoint(ctx, next, 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, foreignRepo := range []bool{false, true} {
		job := domain.PRPromotionJob{RepoID: checkpoint.RepoID, PR: checkpoint.PR, GitOrigin: jobOrigin + "-foreign", State: "completed", CreatedAt: now}
		job.PR.Number = 500
		if foreignRepo {
			job.RepoID = domain.HashContent([]byte(t.Name() + "foreign"))
			job.GitOrigin = jobOrigin
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: job.RepoID}); err != nil {
				t.Fatal(err)
			}
		}
		job.ID = domain.PRPromotionID(job.RepoID, job.PR.Number)
		if _, err := storage.EnqueuePRJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{0, -1, 102} {
		if _, err := storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, jobOrigin, checkpoint.ProviderRepositoryID, now, limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("unbounded history work query", limit, err)
		}
	}
	jobs, err := storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, jobOrigin, checkpoint.ProviderRepositoryID, now, 101)
	if err != nil || len(jobs) != 5 {
		t.Fatal("old history jobs starved by newest 100 or foreign scopes", len(jobs), err)
	}
	if jobs[len(jobs)-1].PR.Number != checkpoint.PR.Number {
		t.Fatal("due checkpoint was not sorted after earlier job creation times", jobs)
	}
	for index := 1; index < len(jobs)-1; index++ {
		if !jobs[index-1].CreatedAt.Before(jobs[index].CreatedAt) {
			t.Fatal("missing checkpoints are not ordered by creation time")
		}
	}
	seen := map[string]bool{}
	for range 6 {
		jobs, err := storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, jobOrigin, checkpoint.ProviderRepositoryID, now, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			break
		}
		job := jobs[0]
		if !oldJobs[job.ID] || seen[job.ID] {
			t.Fatal("history reservation leaked or repeated work", job)
		}
		seen[job.ID] = true
		next, err := storage.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, job.PR.Number)
		if errors.Is(err, domain.ErrNotFound) {
			next = checkpoint
			next.PR = job.PR
			next.Version = 0
		} else if err != nil {
			t.Fatal(err)
		}
		next.Version++
		next.NextAttempt = now.Add(time.Hour)
		if err := storage.SavePRHistoryCheckpoint(ctx, next, next.Version-1); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != len(oldJobs) {
		t.Fatal("bounded reservations failed to drain old backlog", seen)
	}
	jobs, err = storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, jobOrigin, checkpoint.ProviderRepositoryID, now, 101)
	if err != nil || len(jobs) != 0 {
		t.Fatal("future reservation still due", len(jobs), err)
	}
	jobs, err = storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, jobOrigin, checkpoint.ProviderRepositoryID, now.Add(time.Hour), 101)
	if err != nil || len(jobs) != 101 {
		t.Fatal("future due boundary or maximum page lost jobs", len(jobs), err)
	}
	for index := 1; index < len(jobs); index++ {
		if jobs[index-1].ID >= jobs[index].ID {
			t.Fatal("equal due times lack stable job ordering")
		}
	}
}

func checkPRHistoryJobFairness(t *testing.T, storage prHistoryTestStore) {
	t.Helper()
	ctx, checkpoint, _ := prHistoryFixture(t, storage)
	now := checkpoint.NextAttempt
	checkpoint.NextAttempt = now.Add(-time.Hour)
	checkpoint.Cursor = "resume-provider-page"
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
		t.Fatal(err)
	}
	resuming := domain.PRPromotionJob{
		ID: domain.PRPromotionID(checkpoint.RepoID, checkpoint.PR.Number), RepoID: checkpoint.RepoID,
		GitOrigin: checkpoint.GitOrigin, PR: checkpoint.PR, State: "completed", CreatedAt: now.Add(-2 * time.Hour),
	}
	if _, err := storage.EnqueuePRJob(ctx, resuming); err != nil {
		t.Fatal(err)
	}
	for index, created := range []time.Time{now.Add(-30 * time.Minute), now.Add(-30 * time.Minute), now.Add(time.Hour)} {
		unseen := resuming
		unseen.PR.Number += index + 1
		unseen.ID = domain.PRPromotionID(unseen.RepoID, unseen.PR.Number)
		unseen.CreatedAt = created
		if _, err := storage.EnqueuePRJob(ctx, unseen); err != nil {
			t.Fatal(err)
		}
		jobs, err := storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, now, 1)
		if err != nil || len(jobs) != 1 || jobs[0].ID != resuming.ID {
			t.Fatalf("unseen arrival %d starved overdue checkpoint: %+v %v", index, jobs, err)
		}
	}
	checkpoint.Version++
	checkpoint.NextAttempt = now.Add(2 * time.Hour)
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, checkpoint.Version-1); err != nil {
		t.Fatal(err)
	}
	jobs, err := storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, now, 101)
	if err != nil || len(jobs) != 3 {
		t.Fatal("missing checkpoints must remain eligible even with future creation times", jobs, err)
	}
	if !jobs[0].CreatedAt.Equal(now.Add(-30*time.Minute)) || !jobs[1].CreatedAt.Equal(jobs[0].CreatedAt) || jobs[0].ID >= jobs[1].ID || !jobs[2].CreatedAt.Equal(now.Add(time.Hour)) {
		t.Fatal("missing checkpoints must sort by creation time, then job ID", jobs)
	}
}

func TestFSPRHistory(t *testing.T) {
	directory := t.TempDir()
	storage, replica := NewFSStore(directory), NewFSStore(directory)
	t.Run("CAS", func(t *testing.T) { checkPRHistoryCAS(t, storage, replica) })
	t.Run("participations", func(t *testing.T) { checkPRParticipations(t, storage, replica) })
	t.Run("observations", func(t *testing.T) { checkPRObservationPaging(t, storage) })
	t.Run("document-identity", func(t *testing.T) { checkPRHistoryDocumentIdentity(t, storage) })
	t.Run("job-scheduling", func(t *testing.T) { checkPRHistoryJobScheduling(t, storage) })
	t.Run("job-fairness", func(t *testing.T) { checkPRHistoryJobFairness(t, storage) })
}

func TestFSPRHistoryReopen(t *testing.T) {
	directory := t.TempDir()
	storage := NewFSStore(directory)
	ctx, checkpoint, participation := prHistoryFixture(t, storage)
	checkpoint.EvidenceCursor = strings.Repeat("f", 64)
	checkpoint.MatchCursor = strings.Repeat("f", 64) + ":" + strings.Repeat("a", 32)
	checkpoint.HistoryCheckedAt = checkpoint.NextAttempt
	if err := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPRParticipation(ctx, participation); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFSStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
	if err != nil || !reflect.DeepEqual(got, checkpoint) {
		t.Fatal("checkpoint lost on reopen", got, err)
	}
	rows, err := reopened.ListPRParticipations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
	if err != nil || len(rows) != 1 || rows[0] != participation {
		t.Fatal("participation lost on reopen", rows, err)
	}
}

func TestFSPRHistoryCorruption(t *testing.T) {
	for _, kind := range []string{"checkpoint-scope", "checkpoint-payload", "participation-key", "observation-key"} {
		t.Run(kind, func(t *testing.T) {
			storage := NewFSStore(t.TempDir())
			ctx, checkpoint, participation := prHistoryFixture(t, storage)
			var path string
			var payload any
			switch kind {
			case "checkpoint-scope":
				path = storage.prHistoryCheckpointPath(checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
				other := checkpoint
				other.ProviderRepositoryID++
				payload = other
			case "checkpoint-payload":
				path = storage.prHistoryCheckpointPath(checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
				payload = map[string]string{"repo_id": "invalid"}
			case "participation-key":
				path = storage.gitEvidencePath("pr-participations", participation.RepoID, participation.ID)
				other := participation
				other.ObservationID = strings.Repeat("f", 64)
				payload = other.WithID()
			case "observation-key":
				observation := prHistoryObservation(checkpoint, "original")
				path = storage.gitEvidencePath("git-observations", observation.RepoID, observation.ID)
				payload = prHistoryObservation(checkpoint, "replaced")
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(path, raw); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "checkpoint-scope", "checkpoint-payload":
				_, err = storage.GetPRHistoryCheckpoint(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
				if updateErr := storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); !errors.Is(updateErr, domain.ErrIntegrity) {
					t.Fatal("corruption overwritten", updateErr)
				}
			case "participation-key":
				_, err = storage.ListPRParticipations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.PR.Number)
				if putErr := storage.PutPRParticipation(ctx, participation); !errors.Is(putErr, domain.ErrIntegrity) {
					t.Fatal("mismatched immutable duplicate accepted", putErr)
				}
			case "observation-key":
				_, err = storage.ListPRRefObservations(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, "", 1)
			}
			if !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("corruption trusted", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(raw) {
				t.Fatal("rejected operation changed durable bytes", err)
			}
		})
	}
}

func TestFSPRHistoryJobCorruption(t *testing.T) {
	for _, kind := range []string{"job-key", "checkpoint-scope"} {
		t.Run(kind, func(t *testing.T) {
			storage := NewFSStore(t.TempDir())
			ctx, checkpoint, _ := prHistoryFixture(t, storage)
			job := domain.PRPromotionJob{ID: domain.PRPromotionID(checkpoint.RepoID, checkpoint.PR.Number), RepoID: checkpoint.RepoID, GitOrigin: checkpoint.GitOrigin, PR: checkpoint.PR, State: "completed", CreatedAt: checkpoint.NextAttempt}
			if _, err := storage.EnqueuePRJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			var path string
			var payload any
			if kind == "job-key" {
				path = storage.prJobPath(job.RepoID, job.ID)
				job.PR.Number++
				job.ID = domain.PRPromotionID(job.RepoID, job.PR.Number)
				payload = job
			} else {
				path = storage.prHistoryCheckpointPath(checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
				other := checkpoint
				other.ProviderRepositoryID++
				payload = other
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(path, raw); err != nil {
				t.Fatal(err)
			}
			jobs, err := storage.ListPRHistoryJobs(ctx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.NextAttempt, 1)
			if !errors.Is(err, domain.ErrIntegrity) || len(jobs) != 0 {
				t.Fatal("corrupt work selection leaked", jobs, err)
			}
		})
	}
}
