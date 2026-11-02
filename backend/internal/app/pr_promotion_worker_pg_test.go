//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prPromotionPGFixture struct {
	service      *Service
	storage      *store.PostgresStore
	observer     *pgx.Conn
	repo         domain.ContentHash
	repository   domain.Repository
	organization string
	root         domain.ContentHash
	base         domain.ContentHash
	tip          domain.ContentHash
	proof        domain.HistoryEvent
	job          domain.PRPromotionJob
}

func newPRPromotionPGFixture(test *testing.T, ready, diverged bool) *prPromotionPGFixture {
	test.Helper()
	wave40IsolatedPG(test)
	service, storage, repo := collaborationPG(test)
	ctx := wave40PGContext(context.Background(), storage)
	team := makeTeamFixture(test, storage)
	config, err := pgxpool.ParseConfig(collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	observer, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observer.Close(cleanup); err != nil {
			test.Error(err)
		}
	})
	if _, err := observer.Exec(ctx, `UPDATE repositories SET webhook_url=$2 WHERE id=$1`, team.repository.ID, "https://example.test/pr-promotion"); err != nil {
		test.Fatal(err)
	}
	metadata, err := storage.GetRepo(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	metadata.RepositoryID = team.repository.ID
	if _, err := storage.PutRepo(ctx, metadata); err != nil {
		test.Fatal(err)
	}
	root := collaborationSnapshot(test, storage, repo, "shared root")
	base := root
	if diverged {
		base = collaborationSnapshot(test, storage, repo, "current main", root)
	}
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base}, ""); err != nil {
		test.Fatal(err)
	}
	if err := service.EnableContextProtocol(ctx, repo); err != nil {
		test.Fatal(err)
	}
	if _, err := service.PutMemoryDigest(ctx, repo, domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: base, Summary: "PWP MAIN MEMORY"})); err != nil {
		test.Fatal(err)
	}
	pr := domain.PullRequestMerge{Number: 71, BaseBranch: "main", HeadBranch: "feature/71", HeadSHA: fmt.Sprintf("%040x", 71), MergeSHA: fmt.Sprintf("%040x", 171)}
	tip := collaborationSnapshot(test, storage, repo, "feature-71", root)
	proof := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 71), RepoID: string(repo), BranchID: pr.HeadBranch, Branch: pr.HeadBranch, Kind: "birth", Source: root, Target: tip, GitAfter: pr.HeadSHA, CreatedAt: time.Now().UTC()}
	if err := service.RecordHistory(ctx, proof); err != nil {
		test.Fatal(err)
	}
	memory, err := service.PutMemoryDigest(ctx, repo, domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: tip, Summary: "PWP SOURCE MEMORY"}))
	if err != nil {
		test.Fatal(err)
	}
	if ready {
		publication := prPublication(proof)
		publication.MemoryHash = memory
		if err := service.RecordHistory(ctx, publication); err != nil {
			test.Fatal(err)
		}
	}
	job, err := service.SubmitPRPromotion(ctx, repo, pr)
	if err != nil {
		test.Fatal(err)
	}
	repository, err := storage.GetRepository(ctx, team.repository.ID)
	if err != nil {
		test.Fatal(err)
	}
	return &prPromotionPGFixture{service: service, storage: storage, observer: observer, repo: repo, repository: repository, organization: team.organization.ID, root: root, base: base, tip: tip, proof: proof, job: job}
}

func newPRPromotionPGOwner(test *testing.T, metadata outbound.MetadataStore, blobs outbound.BlobStore, repositories outbound.RepositoryStore) *PRPromotionWorker {
	test.Helper()
	worker, err := NewPRPromotionWorker(PRPromotionWorkerConfig{Profile: PRPromotionComplete, Metadata: metadata, Blobs: blobs, Engine: gitengine.NewEngine(metadata), Repositories: repositories, RequireCompleteMemory: true})
	if err != nil {
		test.Fatal(err)
	}
	return worker
}

type prPromotionPGPublication struct {
	refs          []domain.Ref
	history       []domain.HistoryEvent
	snapshots     []domain.Snapshot
	reflog        []domain.RefLogEntry
	revision      domain.RepositoryRevision
	notifications []domain.NotificationJob
	audit         []domain.OrganizationAuditEvent
	memory        domain.MemoryProjection
}

