//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type progressObservation struct {
	Operation      string             `json:"operation"`
	Repo           string             `json:"repo"`
	ElapsedMS      float64            `json:"elapsed_ms"`
	QueueMS        float64            `json:"granted_queue_ms"`
	ReadTxMS       float64            `json:"owned_read_transaction_ms"`
	WriteTxMS      float64            `json:"owned_write_transaction_ms"`
	Outcome        string             `json:"outcome"`
	Detail         string             `json:"detail,omitempty"`
	StartMS        float64            `json:"start_ms"`
	EndMS          float64            `json:"end_ms"`
	Claims         []progressClaim    `json:"claims,omitempty"`
	Phases         map[string]float64 `json:"phases_ms,omitempty"`
	RootHeapBytes  int64              `json:"prepared_root_heap_bytes,omitempty"`
	RootSpoolBytes int64              `json:"prepared_root_spool_bytes,omitempty"`
	ResumeWaitMS   float64            `json:"root_resume_wait_ms,omitempty"`
	permits        map[*resourcePermit]bool
	origin         time.Time
}

type progressClaim struct {
	Repo    domain.ContentHash `json:"repo"`
	Phase   string             `json:"phase"`
	StartMS float64            `json:"start_ms"`
}

type progressObservationKey struct{}

type progressMetrics struct {
	mu      sync.Mutex
	rows    []progressObservation
	origin  time.Time
	failure chan error
}

func (metrics *progressMetrics) run(ctx context.Context, name string, repo domain.ContentHash, operation func(context.Context) error) error {
	row := &progressObservation{Operation: name, Repo: string(repo), permits: make(map[*resourcePermit]bool), origin: metrics.origin}
	started := time.Now()
	row.StartMS = float64(started.Sub(metrics.origin)) / float64(time.Millisecond)
	err := operation(context.WithValue(ctx, progressObservationKey{}, row))
	row.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
	row.EndMS = row.StartMS + row.ElapsedMS
	row.Outcome = "success"
	if err != nil {
		row.Outcome, row.Detail = "error", err.Error()
		if errors.Is(err, domain.ErrResourceOverloaded) {
			row.Outcome = "overloaded"
		} else if errors.Is(err, domain.ErrMemoryReadNotReady) || errors.Is(err, domain.ErrPendingProjectionNotReady) || errors.Is(err, domain.ErrGraphProjectionNotReady) || errors.Is(err, domain.ErrDocRangeIndexPending) {
			row.Outcome = "not_ready"
		} else if errors.Is(err, domain.ErrConflict) {
			row.Outcome = "conflict"
		} else if errors.Is(err, context.Canceled) {
			row.Outcome = "canceled"
		}
	}
	metrics.mu.Lock()
	metrics.rows = append(metrics.rows, *row)
	metrics.mu.Unlock()
	if row.Outcome == "error" {
		select {
		case metrics.failure <- fmt.Errorf("%s: %w", name, err):
		default:
		}
	}
	return err
}

func observeProgressPermit(ctx context.Context) *progressObservation {
	row, _ := ctx.Value(progressObservationKey{}).(*progressObservation)
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if row != nil && permit != nil && permit.live.Load() && !row.permits[permit] {
		row.permits[permit] = true
		row.QueueMS += float64(permit.wait) / float64(time.Millisecond)
	}
	return row
}

type progressStore struct{ *store.PostgresStore }

func observeProgressPhase(ctx context.Context, phase string, started time.Time) {
	if row, ok := ctx.Value(progressObservationKey{}).(*progressObservation); ok {
		if row.Phases == nil {
			row.Phases = make(map[string]float64)
		}
		row.Phases[phase] += float64(time.Since(started)) / float64(time.Millisecond)
	}
}

type progressRootPublication struct {
	outbound.PreparedRootDocPublication
	resources outbound.RootPublicationResources
}

func (publication *progressRootPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	if bounded, ok := publication.PreparedRootDocPublication.(outbound.BoundedRootDocPublication); ok {
		resources, err := bounded.RetainedResources()
		publication.resources = resources
		return resources, err
	}
	return outbound.RootPublicationResources{}, nil
}

