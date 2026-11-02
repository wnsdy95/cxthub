package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphProjectionFailure struct {
	job    domain.GraphProjectionLease
	reason string
	retry  bool
}

type graphProjectionWorkerStore struct {
	outbound.MetadataStore
	outbound.GraphProjectionStore
	metadata   domain.Repo
	jobs       []domain.GraphProjectionLease
	pages      []domain.RepositoryViewSourcePage
	inputs     []domain.RepositoryViewSourceEntry
	configured []domain.ContentHash
	requests   []domain.RepositoryViewSourceRequest
	saved      []domain.GraphProjectionLease
	published  []domain.GraphProjectionPart
	failures   []graphProjectionFailure
	calls      map[string]int
	checkpoint domain.RepositoryViewSourceCheckpoint
	proof      []domain.RepositoryViewSourceEntry
	step       func(context.Context, string) error
}

func (storage *graphProjectionWorkerStore) visit(ctx context.Context, step string) error {
	storage.calls[step]++
	if storage.step != nil {
		if err := storage.step(ctx, step); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (storage *graphProjectionWorkerStore) ConfigureGraphProjection(ctx context.Context, repo domain.ContentHash, enabled bool) error {
	if !enabled {
		return domain.ErrValidation
	}
	if err := storage.visit(ctx, "configure"); err != nil {
		return err
	}
	storage.configured = append(storage.configured, repo)
	return nil
}

func (storage *graphProjectionWorkerStore) ClaimGraphProjection(ctx context.Context, duration time.Duration) (domain.GraphProjectionLease, error) {
	if duration != time.Minute {
		return domain.GraphProjectionLease{}, domain.ErrValidation
	}
	if err := storage.visit(ctx, "claim"); err != nil {
		return domain.GraphProjectionLease{}, err
	}
	if len(storage.jobs) == 0 {
		return domain.GraphProjectionLease{}, domain.ErrNotFound
	}
	job := storage.jobs[0]
	storage.jobs = storage.jobs[1:]
	return job, nil
}

func (storage *graphProjectionWorkerStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if err := storage.visit(ctx, "identity"); err != nil {
		return domain.Repo{}, err
	}
	if repo != storage.metadata.ID {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return storage.metadata, nil
}

func (storage *graphProjectionWorkerStore) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	if err := storage.visit(ctx, "source"); err != nil {
		return domain.RepositoryViewSourcePage{}, err
	}
	if repo != storage.metadata.ID || request.Validate() != nil {
		return domain.RepositoryViewSourcePage{}, domain.ErrIntegrity
	}
	storage.requests = append(storage.requests, request)
	if len(storage.pages) == 0 {
		return domain.RepositoryViewSourcePage{}, domain.ErrNotFound
	}
	page := storage.pages[0]
	storage.pages = storage.pages[1:]
	return page, nil
}

func (storage *graphProjectionWorkerStore) SaveGraphProjectionPage(ctx context.Context, job domain.GraphProjectionLease, page domain.RepositoryViewSourcePage) error {
	if err := storage.visit(ctx, "save"); err != nil {
		return err
	}
	storage.saved = append(storage.saved, job)
	return nil
}

func (storage *graphProjectionWorkerStore) LoadGraphProjectionInputs(ctx context.Context, job domain.GraphProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
	if err := storage.visit(ctx, "load"); err != nil {
		return nil, err
	}
	return storage.inputs, nil
}

func (storage *graphProjectionWorkerStore) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, metadata domain.Repo, proof []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	if err := storage.visit(ctx, "factory"); err != nil {
		return nil, err
	}
	storage.checkpoint, storage.proof = checkpoint, proof
	return store.NewGraphSourceReader(ctx, checkpoint, metadata, proof)
}

func (storage *graphProjectionWorkerStore) PublishGraphProjection(ctx context.Context, job domain.GraphProjectionLease, parts []domain.GraphProjectionPart) error {
	if err := storage.visit(ctx, "publish"); err != nil {
		return err
	}
	storage.published = parts
	return nil
}

func (storage *graphProjectionWorkerStore) FailGraphProjection(ctx context.Context, job domain.GraphProjectionLease, reason string, retry bool) error {
	storage.failures = append(storage.failures, graphProjectionFailure{job, reason, retry})
	return storage.visit(ctx, "fail")
}

func graphProjectionWorkerFixture(t *testing.T) (*Service, *graphProjectionWorkerStore, domain.GraphProjectionLease, domain.RepositoryViewSourceCheckpoint) {
	t.Helper()
	repo := domain.HashContent([]byte(t.Name()))
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 7}
	job := domain.GraphProjectionLease{RepoID: repo, Dataset: "22222222-2222-4222-8222-222222222222", Epoch: checkpoint.Epoch, Fence: 3, Phase: "project", AppliedSequence: -1, InputSequence: checkpoint.Sequence, LeaseUntil: time.Now().Add(time.Minute)}
	metadata := domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/graph.git", ContextProtocol: 1}
	marshal := func(value any) json.RawMessage {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	snapshot := domain.Snapshot{ID: domain.HashContent([]byte("snapshot")), RepoID: repo, DocHash: domain.HashContent([]byte("snapshot")), Provider: domain.ProviderCodex, SessionID: "session", CreatedAt: time.Unix(123, 0).UTC()}
	tree, err := domain.NewGitTreeNode(map[string]domain.GitEntry{}, 40)
	if err != nil {
		t.Fatal(err)
	}
	storage := &graphProjectionWorkerStore{metadata: metadata, jobs: []domain.GraphProjectionLease{job}, calls: map[string]int{}}
	storage.inputs = []domain.RepositoryViewSourceEntry{
		{Sequence: 2, Kind: "git_tree", Key: string(marshal([]string{metadata.GitRemoteURL, tree.OID})), Value: marshal(map[string]any{"repo_id": repo, "origin": metadata.GitRemoteURL, "oid": tree.OID, "payload": tree})},
		{Sequence: 1, Kind: "pending", Key: "session", Value: marshal(domain.Pending{RepoID: repo, SessionID: "session", Target: snapshot.ID, Branch: "main"})},
		{Sequence: 1, Kind: "protocol", Key: string(repo), Value: marshal(map[string]any{"context_protocol": 1})},
		{Sequence: 1, Kind: "repository", Key: string(repo), Value: marshal(struct {
			domain.Repo
			RemoteURLSupported    bool `json:"remote_url_supported"`
			GitRemoteURLSupported bool `json:"git_remote_url_supported"`
		}{metadata, true, true})},
		{Sequence: 4, Kind: "revision", Key: string(repo), Value: marshal(domain.RepositoryRevision{Graph: 4, Pending: 3})},
		{Sequence: 1, Kind: "snapshot", Key: string(snapshot.ID), Value: marshal(snapshot)},
	}
	service := &Service{meta: storage}
	return service, storage, job, checkpoint
}

func TestGraphProjectionWorkerCapabilitiesAndConfiguration(t *testing.T) {
	service, storage, job, _ := graphProjectionWorkerFixture(t)
	for _, incomplete := range []outbound.MetadataStore{
		struct{ outbound.MetadataStore }{},
		struct {
			outbound.MetadataStore
			outbound.GraphProjectionStore
		}{GraphProjectionStore: storage},
		struct {
			outbound.MetadataStore
			outbound.GraphProjectionStore
			outbound.RepositoryGraphSourceStore
		}{GraphProjectionStore: storage, RepositoryGraphSourceStore: storage},
		struct {
			outbound.MetadataStore
			outbound.GraphProjectionStore
			outbound.GraphSourceReaderFactory
		}{GraphProjectionStore: storage, GraphSourceReaderFactory: storage},
	} {
		unsupported := &Service{meta: incomplete}
		if err := unsupported.ConfigureGraphProjections(context.Background(), []domain.ContentHash{job.RepoID}); !errors.Is(err, domain.ErrCatalogUnsupported) || len(storage.configured) != 0 {
			t.Fatal("incomplete capability accepted", err)
		}
		if err := unsupported.ConfigureGraphProjections(context.Background(), nil); err != nil {
			t.Fatal("disabled worker required graph capabilities", err)
		}
		unsupported.RunGraphProjectionWorker(context.Background())
	}
	if err := service.ConfigureGraphProjections(context.Background(), []domain.ContentHash{job.RepoID, "invalid"}); err == nil || len(storage.configured) != 0 {
		t.Fatal("configuration wrote before validating all repos", err)
	}
	if err := service.ConfigureGraphProjections(context.Background(), []domain.ContentHash{job.RepoID, job.RepoID}); err != nil || !reflect.DeepEqual(storage.configured, []domain.ContentHash{job.RepoID}) || !service.graphProjectionRepos[job.RepoID] {
		t.Fatal("worker rollout was not persisted", err)
	}
	storage.step = func(_ context.Context, step string) error {
		if step == "configure" {
			return domain.ErrForbidden
		}
		return nil
	}
	if err := service.ConfigureGraphProjections(context.Background(), []domain.ContentHash{hh("other")}); !errors.Is(err, domain.ErrForbidden) || !service.graphProjectionRepos[job.RepoID] || len(service.graphProjectionRepos) != 1 {
		t.Fatal("failed configuration replaced selection", err)
	}
	if err := service.ConfigureGraphProjections(context.Background(), nil); err != nil || len(service.graphProjectionRepos) != 0 {
		t.Fatal("empty selection retained worker", err)
	}
	service.RunGraphProjectionWorker(context.Background())
	if storage.calls["claim"] != 0 || len(service.pendingProjectionRepos) != 0 || service.graphCache.Stats().Entries != 0 {
		t.Fatal("configuration started work or changed serving state", storage.calls)
	}
}

func TestGraphProjectionWorkerCapturesOnePagePerLeaseThenPublishesFrozenGeneration(t *testing.T) {
	service, storage, project, checkpoint := graphProjectionWorkerFixture(t)
	first := project
	first.Phase, first.InputSequence = "capture", -1
	second := first
	second.Cursor, second.Fence = "next", first.Fence+1
	project.Fence = second.Fence + 1
	storage.jobs = []domain.GraphProjectionLease{first, second, project}
	storage.pages = []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", storage.inputs[:2], "next"), graphCapturePage(checkpoint, "baseline", storage.inputs[2:], "")}
	original, err := json.Marshal(storage.inputs)
	if err != nil {
		t.Fatal(err)
	}
	storage.metadata.DefaultBranch = "live-must-not-project"
	for index := 1; index <= 2; index++ {
		if err := service.ProcessGraphProjections(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if len(storage.saved) != index || storage.calls["source"] != index || storage.calls["load"] != 0 || storage.calls["factory"] != 0 || storage.calls["publish"] != 0 {
			t.Fatal("capture projected or drained multiple pages", storage.calls)
		}
	}
	for index, request := range storage.requests {
		if request.Limit != domain.DefaultRepositoryViewSourceLimit || request.After != nil || index == 1 && request.Cursor != "next" {
			t.Fatal("capture request lost durable cursor or bound", request)
		}
	}
	restarted := &Service{meta: storage}
	if err := restarted.ProcessGraphProjections(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	got, err := domain.DecodeGraphViewProjection(context.Background(), project.RepoID, storage.published)
	if err != nil || got.Graph == nil || got.Revision.Graph != 4 || got.Graph.PositionEvent != "" || len(got.Graph.SnapshotIDs) != 1 || got.Graph.HoldCounts["main"] != 1 || storage.checkpoint != checkpoint || len(storage.proof) != 1 || storage.proof[0].Kind != "git_tree" {
		t.Fatalf("published generation is not captured graph: %+v, %v, checkpoint=%+v", got, err, storage.checkpoint)
	}
	captured := CapturedGraphSource{checkpoint: checkpoint, entries: storage.inputs}
	metadata, view, evidence, err := captured.Inputs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.NewGraphSourceReader(context.Background(), checkpoint, metadata, evidence)
	if err != nil {
		t.Fatal(err)
	}
	want, err := ProjectCapturedRepositoryView(context.Background(), checkpoint, view, reader)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("durable graph differs from captured projection: %v\ngot=%+v\nwant=%+v", err, got, want)
	}
	after, err := json.Marshal(storage.inputs)
	if err != nil || string(original) != string(after) || storage.calls["identity"] != 3 || restarted.graphCache.Stats().Entries != 0 || storage.calls["publish"] != 1 {
		t.Fatal("worker mutated inputs, filled serving cache, or reread live metadata", err, storage.calls)
	}
}

func TestGraphProjectionWorkerDeltaUsesGraphCheckpoint(t *testing.T) {
	service, storage, job, checkpoint := graphProjectionWorkerFixture(t)
	job.Phase, job.AppliedSequence, job.InputSequence = "capture", 3, -1
	storage.jobs = []domain.GraphProjectionLease{job}
	storage.pages = []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "delta", nil, "")}
	if err := service.ProcessGraphProjections(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	want := checkpoint
	want.Sequence = job.AppliedSequence
	if len(storage.requests) != 1 || !reflect.DeepEqual(storage.requests[0].After, &want) || len(storage.saved) != 1 || storage.calls["publish"] != 0 {
		t.Fatal("delta reused pending source or projected in capture lease", storage.requests)
	}
}

func TestGraphProjectionWorkerRejectsWrongScopeAndIncompletePages(t *testing.T) {
	for _, name := range []string{"scope", "epoch", "repo", "mode", "missing-checkpoint", "wrong-checkpoint", "empty-nonfinal", "both-cursors", "repeated-cursor", "oversized-page"} {
		t.Run(name, func(t *testing.T) {
			service, storage, job, checkpoint := graphProjectionWorkerFixture(t)
			job.Phase, job.InputSequence = "capture", -1
			page := graphCapturePage(checkpoint, "baseline", storage.inputs, "")
			wantErr, reason := domain.ErrIntegrity, "invalid_source"
			switch name {
			case "scope":
				page.Scope = domain.RepositoryViewSourceScope
				wantErr, reason = domain.ErrCatalogResetRequired, "source_reset"
			case "epoch":
				page.Epoch = "33333333-3333-4333-8333-333333333333"
				wantErr, reason = domain.ErrCatalogResetRequired, "source_reset"
			case "repo":
				page.RepoID = hh("wrong repo")
			case "mode":
				page.Mode = "delta"
			case "missing-checkpoint":
				page.Checkpoint = nil
			case "wrong-checkpoint":
				page.Checkpoint.Scope = domain.RepositoryViewSourceScope
			case "empty-nonfinal":
				page.Entries, page.NextCursor, page.Checkpoint = nil, "next", nil
			case "both-cursors":
				page.NextCursor = "next"
			case "repeated-cursor":
				job.Cursor, page.NextCursor, page.Checkpoint = "next", "next", nil
			case "oversized-page":
				page.Entries = make([]domain.RepositoryViewSourceEntry, domain.DefaultRepositoryViewSourceLimit+1)
				wantErr, reason = domain.ErrRepositoryViewSourceLimit, "resource_limit"
			}
			storage.jobs, storage.pages = []domain.GraphProjectionLease{job}, []domain.RepositoryViewSourcePage{page}
			err := service.ProcessGraphProjections(context.Background(), 1)
			if !errors.Is(err, wantErr) || storage.calls["save"] != 0 || storage.calls["publish"] != 0 || len(storage.failures) != 1 || storage.failures[0].reason != reason {
				t.Fatal("invalid capture advanced durable state", err, storage.calls, storage.failures)
			}
		})
	}
}

func TestGraphProjectionWorkerRejectsIncompleteInputsBeforePublishing(t *testing.T) {
	for _, name := range []string{"unfinished-cursor", "no-generation", "already-applied", "missing-metadata", "future-metadata", "future-evidence", "tombstone", "unknown-kind", "invalid-proof", "duplicate-metadata", "oversized-image"} {
		t.Run(name, func(t *testing.T) {
			service, storage, job, _ := graphProjectionWorkerFixture(t)
			switch name {
			case "unfinished-cursor":
				job.Cursor = "unfinished"
			case "no-generation":
				job.InputSequence = -1
			case "already-applied":
				job.AppliedSequence = job.InputSequence
			case "missing-metadata":
				storage.inputs = storage.inputs[:1]
			case "future-metadata":
				storage.inputs[1].Sequence = job.InputSequence + 1
			case "future-evidence":
				storage.inputs[0].Sequence = job.InputSequence + 1
			case "tombstone":
				storage.inputs[1].Deleted = true
			case "unknown-kind":
				storage.inputs[0].Kind = "unknown"
			case "invalid-proof":
				storage.inputs[0].Value = json.RawMessage(`{}`)
			case "duplicate-metadata":
				storage.inputs = append(storage.inputs, storage.inputs[3])
			case "oversized-image":
				storage.inputs[0].Value = json.RawMessage(strings.Repeat("x", domain.MaxRepositoryViewSourceImageBytes+1))
			}
			storage.jobs = []domain.GraphProjectionLease{job}
			err := service.ProcessGraphProjections(context.Background(), 1)
			if err == nil || storage.calls["publish"] != 0 || len(storage.failures) != 1 || storage.failures[0].retry {
				t.Fatal("incomplete generation published or retried as temporary", err, storage.calls, storage.failures)
			}
		})
	}
}

func TestGraphProjectionWorkerFailureClassificationAndFence(t *testing.T) {
	for _, scenario := range []struct {
		err    error
		reason string
		retry  bool
	}{
		{errors.New("temporary"), "temporary_failure", true},
		{context.Canceled, "temporary_failure", true},
		{domain.ErrCatalogResetRequired, "source_reset", true},
		{domain.ErrConflict, "lease_lost", true},
		{domain.ErrRefConflict, "lease_lost", true},
		{domain.ErrRepositoryViewSourceLimit, "resource_limit", false},
		{domain.ErrIntegrity, "invalid_source", false},
		{domain.ErrValidation, "invalid_source", false},
		{domain.ErrNotFound, "invalid_source", false},
		{domain.ErrForbidden, "access_denied", false},
		{domain.ErrDocumentIdentityUpgradeRequired, "identity_upgrade_required", true},
	} {
		t.Run(scenario.err.Error(), func(t *testing.T) {
			service, storage, job, _ := graphProjectionWorkerFixture(t)
			finishErr := errors.New("failure fence refused")
			storage.step = func(ctx context.Context, step string) error {
				if step == "load" {
					return scenario.err
				}
				if step == "fail" {
					deadline, bounded := ctx.Deadline()
					if !bounded || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
						t.Fatal("failure write not independently bounded", ctx.Err())
					}
					return finishErr
				}
				return nil
			}
			err := service.ProcessGraphProjections(context.Background(), 1)
			want := graphProjectionFailure{job, scenario.reason, scenario.retry}
			if !errors.Is(err, scenario.err) || !errors.Is(err, finishErr) || !reflect.DeepEqual(storage.failures, []graphProjectionFailure{want}) || storage.calls["publish"] != 0 {
				t.Fatal("failure lost classification, cause, or original fence", err, storage.failures)
			}
		})
	}
}