func (fixture *prPromotionPGFixture) publication(ctx context.Context, storage *store.PostgresStore) (prPromotionPGPublication, error) {
	var state prPromotionPGPublication
	err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		var err error
		if state.refs, err = storage.ListRefs(read, fixture.repo); err != nil {
			return err
		}
		if state.history, err = storage.ListHistoryEvents(read, fixture.repo); err != nil {
			return err
		}
		if state.snapshots, err = storage.ListSnapshots(read, fixture.repo, ""); err != nil {
			return err
		}
		if state.reflog, err = storage.ReadReflog(read, fixture.repo); err != nil {
			return err
		}
		if state.revision, err = storage.RepositoryRevision(read, fixture.repo); err != nil {
			return err
		}
		if state.notifications, err = storage.ListNotifications(read, fixture.repository.ID); err != nil {
			return err
		}
		if state.audit, err = storage.ListOrganizationAudit(read, fixture.organization, 500); err != nil {
			return err
		}
		for _, ref := range state.refs {
			if ref.Kind == domain.RefBranch && ref.Name == "main" {
				state.memory, err = ProjectMemory(read, storage, fixture.repo, ref.Target)
				return err
			}
		}
		return domain.ErrNotFound
	})
	return state, err
}

func prPromotionPGReceipts(history []domain.HistoryEvent, pr domain.PullRequestMerge) (int, int) {
	bindings, completions := 0, 0
	for _, event := range history {
		if event.Kind != "pr-merge" || event.PR == nil || *event.PR != pr {
			continue
		}
		if event.PRCompleted {
			completions++
		} else {
			bindings++
		}
	}
	return bindings, completions
}

type prPromotionPGTransactionStore struct {
	*store.PostgresStore
	failure   string
	injected  string
	staged    func(context.Context, domain.PRPromotionJob) error
	histories int
	appends   int
	documents int
	outbox    int
	completed int
}

func (storage *prPromotionPGTransactionStore) writeContext(ctx context.Context, requireAfterCommit bool) error {
	_, system := inbound.RepositoryActor(ctx)
	deadline, bounded := ctx.Deadline()
	if !storage.InTransaction(ctx) || storage.InReadOnlyTransaction(ctx) || (requireAfterCommit && ctx.Value(afterCommitKey{}) == nil) || !system || !bounded || time.Until(deadline) > 90*time.Second || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), ReadDocumentIdentities(storage, storage)) {
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *prPromotionPGTransactionStore) ApplyHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	if err := storage.writeContext(ctx, true); err != nil {
		return err
	}
	storage.histories++
	return storage.PostgresStore.ApplyHistoryEvent(ctx, event)
}

func (storage *prPromotionPGTransactionStore) AppendRef(ctx context.Context, repo domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	if err := storage.writeContext(ctx, true); err != nil {
		return err
	}
	storage.appends++
	return storage.PostgresStore.AppendRef(ctx, repo, ref, expected)
}

func (storage *prPromotionPGTransactionStore) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if err := storage.writeContext(ctx, true); err != nil {
		return domain.VerifiedDocReference{}, err
	}
	storage.documents++
	return storage.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
}