func (publication *progressRootPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	if row, ok := ctx.Value(progressObservationKey{}).(*progressObservation); ok {
		row.RootHeapBytes, row.RootSpoolBytes = publication.resources.HeapBytes, publication.resources.SpoolBytes
		if permit, ok := ctx.Value(resourcePermitKey{}).(*resourcePermit); ok {
			row.ResumeWaitMS = float64(permit.resumeWait.Load()) / float64(time.Millisecond)
			row.QueueMS += row.ResumeWaitMS
		}
	}
	defer observeProgressPhase(ctx, "complete", time.Now())
	return publication.PreparedRootDocPublication.Complete(ctx, job, now)
}

type progressStagedRootPublication struct {
	*progressRootPublication
	stage outbound.StagedDocPublication
}

func (publication *progressStagedRootPublication) Stage(ctx context.Context, job domain.DocFinalizationJob, lease time.Duration) error {
	defer observeProgressPhase(ctx, "stage", time.Now())
	return publication.stage.Stage(ctx, job, lease)
}

func (storage *progressStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	defer observeProgressPhase(ctx, "prepare", time.Now())
	prepared, err := storage.PostgresStore.PrepareRootDocJob(ctx, job)
	if prepared == nil {
		return nil, err
	}
	wrapped := &progressRootPublication{PreparedRootDocPublication: prepared}
	if staged, ok := prepared.(outbound.StagedDocPublication); ok {
		return &progressStagedRootPublication{progressRootPublication: wrapped, stage: staged}, err
	}
	return wrapped, err
}

func observeProgressClaim(ctx context.Context, repo domain.ContentHash, phase string, err error) {
	row := observeProgressPermit(ctx)
	if row != nil && err == nil {
		row.Claims = append(row.Claims, progressClaim{Repo: repo, Phase: phase, StartMS: float64(time.Since(row.origin)) / float64(time.Millisecond)})
	}
}

func (storage *progressStore) WithinReadSnapshot(ctx context.Context, operation func(context.Context) error) error {
	row := observeProgressPermit(ctx)
	owned := !storage.InTransaction(ctx)
	started := time.Now()
	err := storage.PostgresStore.WithinReadSnapshot(ctx, operation)
	if row != nil && owned {
		row.ReadTxMS += float64(time.Since(started)) / float64(time.Millisecond)
	}
	return err
}

func (storage *progressStore) WithinRepository(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	row := observeProgressPermit(ctx)
	owned := !storage.InTransaction(ctx)
	started := time.Now()
	err := storage.PostgresStore.WithinRepository(ctx, repo, operation)
	if row != nil && owned {
		row.WriteTxMS += float64(time.Since(started)) / float64(time.Millisecond)
	}
	return err
}

func (storage *progressStore) ClaimGraphProjection(ctx context.Context, lease time.Duration) (domain.GraphProjectionLease, error) {
	observeProgressPermit(ctx)
	job, err := storage.PostgresStore.ClaimGraphProjection(ctx, lease)
	observeProgressClaim(ctx, job.RepoID, job.Phase, err)
	return job, err
}

func (storage *progressStore) ClaimGraphQualification(ctx context.Context, repo domain.ContentHash, lease time.Duration) (domain.GraphQualificationLease, error) {
	observeProgressPermit(ctx)
	job, err := storage.PostgresStore.ClaimGraphQualification(ctx, repo, lease)
	observeProgressClaim(ctx, job.RepoID, "qualify", err)
	return job, err
}

func (storage *progressStore) ClaimMemoryReadWork(ctx context.Context, repos []domain.ContentHash, lease time.Duration) (domain.MemoryReadWork, error) {
	observeProgressPermit(ctx)
	job, err := storage.PostgresStore.ClaimMemoryReadWork(ctx, repos, lease)
	observeProgressClaim(ctx, job.Target.RepoID, "prepare", err)
	return job, err
}

func (storage *progressStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	observeProgressPermit(ctx)
	job, err := storage.PostgresStore.ClaimDocJob(ctx, repo, now, lease)
	observeProgressClaim(ctx, job.RepoID, "publish", err)
	return job, err
}