func TestGraphProjectionWorkerPreservesOldPublicationOnFailure(t *testing.T) {
	for _, stage := range []string{"identity", "load", "factory", "publish"} {
		t.Run(stage, func(t *testing.T) {
			service, storage, _, _ := graphProjectionWorkerFixture(t)
			old := []domain.GraphProjectionPart{{Kind: "old", Key: "generation", Value: json.RawMessage(`{}`)}}
			storage.published = old
			storage.step = func(_ context.Context, step string) error {
				if step == stage {
					return domain.ErrConflict
				}
				return nil
			}
			if err := service.ProcessGraphProjections(context.Background(), 1); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(storage.published, old) || len(storage.failures) != 1 {
				t.Fatal("failure replaced old generation or fell back live", err, storage.calls)
			}
		})
	}
}

func TestGraphProjectionWorkerCancellationDoesNotFailLease(t *testing.T) {
	for _, stage := range []string{"claim", "identity", "source", "save", "load", "factory", "publish"} {
		t.Run(stage, func(t *testing.T) {
			service, storage, job, checkpoint := graphProjectionWorkerFixture(t)
			ctx, gate := admissionTestContext(time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if stage == "source" || stage == "save" {
				job.Phase, job.InputSequence = "capture", -1
				storage.jobs = []domain.GraphProjectionLease{job}
				storage.pages = []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", storage.inputs, "")}
			}
			storage.step = func(_ context.Context, step string) error {
				if step == stage {
					cancel()
				}
				return nil
			}
			if err := service.ProcessGraphProjections(ctx, 1); !errors.Is(err, context.Canceled) || len(storage.failures) != 0 || len(storage.published) != 0 {
				t.Fatal("shutdown failed or published a lease", err, storage.failures)
			}
			assertResourcesReleased(t, gate)
		})
	}
}

func TestGraphProjectionWorkerContextAndProcessingLimits(t *testing.T) {
	service, storage, job, checkpoint := graphProjectionWorkerFixture(t)
	ctx, gate := admissionTestContext(time.Second)
	storage.step = func(bound context.Context, step string) error {
		if _, system := inbound.RepositoryActor(bound); !system || !reflect.DeepEqual(inbound.DocumentIdentities(bound), service.DocumentIdentitiesSupported()) {
			t.Fatal("worker identity or actor was lost", step)
		}
		for _, identity := range service.DocumentIdentitiesSupported() {
			if err := outbound.CheckDocumentIdentityCompatibility(bound, identity); err != nil {
				t.Fatal("worker compatibility was lost", step, err)
			}
		}
		deadline, bounded := bound.Deadline()
		if !bounded || time.Until(deadline) > 30*time.Second {
			t.Fatal("unbounded worker operation", step)
		}
		permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
		if permit == nil || !permit.live.Load() || permit.class != ResourceBulk {
			t.Fatal("operation outside bulk admission", step)
		}
		return nil
	}
	for _, limit := range []int{-1, 0, 129} {
		if err := service.ProcessGraphProjections(ctx, limit); !errors.Is(err, domain.ErrValidation) || storage.calls["claim"] != 0 {
			t.Fatal("invalid batch limit accepted", limit, err)
		}
	}
	capture := job
	capture.Phase, capture.InputSequence = "capture", -1
	storage.jobs = []domain.GraphProjectionLease{capture, job}
	storage.pages = []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", storage.inputs, "")}
	if err := service.ProcessGraphProjections(ctx, 128); err != nil || storage.calls["claim"] != 3 || storage.calls["publish"] != 1 {
		t.Fatal("worker did not stop at empty queue", err, storage.calls)
	}
	assertResourcesReleased(t, gate)
}

func TestGraphProjectionWorkerRechecksRepositoryIdentityBeforeInputs(t *testing.T) {
	service, storage, job, _ := graphProjectionWorkerFixture(t)
	storage.metadata.RequiredDocIdentity = domain.DocumentIdentityRootV1
	ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := service.ProcessGraphProjections(ctx, 1); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("caller declaration bypassed worker identity capabilities", err)
	}
	want := graphProjectionFailure{job, "identity_upgrade_required", true}
	if storage.calls["identity"] != 1 || storage.calls["load"] != 0 || storage.calls["factory"] != 0 || storage.calls["publish"] != 0 || !reflect.DeepEqual(storage.failures, []graphProjectionFailure{want}) {
		t.Fatal("identity fence did not precede captured inputs", storage.calls, storage.failures)
	}
}
