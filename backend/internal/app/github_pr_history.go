package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type githubPRHistoryWork struct {
	connection domain.GitHubConnection
	binding    domain.GitHubBinding
	remote     domain.GitHubRepository
	checkpoint domain.PRHistoryCheckpoint
	due        time.Time
}

type prHistoryAttemptError struct{ cause error }

func (failure *prHistoryAttemptError) Error() string { return failure.cause.Error() }
func (failure *prHistoryAttemptError) Unwrap() error { return failure.cause }

func (g *GitHubWorker) reconcilePRHistories(ctx context.Context, limit int) error {
	reader, supported := g.remote.(outbound.GitHubPRHistory)
	storage, available := g.metadata.(outbound.PRHistoryStore)
	if !supported || !available {
		return nil
	}
	connections, err := g.store.ListGitHubConnections(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	work := []githubPRHistoryWork{}
	for _, connection := range connections {
		if !connection.Enabled || connection.Installation.Suspended || connection.CheckedAt.Before(now.Add(-10*time.Minute)) || (connection.Status != "connected" && connection.Status != "team_access_required") {
			continue
		}
		for _, binding := range connection.Bindings {
			if err := g.work.checkDocumentIdentity(ctx, binding.ContextRepoID, true); err != nil {
				continue
			}
			if _, err := g.binding(ctx, connection, binding); err != nil {
				continue
			}
			var remote domain.GitHubRepository
			for _, candidate := range connection.Repositories {
				if candidate.ID == binding.ExternalID {
					remote = candidate
					break
				}
			}
			if remote.ID == 0 {
				continue
			}
			pending, err := storage.ListPRHistoryJobs(ctx, binding.ContextRepoID, binding.GitOrigin, normalizeGitURL(binding.GitOrigin), binding.ExternalID, now, 101)
			if err != nil {
				log.Printf("GitHub PR history repository %s deferred", binding.ContextRepoID)
				continue
			}
			for _, job := range pending {
				if job.GitOrigin != normalizeGitURL(binding.GitOrigin) {
					continue
				}
				checkpoint, err := storage.GetPRHistoryCheckpoint(ctx, job.RepoID, binding.GitOrigin, remote.ID, job.PR.Number)
				if errors.Is(err, domain.ErrNotFound) {
					checkpoint = domain.PRHistoryCheckpoint{RepoID: job.RepoID, GitOrigin: binding.GitOrigin, InstallationID: connection.Installation.ID, ProviderRepositoryID: remote.ID, PR: job.PR}
				} else if err != nil {
					continue
				}
				if checkpoint.PR != job.PR {
					continue
				}
				if !checkpoint.NextAttempt.After(now) {
					due := checkpoint.NextAttempt
					if checkpoint.Version == 0 {
						due = job.CreatedAt
					}
					work = append(work, githubPRHistoryWork{connection: connection, binding: binding, remote: remote, checkpoint: checkpoint, due: due})
				}
			}
		}
	}
	sort.Slice(work, func(left, right int) bool {
		if !work[left].due.Equal(work[right].due) {
			return work[left].due.Before(work[right].due)
		}
		leftKey := fmt.Sprintf("%s:%020d:%020d", work[left].checkpoint.RepoID, work[left].remote.ID, work[left].checkpoint.PR.Number)
		rightKey := fmt.Sprintf("%s:%020d:%020d", work[right].checkpoint.RepoID, work[right].remote.ID, work[right].checkpoint.PR.Number)
		return leftKey < rightKey
	})
	completed := 0
	attempts := 0
	for _, item := range work {
		if limit <= 0 || attempts >= limit*4 {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := g.reconcilePRHistory(ctx, reader, storage, item); err != nil {
			var attempted *prHistoryAttemptError
			if errors.As(err, &attempted) {
				attempts++
			}
			if !errors.Is(err, domain.ErrConflict) {
				log.Printf("GitHub PR history %s/%d deferred", item.checkpoint.RepoID, item.checkpoint.PR.Number)
			}
		} else {
			attempts++
			completed++
			if completed >= limit {
				break
			}
		}
	}
	return nil
}

func (g *GitHubWorker) reconcilePRHistory(ctx context.Context, reader outbound.GitHubPRHistory, storage outbound.PRHistoryStore, item githubPRHistoryWork) (result error) {
	started := false
	defer func() {
		if started && result != nil {
			result = &prHistoryAttemptError{cause: result}
		}
	}()
	ctx = g.workerDocumentIdentityContext(inbound.WithSystemActor(ctx))
	checkpoint := item.checkpoint
	now := time.Now().UTC()
	if checkpoint.RepoID != item.binding.ContextRepoID || checkpoint.GitOrigin != item.binding.GitOrigin || checkpoint.ProviderRepositoryID != item.binding.ExternalID || checkpoint.ProviderRepositoryID != item.remote.ID {
		return domain.ErrIntegrity
	}
	err := repositoryWriteErrorWithWork(context.WithValue(ctx, revisionScopeKey{}, "none"), g.work, checkpoint.RepoID, func(tx context.Context) error {
		if _, err := g.binding(tx, item.connection, item.binding); err != nil {
			return err
		}
		current, err := storage.GetPRHistoryCheckpoint(tx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
		if errors.Is(err, domain.ErrNotFound) && checkpoint.Version == 0 {
			current = checkpoint
		} else if err != nil {
			return err
		}
		if current != checkpoint {
			return domain.ErrConflict
		}
		reserved := checkpoint
		reserved.Version++
		reserved.NextAttempt = now.Add(2 * time.Minute)
		return storage.SavePRHistoryCheckpoint(tx, reserved, checkpoint.Version)
	})
	if err != nil {
		return err
	}
	started = true
	checkpoint.Version++
	checkpoint.NextAttempt = now.Add(2 * time.Minute)
	var page *domain.GitHubPRHistoryPage
	var readErr error
	if !checkpoint.Complete || !checkpoint.HistoryCheckedAt.After(now.Add(-10*time.Minute)) {
		cursor := checkpoint.Cursor
		if checkpoint.Complete {
			cursor = ""
		}
		response, err := reader.PullRequestHeadHistory(ctx, item.connection.Installation.ID, item.remote, checkpoint.PR.Number, cursor)
		readErr = err
		if readErr == nil {
			readErr = validatePRHistoryPage(checkpoint, response, cursor)
		}
		if readErr == nil {
			page = &response
		}
	}
	next := checkpoint
	next.Version++
	next.InstallationID = item.connection.Installation.ID
	observations := []domain.GitRefObservation{}
	if page != nil {
		next.PullRequestID, next.MergedAt = page.PullRequestDatabaseID, page.MergedAt
		next.Complete = !page.HasNextPage
		next.Cursor = ""
		if page.HasNextPage {
			next.Cursor = page.EndCursor
			next.NextAttempt = now.Add(20 * time.Second)
		}
		next.HistoryCheckedAt = now
		for _, rewrite := range page.Rewrites {
			if rewrite.CreatedAt.After(page.MergedAt) {
				continue
			}
			metadata := &domain.GitPullRequestObservation{InstallationID: item.connection.Installation.ID, RepositoryID: item.remote.ID, ID: page.PullRequestDatabaseID, Number: page.Number, Action: "force-pushed", HeadRepositoryID: page.HeadRepositoryID, BaseRepositoryID: page.RepositoryID, BaseRef: page.BaseBranch, ProviderEventID: rewrite.ID, OccurredAt: rewrite.CreatedAt}
			delivery := githubHash(fmt.Sprintf("%d:%d:%d:%s", item.connection.Installation.ID, item.remote.ID, page.PullRequestDatabaseID, rewrite.ID))
			observations = append(observations, domain.GitRefObservation{RepoID: checkpoint.RepoID, GitOrigin: checkpoint.GitOrigin, Source: "github-pr-reconciliation", Ref: "refs/heads/" + page.HeadBranch, Before: rewrite.Before, After: rewrite.After, Forced: true, Delivery: delivery, PullRequest: metadata}.WithID())
		}
	}
	if readErr != nil && (checkpoint.PullRequestID == 0 || errors.Is(readErr, domain.ErrForbidden) || errors.Is(readErr, domain.ErrNotFound) || errors.Is(readErr, domain.ErrIntegrity) || errors.Is(readErr, domain.ErrValidation)) {
		return readErr
	}
	prepared, err := g.preparePRParticipations(ctx, storage, &next, observations)
	if err != nil {
		return err
	}
	err = repositoryReadForContributionWithPublications(ctx, g.publications, next, &prepared)
	if err != nil {
		return err
	}
	writeErr := repositoryWriteErrorWithWork(evidenceWriteContext(ctx), g.work, checkpoint.RepoID, func(tx context.Context) error {
		if _, err := g.binding(tx, item.connection, item.binding); err != nil {
			return err
		}
		current, err := storage.GetPRHistoryCheckpoint(tx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number)
		if errors.Is(err, domain.ErrNotFound) && checkpoint.Version == 0 {
			current = checkpoint
		} else if err != nil {
			return err
		}
		if current != checkpoint {
			return domain.ErrConflict
		}
		revisions := g.metadata.(outbound.RepositoryRevisions)
		revision, err := revisions.RepositoryRevision(tx, checkpoint.RepoID)
		if err != nil {
			return err
		}
		if revision.Graph != prepared.revision.Graph || revision.Evidence != prepared.revision.Evidence {
			return domain.ErrConflict
		}
		if page != nil {
			scans, ok := g.metadata.(outbound.GitScanStore)
			if !ok {
				return domain.ErrNotFound
			}
			for _, observation := range observations {
				if err := scans.RecordGitRefObservation(tx, observation, now); err != nil {
					return err
				}
			}
		}
		for _, participation := range prepared.participations {
			if _, err := g.metadata.GetSnapshot(tx, checkpoint.RepoID, participation.Capture); err != nil {
				return err
			}
			if err := storage.PutPRParticipation(tx, participation); err != nil {
				return err
			}
		}
		if err := g.publications.applyPreparedPRContributions(tx, next, prepared); err != nil {
			return err
		}
		return storage.SavePRHistoryCheckpoint(tx, next, checkpoint.Version)
	})
	return errors.Join(readErr, writeErr)
}

func validatePRHistoryPage(checkpoint domain.PRHistoryCheckpoint, page domain.GitHubPRHistoryPage, cursor string) error {
	if page.Validate() != nil || !page.Merged || page.RepositoryID != checkpoint.ProviderRepositoryID || page.HeadRepositoryID != checkpoint.ProviderRepositoryID || page.Number != checkpoint.PR.Number || page.BaseBranch != checkpoint.PR.BaseBranch || page.HeadBranch != checkpoint.PR.HeadBranch || page.MergeSHA != checkpoint.PR.MergeSHA || (page.HasNextPage && (page.EndCursor == cursor || len(page.Rewrites) == 0)) {
		return domain.ErrIntegrity
	}
	if checkpoint.PullRequestID != 0 && (page.PullRequestDatabaseID != checkpoint.PullRequestID || !page.MergedAt.Equal(checkpoint.MergedAt)) {
		return domain.ErrIntegrity
	}
	return nil
}

type preparedPRParticipations struct {
	revision       domain.RepositoryRevision
	participations []domain.PRParticipation
	history        []domain.HistoryEvent
	contributions  []domain.PRContribution
	memories       []domain.PRContributionMemory
}

func (g *GitHubWorker) preparePRParticipations(ctx context.Context, storage outbound.PRHistoryStore, checkpoint *domain.PRHistoryCheckpoint, fresh []domain.GitRefObservation) (preparedPRParticipations, error) {
	var prepared preparedPRParticipations
	revisions, ok := g.metadata.(outbound.RepositoryRevisions)
	if !ok {
		return prepared, domain.ErrNotFound
	}
	type inputs struct {
		revision     domain.RepositoryRevision
		history      []domain.HistoryEvent
		observations []domain.GitRefObservation
	}
	read, err := repositoryReadForRepoWithWork(ctx, g.work, checkpoint.RepoID, func(tx context.Context) (inputs, error) {
		var result inputs
		var err error
		result.revision, err = revisions.RepositoryRevision(tx, checkpoint.RepoID)
		if err == nil {
			result.history, err = g.ListHistory(tx, checkpoint.RepoID)
		}
		if err == nil {
			result.observations, err = storage.ListPRRefObservations(tx, checkpoint.RepoID, checkpoint.GitOrigin, checkpoint.ProviderRepositoryID, checkpoint.PR.Number, checkpoint.EvidenceCursor, 101)
		}
		return result, err
	})
	if err != nil {
		return prepared, err
	}
	prepared.revision = read.revision
	prepared.history = read.history
	verifier, err := domain.NewPRParticipationVerifier(*checkpoint, read.history)
	if err != nil {
		return prepared, err
	}
	byID := make(map[string]domain.GitRefObservation)
	for _, observation := range append(read.observations, fresh...) {
		if observation.ID > checkpoint.EvidenceCursor {
			byID[observation.ID] = observation
		}
	}
	observations := make([]domain.GitRefObservation, 0, len(byID))
	for _, observation := range byID {
		observations = append(observations, observation)
	}
	sort.Slice(observations, func(left, right int) bool { return observations[left].ID < observations[right].ID })
	publications := make(map[string][]domain.HistoryEvent)
	for _, event := range read.history {
		if event.Kind == "publish" {
			publications[event.GitAfter] = append(publications[event.GitAfter], event)
		}
	}
	matched := 0
	for _, observation := range observations[:min(100, len(observations))] {
		candidates := append([]domain.HistoryEvent{}, publications[observation.Before]...)
		if observation.Before != observation.After {
			candidates = append(candidates, publications[observation.After]...)
		}
		sort.Slice(candidates, func(left, right int) bool { return candidates[left].ID < candidates[right].ID })
		afterPublication := ""
		if strings.HasPrefix(checkpoint.MatchCursor, observation.ID+":") {
			afterPublication = strings.TrimPrefix(checkpoint.MatchCursor, observation.ID+":")
		}
		for _, publication := range candidates {
			if publication.ID <= afterPublication {
				continue
			}
			if matched >= 100 {
				checkpoint.NextAttempt = time.Now().UTC().Add(20 * time.Second)
				return prepared, nil
			}
			matched++
			checkpoint.MatchCursor = observation.ID + ":" + publication.ID
			participation, err := verifier.Verify(observation, publication)
			if errors.Is(err, domain.ErrPRSourcePending) || errors.Is(err, domain.ErrConflict) {
				continue
			}
			if err != nil {
				return prepared, err
			}
			prepared.participations = append(prepared.participations, participation)
		}
		checkpoint.EvidenceCursor = observation.ID
		checkpoint.MatchCursor = ""
	}
	if len(observations) > 100 {
		checkpoint.NextAttempt = time.Now().UTC().Add(20 * time.Second)
	} else {
		checkpoint.EvidenceCursor = ""
	}
	return prepared, nil
}