func (storage *progressStore) ClaimPendingProjection(ctx context.Context, lease time.Duration) (domain.PendingProjectionLease, error) {
	observeProgressPermit(ctx)
	job, err := storage.PostgresStore.ClaimPendingProjection(ctx, lease)
	observeProgressClaim(ctx, job.RepoID, job.Phase, err)
	return job, err
}

type progressRepository struct {
	repo       domain.ContentHash
	selection  domain.EffectiveMemoryRequest
	memoryHash domain.ContentHash
	document   domain.ContentHash
	initial    domain.RepositoryViewSourceCheckpoint
	snapshots  []domain.ContentHash
	pending    map[string]domain.ContentHash
}

func progressSize(t *testing.T, name string, fallback int, allowed ...int) int {
	t.Helper()
	value := fallback
	if raw := os.Getenv(name); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatal(name, err)
		}
		value = parsed
	}
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	t.Fatalf("unsupported %s=%d", name, value)
	return 0
}

func progressDocument(ctx context.Context, service *Service, repo domain.ContentHash, name string, size int) (domain.DocFinalizationJob, error) {
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: name, SourceProvider: domain.ProviderCodex}}
	for position := range size / 1024 {
		event := domain.CIREvent{Seq: position, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("%s-%d ", name, position) + strings.Repeat("x", 1024)}}}
		if position%4 != 0 {
			event.Kind, event.Role = domain.EventToolResult, ""
			event.CallID, event.Output = fmt.Sprintf("tool-%d", position), event.Blocks[0].Text
			event.Blocks = nil
		}
		document.Events = append(document.Events, event)
	}
	manifest, chunks, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return domain.DocFinalizationJob{}, err
	}
	for hash, body := range chunks {
		if _, err := service.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{{Hash: hash, Data: body}}}); err != nil {
			return domain.DocFinalizationJob{}, err
		}
		delete(chunks, hash)
	}
	job, err := domain.NewDocFinalizationJobForRepresentation(repo, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, time.Now().UTC())
	if err != nil {
		return job, err
	}
	jobs, err := service.docJobs()
	if err != nil {
		return job, err
	}
	status, err := service.SubmitDocFinalization(ctx, repo, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw})
	if err != nil {
		return job, err
	}
	return jobs.GetDocJob(ctx, repo, status.ID)
}