func (storage *prPromotionPGTransactionStore) EnqueueNotification(ctx context.Context, delivery outbound.NotificationDelivery) error {
	if err := storage.writeContext(ctx, true); err != nil {
		return err
	}
	storage.outbox++
	if err := storage.PostgresStore.EnqueueNotification(ctx, delivery); err != nil {
		return err
	}
	if storage.failure == "outbox" {
		storage.injected = "outbox"
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *prPromotionPGTransactionStore) AdvanceRepositoryRevision(ctx context.Context, repo domain.ContentHash, pending bool) error {
	if err := storage.writeContext(ctx, false); err != nil {
		return err
	}
	if err := storage.PostgresStore.AdvanceRepositoryRevision(ctx, repo, pending); err != nil {
		return err
	}
	if storage.failure == "revision" {
		storage.injected = "revision"
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *prPromotionPGTransactionStore) FinishPRJob(ctx context.Context, job domain.PRPromotionJob) error {
	if job.State != "completed" {
		return storage.PostgresStore.FinishPRJob(ctx, job)
	}
	if err := storage.writeContext(ctx, true); err != nil {
		return err
	}
	storage.completed++
	if err := storage.PostgresStore.FinishPRJob(ctx, job); err != nil {
		return err
	}
	if storage.staged != nil {
		if err := storage.staged(ctx, job); err != nil {
			return err
		}
	}
	if storage.failure == "finish" {
		storage.injected = "finish"
		return domain.ErrIntegrity
	}
	return nil
}

func TestPGPRPromotionWorkerDirectAtomicPublicationAndRollback(test *testing.T) {
	for _, failure := range []string{"finish", "revision", "outbox"} {
		test.Run(failure, func(test *testing.T) {
			fixture := newPRPromotionPGFixture(test, true, true)
			ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
			defer cancel()
			before, err := fixture.publication(ctx, fixture.storage)
			if err != nil {
				test.Fatal(err)
			}
			metadata := &prPromotionPGTransactionStore{PostgresStore: fixture.storage, failure: failure}
			owner := newPRPromotionPGOwner(test, metadata, metadata, metadata)
			staged, release := make(chan prPromotionPGPublication, 1), make(chan struct{})
			metadata.staged = func(write context.Context, job domain.PRPromotionJob) error {
				current, err := metadata.GetPRJob(write, fixture.repo, job.ID)
				if err != nil || current.State != "completed" || current.Version != job.Version {
					return errors.Join(domain.ErrIntegrity, err)
				}
				ref, err := metadata.GetRef(write, fixture.repo, domain.RefBranch, "main")
				if err != nil || ref.Target != fixture.tip || ref.BranchID != fixture.job.BaseBranchID {
					return errors.Join(domain.ErrIntegrity, err)
				}
				state, err := fixture.publication(write, fixture.storage)
				if err != nil {
					return err
				}
				bindings, completions := prPromotionPGReceipts(state.history, job.PR)
				if bindings != 1 || completions != 1 || len(state.notifications) != len(before.notifications)+1 || strings.Count(state.memory.Digest.Summary, "PWP MAIN MEMORY") != 1 || strings.Count(state.memory.Digest.Summary, "PWP SOURCE MEMORY") != 1 {
					return domain.ErrIntegrity
				}
				if failure == "finish" {
					staged <- state
					select {
					case <-release:
					case <-write.Done():
						return write.Err()
					}
				}
				return nil
			}
			finished, joined := make(chan error, 1), make(chan struct{})
			go func() { defer close(joined); finished <- owner.ProcessPRPromotions(ctx, 1) }()
			defer func() { cancel(); <-joined }()
			if failure == "finish" {
				select {
				case state := <-staged:
					for _, snapshot := range state.snapshots {
						if snapshot.ID == fixture.tip && (!slices.Equal(snapshot.Parents, []domain.ContentHash{fixture.root}) || !slices.Contains(snapshot.GraftParents, fixture.base)) {
							test.Fatal("staged append changed natural ancestry or omitted current main", snapshot)
						}
					}
				case <-ctx.Done():
					test.Fatal("direct owner never staged its actual completion", ctx.Err())
				}
				peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
				if err != nil {
					test.Fatal(err)
				}
				defer peer.Close()
				outside, err := fixture.publication(ctx, peer)
				if err != nil || !reflect.DeepEqual(outside, before) {
					test.Fatal("uncommitted refs, history, context, memory or outbox became visible", outside, err)
				}
				close(release)
			}
			err = <-finished
			<-joined
			if err != nil {
				test.Fatal(err)
			}
			after, err := fixture.publication(ctx, fixture.storage)
			if err != nil || !reflect.DeepEqual(after, before) {
				test.Fatal("failed publication escaped its outer transaction", after, err)
			}
			job, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID)
			if err != nil || job.State != "attention" || job.Reason != "integrity_check_failed" || job.Attempts != 1 || job.Version != fixture.job.Version+1 || !job.LeaseUntil.IsZero() {
				test.Fatal("rollback did not settle the original durable claim", job, err)
			}
			if metadata.injected != failure {
				test.Fatal("direct worker never reached the requested real rollback boundary", failure, metadata.injected)
			}
			metadata.failure, metadata.staged = "", nil
			if err := fixture.service.RetryPRPromotion(ctx, fixture.repo, job.ID); err != nil {
				test.Fatal(err)
			}
			if err := owner.ProcessPRPromotions(ctx, 1); err != nil {
				test.Fatal(err)
			}
			after, err = fixture.publication(ctx, fixture.storage)
			if err != nil {
				test.Fatal(err)
			}
			job, err = fixture.storage.GetPRJob(ctx, fixture.repo, job.ID)
			bindings, completions := prPromotionPGReceipts(after.history, fixture.job.PR)
			if err != nil || job.State != "completed" || bindings != 1 || completions != 1 || after.revision.Graph <= before.revision.Graph || len(after.notifications) != len(before.notifications)+1 || !reflect.DeepEqual(after.audit, before.audit) || metadata.histories < 2 || metadata.appends == 0 || metadata.documents == 0 || metadata.outbox == 0 || metadata.completed == 0 {
				test.Fatal("direct recovery omitted a real mutation boundary", job, after, err)
			}
			for _, text := range []string{"PWP MAIN MEMORY", "PWP SOURCE MEMORY"} {
				if strings.Count(after.memory.Digest.Summary, text) != 1 {
					test.Fatal("committed context did not project both immutable memories once", after.memory)
				}
			}
			assertResourcesReleased(test, processResources)
		})
	}
}

type prPromotionPGClaimStore struct {
	*store.PostgresStore
	claimAt     time.Time
	claimed     chan domain.PRPromotionJob
	release     chan struct{}
	staleFences int
}

func (storage *prPromotionPGClaimStore) ClaimPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.PRPromotionJob, error) {
	_, system := inbound.RepositoryActor(ctx)
	if !system || storage.InTransaction(ctx) || ctx.Value(resourcePermitKey{}) == nil || lease != prJobLease || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), ReadDocumentIdentities(storage, storage)) {
		return domain.PRPromotionJob{}, domain.ErrIntegrity
	}
	if !storage.claimAt.IsZero() {
		now = storage.claimAt
	}
	job, err := storage.PostgresStore.ClaimPRJob(ctx, repo, id, now, lease)
	if err == nil && storage.claimed != nil {
		storage.claimed <- job
		select {
		case <-storage.release:
		case <-ctx.Done():
			return domain.PRPromotionJob{}, ctx.Err()
		}
	}
	return job, err
}

func (storage *prPromotionPGClaimStore) FencePRJob(ctx context.Context, job domain.PRPromotionJob) error {
	err := storage.PostgresStore.FencePRJob(ctx, job)
	if errors.Is(err, domain.ErrRefConflict) {
		storage.staleFences++
	}
	return err
}

func TestPGPRPromotionWorkerDirectLateSourceRestartAndCompletedReplay(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, false, false)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	before, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	metadata := &prPromotionPGClaimStore{PostgresStore: fixture.storage}
	owner := newPRPromotionPGOwner(test, metadata, metadata, metadata)
	previous := fixture.job
	for attempt := 1; attempt <= prSourcePendingAttempts; attempt++ {
		if err := owner.ProcessPRPromotions(ctx, 1); err != nil {
			test.Fatal(err)
		}
		job, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID)
		state, reason := "waiting", "source_context_pending"
		if attempt == prSourcePendingAttempts {
			state, reason = "attention", "source_finalization_required"
		}
		if err != nil || job.State != state || job.Reason != reason || job.Version != previous.Version+1 || job.Attempts != attempt || !job.LeaseUntil.IsZero() || !job.NextAttempt.Equal(job.UpdatedAt.Add(time.Second<<min(attempt, 8))) {
			test.Fatal("source retry did not persist its exact state before clock advancement", previous, job, err)
		}
		previous = job
		metadata.claimAt = job.NextAttempt.Add(time.Nanosecond)
	}
	if after, err := fixture.publication(ctx, fixture.storage); err != nil || !reflect.DeepEqual(after, before) {
		test.Fatal("unpublished source froze history or changed branch memory", after, err)
	}
	publication := prPublication(fixture.proof)
	source, err := fixture.storage.GetSnapshot(ctx, fixture.repo, fixture.tip)
	if err != nil {
		test.Fatal(err)
	}
	publication.MemoryHash = source.MemoryHash
	if err := fixture.storage.ApplyHistoryEvent(ctx, publication); err != nil {
		test.Fatal(err)
	}
	if job, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID); err != nil || !reflect.DeepEqual(job, previous) {
		test.Fatal("fixture accidentally woke the job before restart", job, err)
	}
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	owner = newPRPromotionPGOwner(test, reopened, reopened, reopened)
	if err := owner.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	done, err := reopened.GetPRJob(ctx, fixture.repo, fixture.job.ID)
	if err != nil || done.State != "completed" || done.Version != previous.Version+2 || done.Attempts != 1 || !done.CreatedAt.Equal(fixture.job.CreatedAt) || done.BaseBranchID != fixture.job.BaseBranchID || done.PR != fixture.job.PR {
		test.Fatal("reconstructed direct owner lost publication wake or immutable identity", done, err)
	}
	move := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 72), RepoID: string(fixture.repo), BranchID: fixture.job.BaseBranchID, Branch: "main", Kind: "advance", Source: fixture.tip, Target: fixture.base, CreatedAt: time.Now().UTC()}
	if err := fixture.service.RecordHistory(ctx, move); err != nil {
		test.Fatal(err)
	}
	rewound, err := fixture.publication(ctx, reopened)
	if err != nil {
		test.Fatal(err)
	}
	replay := done
	replay.State, replay.Reason, replay.FailureClass = "waiting", "", ""
	replay.Version++
	replay.UpdatedAt, replay.NextAttempt, replay.LeaseUntil = time.Now().UTC(), time.Now().UTC(), time.Time{}
	raw, err := json.Marshal(replay)
	if err != nil {
		test.Fatal(err)
	}
	changed, err := fixture.observer.Exec(ctx, `UPDATE pr_promotion_jobs SET payload=$4,state=$5,version=$6,next_attempt=$7,lease_until=$8 WHERE repo_id=$1 AND id=$2 AND version=$3 AND state='completed'`, fixture.repo, done.ID, done.Version, raw, replay.State, replay.Version, replay.NextAttempt, replay.LeaseUntil)
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("could not install versioned completed-receipt replay", err)
	}
	owner = newPRPromotionPGOwner(test, reopened, reopened, reopened)
	if err := owner.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	after, err := fixture.publication(ctx, reopened)
	if err != nil {
		test.Fatal(err)
	}
	if after.revision.Graph <= rewound.revision.Graph {
		test.Fatal("actual replay did not traverse the existing write boundary")
	}
	after.revision = rewound.revision
	if !reflect.DeepEqual(after, rewound) {
		test.Fatal("completed receipt replay undid a later rewind or duplicated immutable effects", after)
	}
	current, err := reopened.GetPRJob(ctx, fixture.repo, done.ID)
	if err != nil || current.State != "completed" || current.Version != replay.Version+1 {
		test.Fatal("direct owner did not actually process the replay", current, err)
	}
}