func TestPGReadModelMixedProgress(t *testing.T) {
	if os.Getenv("CXT_READ_PROGRESS") != "1" {
		t.Skip("opt-in mixed progress experiment")
	}
	dsn, err := url.Parse(os.Getenv("CXT_TEST_DSN"))
	if err != nil || (dsn.Scheme != "postgres" && dsn.Scheme != "postgresql") || !strings.HasPrefix(strings.TrimPrefix(dsn.Path, "/"), "cxt_read_progress_") {
		t.Fatal("explicit disposable cxt_read_progress_ database required")
	}
	snapshotCount := progressSize(t, "CXT_READ_PROGRESS_SNAPSHOTS", 100, 100, 1000)
	documentMiB := progressSize(t, "CXT_READ_PROGRESS_DOC_MIB", 4, 4, 32)
	ctx, gate := admissionTestContext(resourceWaitLimit)
	ctx = inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), binaryDocumentIdentitiesSupported())
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	var repositories []progressRepository
	var storage *progressStore
	for index := range 2 {
		fixture, original, _, repo, request, _ := qualifiedMemoryFixturePG(t)
		qualifyMemoryPreparationCostPG(t, fixture, original, repo, request)
		metadata, err := original.GetRepo(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		owner := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("progress%d", time.Now().UnixNano()), Email: "progress@example.test"}
		if err := original.UpsertUser(ctx, owner); err != nil {
			t.Fatal(err)
		}
		record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: owner.Username, Name: "Progress", Slug: "progress"}
		if err := original.CreateRepository(ctx, record); err != nil {
			t.Fatal(err)
		}
		metadata.RepositoryID = record.ID
		if _, err := original.PutRepo(ctx, metadata); err != nil {
			t.Fatal(err)
		}
		if err := fixture.ConfigureConversationRootPublication(true); err != nil {
			t.Fatal(err)
		}
		if storage == nil {
			storage = &progressStore{PostgresStore: original}
		}
		tip := request.Selection.SnapshotID
		snapshots := []domain.ContentHash{tip}
		pending := make(map[string]domain.ContentHash)
		for number := range snapshotCount - 1 {
			tip = collaborationSnapshot(t, original, repo, fmt.Sprintf("repo-%d-snapshot-%d", index, number), tip)
			snapshots = append(snapshots, tip)
		}
		if err := original.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: tip}, ""); err != nil {
			t.Fatal(err)
		}
		for number := range 2 {
			target := collaborationSnapshot(t, original, repo, fmt.Sprintf("pending-%d", number), tip)
			snapshot, err := original.GetSnapshot(ctx, repo, target)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.SessionID, snapshot.Message = fmt.Sprintf("pending-%d", number), "hook: pending"
			snapshots = append(snapshots, target)
			pending[snapshot.SessionID] = target
			if err := original.PutSnapshot(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			if err := fixture.PutPending(ctx, repo, snapshot.SessionID, domain.Pending{SessionID: snapshot.SessionID, Branch: "main", Provider: domain.ProviderCodex, Target: target}); err != nil {
				t.Fatal(err)
			}
		}
		if err := original.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
			t.Fatal(err)
		}
		job, err := progressDocument(ctx, fixture, repo, fmt.Sprintf("ready-%d", index), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.ProcessDocFinalizations(ctx, 1); err != nil {
			t.Fatal("initial document", err)
		}
		if err := original.PutSnapshot(ctx, domain.Snapshot{ID: job.DocHash, RepoID: repo, DocHash: job.DocHash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Parents: []domain.ContentHash{tip}}); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, job.DocHash)
		sort.Slice(snapshots, func(left, right int) bool { return snapshots[left] < snapshots[right] })
		repositories = append(repositories, progressRepository{repo: repo, selection: request, memoryHash: request.Selection.MemoryHash, document: job.DocHash, snapshots: snapshots, pending: pending})
	}
	service := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := service.ConfigureConversationRootPublication(true); err != nil {
		t.Fatal(err)
	}
	repos := []domain.ContentHash{repositories[0].repo, repositories[1].repo}
	for _, configure := range []func(context.Context, []domain.ContentHash) error{service.ConfigureGraphProjections, service.ConfigureGraphQualifications, service.ConfigurePendingProjections, service.ConfigureMemoryReadPreparations} {
		if err := configure(ctx, repos); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.ConfigureGraphReads(repos); err != nil {
		t.Fatal(err)
	}
	for index := range repositories {
		repositories[index].selection.Selection.MemoryHash = ""
		if _, err := service.RequestMemoryReadPreparation(ctx, repositories[index].repo, repositories[index].selection); err != nil {
			t.Fatal(err)
		}
	}
	var anchor *PreparedMemoryRead
	workerCalls := []struct {
		name string
		run  func(context.Context) error
	}{
		{"graph_worker", func(bound context.Context) error { return service.ProcessGraphProjections(bound, 1) }},
		{"graph_qualification", func(bound context.Context) error { return service.ProcessGraphQualifications(bound, 1) }},
		{"pending_worker", func(bound context.Context) error { return service.ProcessPendingProjections(bound, 1) }},
		{"memory_worker", func(bound context.Context) error { return service.processMemoryReadPreparations(bound, 1, &anchor) }},
		{"document_worker", func(bound context.Context) error { return service.ProcessDocFinalizations(bound, 1) }},
	}
	warmDeadline := time.Now().Add(90 * time.Second)
	for {
		if err := service.ProcessDocRangeJobs(ctx, 1); err != nil {
			t.Fatal("warm visible index", err)
		}
		for _, worker := range workerCalls {
			if err := worker.run(ctx); err != nil {
				t.Fatal("warm", worker.name, err)
			}
		}
		ready := true
		for index := range repositories {
			repo := &repositories[index]
			graph, err := storage.ReadQualifiedGraphProjection(ctx, repo.repo)
			if err != nil {
				t.Fatal(err)
			}
			repo.initial = graph.Applied
			_, memoryErr := service.QueryCurrentMemory(ctx, repo.repo, repo.selection)
			_, pendingErr := service.GetPendingState(ctx, repo.repo)
			_, visibleErr := service.ReadDocVisiblePage(ctx, repo.repo, repo.document, domain.DocVisiblePageRequest{View: "chat", Limit: 25})
			ready = ready && graph.Complete && memoryErr == nil && pendingErr == nil && visibleErr == nil
		}
		if ready {
			break
		}
		if time.Now().After(warmDeadline) {
			t.Fatal("initial read models did not complete")
		}
		time.Sleep(50 * time.Millisecond)
	}
	metrics := &progressMetrics{origin: time.Now(), failure: make(chan error, 1)}
	defer func() {
		metrics.mu.Lock()
		defer metrics.mu.Unlock()
		encoded, err := json.MarshalIndent(metrics.rows, "", "  ")
		if err != nil {
			t.Error(err)
		}
		if directory := os.Getenv("CXT_READ_PROGRESS_ARTIFACTS"); directory != "" {
			if err := os.WriteFile(filepath.Join(directory, "attempts.json"), encoded, 0600); err != nil {
				t.Error(err)
			}
		}
		groups := make(map[string][]float64)
		outcomes := make(map[string]int)
		for _, row := range metrics.rows {
			groups[row.Operation] = append(groups[row.Operation], row.ElapsedMS)
			outcomes[row.Operation+"/"+row.Outcome]++
		}
		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			values := groups[key]
			sort.Float64s(values)
			t.Logf("attempts operation=%s count=%d p50_ms=%.2f p95_ms=%.2f max_ms=%.2f", key, len(values), values[(len(values)*50+99)/100-1], values[(len(values)*95+99)/100-1], values[len(values)-1])
		}
		counts, _ := json.Marshal(outcomes)
		t.Logf("outcomes=%s; granted_queue_ms excludes failed admission; transaction observations are owned adapter scopes, not SQL lock time; no HTTP/auth/PR topology coverage", counts)
	}()
	load, stop := context.WithCancel(ctx)
	var goroutines sync.WaitGroup
	defer func() { stop(); goroutines.Wait(); assertResourcesReleased(t, gate) }()
	for _, worker := range workerCalls {
		goroutines.Go(func() {
			for load.Err() == nil {
				_ = metrics.run(load, worker.name, "", worker.run)
				select {
				case <-load.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
	for _, repo := range repositories {
		goroutines.Go(func() {
			position := 0
			visibleRequest := domain.DocVisiblePageRequest{View: "chat", Limit: 25}
			var visiblePrevious *domain.DocVisiblePage
			for load.Err() == nil {
				_ = metrics.run(load, "graph_read", repo.repo, func(bound context.Context) error {
					graph, err := service.QueryGraphState(bound, repo.repo, "")
					if err == nil && (graph.PrimaryBranch != "main" || !reflect.DeepEqual(graph.SnapshotIDs, repo.snapshots)) {
						return fmt.Errorf("graph lost fixture snapshots or main identity")
					}
					return err
				})
				_ = metrics.run(load, "pending_read", repo.repo, func(bound context.Context) error {
					page, err := service.GetPendingState(bound, repo.repo)
					if err == nil && len(page.Pending) != 2 {
						return fmt.Errorf("pending state lost captures: %d", len(page.Pending))
					}
					if err == nil {
						seen := make(map[string]bool)
						for _, capture := range page.Pending {
							expected, exists := repo.pending[capture.SessionID]
							if !exists || seen[capture.SessionID] || capture.Target != expected || capture.Provider != domain.ProviderCodex || capture.Branch != "main" {
								return fmt.Errorf("pending state changed repository capture identity")
							}
							seen[capture.SessionID] = true
						}
						if len(seen) != len(repo.pending) {
							return fmt.Errorf("pending state omitted a session")
						}
					}
					return err
				})
				_ = metrics.run(load, "memory_read", repo.repo, func(bound context.Context) error {
					_, err := service.QueryCurrentMemory(bound, repo.repo, repo.selection)
					return err
				})
				_ = metrics.run(load, "chat_read", repo.repo, func(bound context.Context) error {
					page, err := service.ReadDocViewEvents(bound, repo.repo, repo.document, "", position, 25, "chat")
					if err == nil {
						visible := 0
						for _, event := range page.Events {
							if event.Event.Kind == domain.EventMessage {
								visible++
								if event.Deferred || event.Index%4 != 0 || len(event.Event.Blocks) != 1 || !strings.HasPrefix(event.Event.Blocks[0].Text, "ready-") {
									return fmt.Errorf("chat message changed at %d", event.Index)
								}
							} else if !event.Deferred {
								return fmt.Errorf("tool detail was fetched eagerly")
							}
						}
						if visible == 0 || visible != 25 && page.Next != -1 {
							return fmt.Errorf("chat page has %d visible events", visible)
						}
						position = page.Next
						if position == -1 {
							position = 0
						}
					}
					return err
				})
				_ = metrics.run(load, "visible_read", repo.repo, func(bound context.Context) error {
					continuation := visibleRequest.Cursor != ""
					page, err := service.ReadDocVisiblePage(bound, repo.repo, repo.document, visibleRequest)
					if err != nil {
						return err
					}
					if len(page.Units) == 0 || page.To <= page.From || len(page.Events) != 0 {
						return fmt.Errorf("invalid visible page or eager folded details")
					}
					if !continuation && page.NextCursor == nil {
						return fmt.Errorf("visible first page omitted continuation for 256-message fixture")
					}
					if visiblePrevious != nil && (page.From != visiblePrevious.To || page.Generation != visiblePrevious.Generation) {
						return fmt.Errorf("visible continuation mixed positions or generations")
					}
					if page.NextCursor == nil {
						visibleRequest, visiblePrevious = domain.DocVisiblePageRequest{View: "chat", Limit: 25}, nil
					} else {
						visibleRequest, visiblePrevious = domain.DocVisiblePageRequest{Cursor: *page.NextCursor}, &page
					}
					if continuation {
						row, _ := bound.Value(progressObservationKey{}).(*progressObservation)
						row.Detail = "continuation"
					}
					return nil
				})
				select {
				case <-load.Done():
					return
				case <-time.After(150 * time.Millisecond):
				}
			}
		})
	}
	for round := range 3 {
		metrics.mu.Lock()
		firstObservation := len(metrics.rows)
		metrics.mu.Unlock()
		jobs := make(chan domain.DocFinalizationJob, len(repositories))
		failures := make(chan error, len(repositories))
		for index := range repositories {
			repo := &repositories[index]
			digest := capturedMemoryDigestPG(repo.selection.Selection.SnapshotID, "qualified source")
			digest.PreviousMemoryHash = repo.memoryHash
			digest.Summary = fmt.Sprintf("current repository %s round %d", repo.repo, round)
			if err := metrics.run(ctx, "memory_write", repo.repo, func(bound context.Context) error {
				var err error
				repo.memoryHash, err = service.PutMemoryDigestCAS(bound, repo.repo, digest)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.RequestMemoryReadPreparation(ctx, repo.repo, repo.selection); err != nil {
				t.Fatal(err)
			}
			goroutines.Go(func() {
				err := metrics.run(load, "chunk_upload", repo.repo, func(bound context.Context) error {
					job, err := progressDocument(bound, service, repo.repo, fmt.Sprintf("%s-round-%d", repo.repo, round), documentMiB<<20)
					if err == nil {
						jobs <- job
					}
					return err
				})
				if err != nil {
					failures <- err
				}
			})
		}
		var accepted []domain.DocFinalizationJob
		for len(accepted) < len(repositories) {
			select {
			case err := <-metrics.failure:
				t.Fatal("mixed operation", err)
			case job := <-jobs:
				accepted = append(accepted, job)
			case err := <-failures:
				t.Fatal("upload", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		deadline := time.Now().Add(90 * time.Second)
		for {
			select {
			case err := <-metrics.failure:
				t.Fatal("mixed operation", err)
			default:
			}
			ready := true
			for _, job := range accepted {
				current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
				if err != nil || current.State == "rejected" {
					t.Fatal("publication", current.State, err)
				}
				ready = ready && current.State == "completed"
			}
			for _, repo := range repositories {
				graph, err := storage.ReadQualifiedGraphProjection(ctx, repo.repo)
				if err != nil {
					t.Fatal(err)
				}
				ready = ready && graph.Complete && graph.Applied.Epoch == repo.initial.Epoch && graph.Applied.Sequence > repo.initial.Sequence
				target, err := memoryReadTarget(repo.repo, repo.selection)
				if err != nil {
					t.Fatal(err)
				}
				identity, err := domain.MemoryReadTargetID(ctx, target)
				if err != nil {
					t.Fatal(err)
				}
				work, err := storage.ReadMemoryReadWork(ctx, repo.repo, identity)
				if err != nil {
					t.Fatal(err)
				}
				ready = ready && work.State == "completed"
				metrics.mu.Lock()
				for _, name := range []string{"graph_read", "pending_read", "memory_read", "chat_read", "visible_read"} {
					observed := false
					for _, row := range metrics.rows[firstObservation:] {
						observed = observed || row.Operation == name && row.Repo == string(repo.repo) && row.Outcome == "success"
					}
					ready = ready && observed
				}
				metrics.mu.Unlock()
			}
			if ready {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d did not complete both repositories within 90s", round)
			}
			time.Sleep(150 * time.Millisecond)
		}
		for index := range repositories {
			graph, err := storage.ReadQualifiedGraphProjection(ctx, repositories[index].repo)
			if err != nil {
				t.Fatal(err)
			}
			repositories[index].initial = graph.Applied
		}
		t.Logf("round=%d both repositories advanced graph, completed memory and published %d document jobs", round, len(accepted))
	}
	stop()
	goroutines.Wait()
	for _, repo := range repositories {
		want, err := service.QueryEffectiveMemory(ctx, repo.repo, repo.selection)
		if err != nil {
			t.Fatal(err)
		}
		got, err := service.QueryCurrentMemory(ctx, repo.repo, repo.selection)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("final current memory differs from reference", err)
		}
		continued, overlapped := false, false
		publicationReads := make(map[string]int)
		for _, read := range metrics.rows {
			if read.Repo != string(repo.repo) || read.Outcome != "success" {
				continue
			}
			continued = continued || read.Operation == "visible_read" && read.Detail == "continuation"
			if read.Operation != "graph_read" && read.Operation != "pending_read" && read.Operation != "chat_read" && read.Operation != "visible_read" {
				continue
			}
			for _, work := range metrics.rows {
				if work.Outcome != "success" || work.Operation != "document_worker" {
					continue
				}
				for _, claim := range work.Claims {
					if claim.Phase == "publish" && read.StartMS >= claim.StartMS && read.EndMS <= work.EndMS {
						overlapped = true
						publicationReads[read.Operation]++
					}
				}
			}
		}
		if !continued || !overlapped {
			t.Errorf("repository %s lacked actual continuation=%v or interactive read within publication=%v", repo.repo, continued, overlapped)
		}
		overlapCounts, _ := json.Marshal(publicationReads)
		t.Logf("repository=%s successful interactive reads wholly within document publication=%s; requires at least one per repository, not one per API", repo.repo, overlapCounts)
		for _, name := range []string{"graph_read", "pending_read", "memory_read", "chat_read", "visible_read"} {
			count := 0
			for _, row := range metrics.rows {
				if row.Operation == name && row.Repo == string(repo.repo) && row.Outcome == "success" {
					count++
				}
			}
			if count < 3 {
				t.Errorf("repository %s %s made only %d successful reads", repo.repo, name, count)
			}
		}
	}
	for _, row := range metrics.rows {
		if row.Outcome == "error" {
			t.Errorf("unexpected %s error: %s", row.Operation, row.Detail)
		}
	}
	t.Logf("scope=two repositories x %d linear snapshots; three rounds of %dMiB text payload/repository plus JSON overhead; 75%% tool events; worker call cadence=100ms (not production timers); fixture setup included in application process; pool=2", snapshotCount, documentMiB)
}