func TestPGPRPromotionWorkerDirectQueuedBaseIdentityRenameReuse(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, false)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	owner := newPRPromotionPGOwner(test, fixture.storage, fixture.storage, fixture.storage)
	unrelated := collaborationSnapshot(test, fixture.storage, fixture.repo, "new main identity", fixture.root)
	rename := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 73), RepoID: string(fixture.repo), BranchID: fixture.job.BaseBranchID, Branch: "trunk", PreviousBranch: "main", Kind: "rename", Source: fixture.base, Target: fixture.base, CreatedAt: time.Now().UTC()}
	reused := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 74), RepoID: string(fixture.repo), BranchID: "replacement-main", Branch: "main", Kind: "birth", BindingParent: rename.ID, Source: fixture.base, Target: unrelated, CreatedAt: time.Now().UTC()}
	for _, event := range []domain.HistoryEvent{rename, reused} {
		if err := fixture.service.RecordHistory(ctx, event); err != nil {
			test.Fatal(err)
		}
	}
	replay, err := fixture.service.SubmitPRPromotion(ctx, fixture.repo, fixture.job.PR)
	if err != nil || replay.BaseBranchID != fixture.job.BaseBranchID {
		test.Fatal("queued base identity followed a reused name", replay, err)
	}
	if err := owner.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	for name, target := range map[string]domain.ContentHash{"trunk": fixture.tip, "main": unrelated} {
		ref, err := fixture.storage.GetRef(ctx, fixture.repo, domain.RefBranch, name)
		if err != nil || ref.Target != target || name == "trunk" && ref.BranchID != fixture.job.BaseBranchID || name == "main" && ref.BranchID != reused.BranchID {
			test.Fatal("direct worker crossed the queued base identity", name, ref, err)
		}
	}
	job, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID)
	if err != nil || job.State != "completed" || job.BaseBranchID != fixture.job.BaseBranchID {
		test.Fatal("rename recovery did not finish its original queue entry", job, err)
	}
	history, err := fixture.storage.ListHistoryEvents(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	bindings, completions := prPromotionPGReceipts(history, fixture.job.PR)
	if bindings != 1 || completions != 1 {
		test.Fatal("rename lost immutable binding or completion", bindings, completions)
	}
}

func TestPGPRPromotionWorkerDirectCurrentOriginAndArchive(test *testing.T) {
	for _, mutation := range []string{"origin", "archive"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newPRPromotionPGFixture(test, true, false)
			ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
			defer cancel()
			owner := newPRPromotionPGOwner(test, fixture.storage, fixture.storage, fixture.storage)
			before, err := fixture.publication(ctx, fixture.storage)
			if err != nil {
				test.Fatal(err)
			}
			query, identity := `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, string(fixture.repo)
			var value any = "https://github.com/acme/rebound.git"
			reason := "identity_or_history_conflict"
			if mutation == "archive" {
				query, identity, value, reason = `UPDATE repositories SET archived=$2 WHERE id=$1`, fixture.repository.ID, true, "policy_changed"
			}
			changed, err := fixture.observer.Exec(ctx, query, identity, value)
			if err != nil || changed.RowsAffected() != 1 {
				test.Fatal("authority mutation did not affect the queued repository", err)
			}
			if err := owner.ProcessPRPromotions(ctx, 1); err != nil {
				test.Fatal(err)
			}
			job, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID)
			if err != nil || job.State != "attention" || job.Reason != reason || job.Attempts != 1 || job.Version != fixture.job.Version+1 || !job.LeaseUntil.IsZero() {
				test.Fatal("direct owner did not recheck current authority", job, err)
			}
			after, err := fixture.publication(ctx, fixture.storage)
			if err != nil || !reflect.DeepEqual(after, before) {
				test.Fatal("changed authority published PR effects", after, err)
			}
		})
	}
}

func expirePRPromotionPGClaim(test *testing.T, ctx context.Context, fixture *prPromotionPGFixture, job domain.PRPromotionJob) {
	test.Helper()
	expired := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	changed, err := fixture.observer.Exec(ctx, `UPDATE pr_promotion_jobs SET lease_until=$4,next_attempt=$4,payload=jsonb_set(jsonb_set(payload,'{lease_until}',to_jsonb($4::timestamptz)),'{next_attempt}',to_jsonb($4::timestamptz)) WHERE repo_id=$1 AND id=$2 AND version=$3 AND state='running'`, job.RepoID, job.ID, job.Version, expired)
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("could not expire both persisted claim eligibility bounds", job, err)
	}
	current, err := fixture.storage.GetPRJob(ctx, job.RepoID, job.ID)
	if err != nil || current.State != "running" || current.Version != job.Version || current.Attempts != job.Attempts || current.Reason != job.Reason || !current.LeaseUntil.Equal(expired) || !current.NextAttempt.Equal(expired) {
		test.Fatal("expiry fixture changed state or left one eligibility bound live", current, err)
	}
}

func TestPGPRPromotionWorkerDirectSharedAdmissionLeaseReplacementAndRecovery(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, false)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	metadata := &prPromotionPGClaimStore{PostgresStore: fixture.storage, claimed: make(chan domain.PRPromotionJob, 1), release: make(chan struct{})}
	owner := newPRPromotionPGOwner(test, metadata, metadata, metadata)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	other := newPRPromotionPGOwner(test, peer, peer, peer)
	before, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	finished, joined := make(chan error, 1), make(chan struct{})
	spoofed := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(ctx, "untrusted-caller"), []domain.DocumentIdentity{"untrusted-format"})
	go func() { defer close(joined); finished <- owner.ProcessPRPromotions(spoofed, 1) }()
	defer func() { cancel(); <-joined }()
	var old domain.PRPromotionJob
	select {
	case old = <-metadata.claimed:
	case <-ctx.Done():
		test.Fatal("actual worker never durably claimed its source", ctx.Err())
	}
	if old.State != "running" || old.Version != fixture.job.Version+1 || old.Attempts != 1 || !old.LeaseUntil.After(time.Now()) {
		test.Fatal("worker did not expose its original live claim", old)
	}
	if err := other.ProcessPRPromotions(ctx, 1); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("peer bypassed the original process-wide one-slot bulk gate", err)
	}
	if _, err := peer.ClaimPRJob(ctx, fixture.repo, old.ID, time.Now().UTC(), prJobLease); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("independent SQL claimant bypassed a live lease", err)
	}
	expirePRPromotionPGClaim(test, ctx, fixture, old)
	replacement, err := peer.ClaimPRJob(ctx, fixture.repo, old.ID, time.Now().UTC(), prJobLease)
	if err != nil || replacement.State != "running" || replacement.Version != old.Version+1 || replacement.Attempts != old.Attempts+1 || replacement.Reason != old.Reason || !replacement.LeaseUntil.After(time.Now()) {
		test.Fatal("SQL recovery did not install a versioned replacement", replacement, err)
	}
	for _, state := range []string{"completed", "retrying"} {
		late := old
		late.State, late.UpdatedAt, late.LeaseUntil = state, time.Now().UTC(), time.Time{}
		if state == "retrying" {
			late.Reason = "temporary_failure"
		}
		if err := fixture.storage.FinishPRJob(ctx, late); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("late success or detached failure overwrote the newer claim", state, err)
		}
	}
	close(metadata.release)
	if err := <-finished; err != nil {
		test.Fatal(err)
	}
	<-joined
	if current, err := peer.GetPRJob(ctx, fixture.repo, old.ID); err != nil || !reflect.DeepEqual(current, replacement) || metadata.staleFences != 1 {
		test.Fatal("actual stale worker or its detached cleanup changed the replacement", current, metadata.staleFences, err)
	}
	if after, err := fixture.publication(ctx, peer); err != nil || !reflect.DeepEqual(after, before) {
		test.Fatal("stale execution changed refs, history, memory, revision or outbox", after, err)
	}
	assertResourcesReleased(test, processResources)
	expirePRPromotionPGClaim(test, ctx, fixture, replacement)
	if err := other.ProcessPRPromotions(ctx, 1); err != nil {
		test.Fatal(err)
	}
	done, err := peer.GetPRJob(ctx, fixture.repo, old.ID)
	if err != nil || done.State != "completed" || done.Version != replacement.Version+1 || done.Attempts != replacement.Attempts+1 || !done.LeaseUntil.IsZero() {
		test.Fatal("direct peer could not recover after the original admission released", done, err)
	}
	ref, err := peer.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
	if err != nil || ref.Target != fixture.tip {
		test.Fatal("recovered worker never published the actual source", ref, err)
	}
	history, err := peer.ListHistoryEvents(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	bindings, completions := prPromotionPGReceipts(history, fixture.job.PR)
	if bindings != 1 || completions != 1 {
		test.Fatal("lease recovery duplicated or omitted immutable receipts", bindings, completions)
	}
	for range 2 {
		other = newPRPromotionPGOwner(test, peer, peer, peer)
		if err := other.ProcessPRPromotions(ctx, 1); err != nil {
			test.Fatal(err)
		}
	}
	if current, err := peer.GetPRJob(ctx, fixture.repo, old.ID); err != nil || !reflect.DeepEqual(current, done) {
		test.Fatal("reconstructed worker reclaimed terminal work", current, err)
	}
	assertResourcesReleased(test, processResources)
}
