//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave40WorkersPGStore struct {
	*store.PostgresStore
	test                *testing.T
	gate                *resourceAdmission
	graphClaims         []domain.GraphProjectionLease
	pendingClaims       []domain.PendingProjectionLease
	qualificationClaims []domain.GraphQualificationLease
	graphRequests       []domain.RepositoryViewSourceRequest
	graphReferences     []domain.GraphProjectionReference
	graphPublications   []domain.GraphProjectionLease
	pendingPublications []domain.PendingProjectionLease
	completed           []domain.GraphQualificationResult
	graphFailures       []domain.GraphProjectionLease
	pendingFailures     []domain.PendingProjectionLease
	qualificationFails  []domain.GraphQualificationLease
	failureReasons      []string
	firstPageContext    context.Context
	afterFirstPage      func(context.Context) error
	denyRepository      bool
}

func (storage *wave40WorkersPGStore) admitted(ctx context.Context, budget time.Duration) {
	storage.test.Helper()
	permit, present := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	deadline, bounded := ctx.Deadline()
	if !present || permit.owner != storage.gate || !permit.live.Load() || !bounded || time.Until(deadline) > budget || ctx.Err() != nil {
		storage.test.Fatal("native owner lost the existing gate or original time budget", budget, ctx.Err())
	}
}

func (storage *wave40WorkersPGStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if storage.denyRepository {
		return domain.Repo{}, domain.ErrForbidden
	}
	return storage.PostgresStore.GetRepo(ctx, repo)
}

func (storage *wave40WorkersPGStore) ClaimGraphProjection(ctx context.Context, duration time.Duration) (domain.GraphProjectionLease, error) {
	storage.admitted(ctx, 30*time.Second)
	if duration != time.Minute || storage.InTransaction(ctx) {
		storage.test.Fatal("graph claim changed the lease or borrowed a transaction")
	}
	lease, err := storage.PostgresStore.ClaimGraphProjection(ctx, duration)
	if err == nil {
		storage.graphClaims = append(storage.graphClaims, lease)
	}
	return lease, err
}

func (storage *wave40WorkersPGStore) ClaimPendingProjection(ctx context.Context, duration time.Duration) (domain.PendingProjectionLease, error) {
	storage.admitted(ctx, 30*time.Second)
	if duration != time.Minute || storage.InTransaction(ctx) {
		storage.test.Fatal("pending claim changed the lease or borrowed a transaction")
	}
	lease, err := storage.PostgresStore.ClaimPendingProjection(ctx, duration)
	if err == nil {
		storage.pendingClaims = append(storage.pendingClaims, lease)
	}
	return lease, err
}

func (storage *wave40WorkersPGStore) ClaimGraphQualification(ctx context.Context, repo domain.ContentHash, duration time.Duration) (domain.GraphQualificationLease, error) {
	storage.admitted(ctx, 5*time.Second)
	if duration != time.Minute || storage.InTransaction(ctx) {
		storage.test.Fatal("qualification claim changed the lease or borrowed a transaction")
	}
	lease, err := storage.PostgresStore.ClaimGraphQualification(ctx, repo, duration)
	if err == nil {
		storage.qualificationClaims = append(storage.qualificationClaims, lease)
	}
	return lease, err
}

func (storage *wave40WorkersPGStore) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	storage.admitted(ctx, 30*time.Second)
	storage.graphRequests = append(storage.graphRequests, request)
	page, err := storage.PostgresStore.RepositoryGraphSourceChanges(ctx, repo, request)
	if err == nil && request.Cursor == "" {
		if !storage.InReadOnlyTransaction(ctx) {
			storage.test.Fatal("first graph page and reference did not share an owned read snapshot")
		}
		storage.firstPageContext = ctx
		if storage.afterFirstPage != nil {
			hook := storage.afterFirstPage
			storage.afterFirstPage = nil
			err = hook(ctx)
		}
	}
	return page, err
}

func (storage *wave40WorkersPGStore) SaveGraphProjectionPageWithReference(ctx context.Context, lease domain.GraphProjectionLease, page domain.RepositoryViewSourcePage, reference domain.GraphProjectionReference) error {
	storage.admitted(ctx, 30*time.Second)
	if !slices.Contains(storage.graphClaims, lease) || storage.InTransaction(ctx) || reference.Validate() != nil {
		storage.test.Fatal("first-page save changed the claimed lease or retained the read transaction")
	}
	if err := storage.PostgresStore.SaveGraphProjectionPageWithReference(ctx, lease, page, reference); err != nil {
		return err
	}
	storage.graphReferences = append(storage.graphReferences, reference)
	return nil
}

func (storage *wave40WorkersPGStore) SaveGraphProjectionPage(ctx context.Context, lease domain.GraphProjectionLease, page domain.RepositoryViewSourcePage) error {
	storage.admitted(ctx, 30*time.Second)
	if !slices.Contains(storage.graphClaims, lease) {
		storage.test.Fatal("graph continuation changed the original lease")
	}
	return storage.PostgresStore.SaveGraphProjectionPage(ctx, lease, page)
}

func (storage *wave40WorkersPGStore) SavePendingProjectionPage(ctx context.Context, lease domain.PendingProjectionLease, page domain.RepositoryViewSourcePage) error {
	storage.admitted(ctx, 30*time.Second)
	if !slices.Contains(storage.pendingClaims, lease) {
		storage.test.Fatal("pending capture changed the original lease")
	}
	return storage.PostgresStore.SavePendingProjectionPage(ctx, lease, page)
}

func (storage *wave40WorkersPGStore) PublishGraphProjection(ctx context.Context, lease domain.GraphProjectionLease, parts []domain.GraphProjectionPart) error {
	storage.admitted(ctx, 30*time.Second)
	if !slices.Contains(storage.graphClaims, lease) || lease.Phase != "project" {
		storage.test.Fatal("graph publication changed the original dataset, checkpoint, fence, or lease")
	}
	if err := storage.PostgresStore.PublishGraphProjection(ctx, lease, parts); err != nil {
		return err
	}
	storage.graphPublications = append(storage.graphPublications, lease)
	return nil
}

func (storage *wave40WorkersPGStore) PublishPendingProjection(ctx context.Context, lease domain.PendingProjectionLease, parts []domain.PendingProjectionPart) error {
	storage.admitted(ctx, 30*time.Second)
	if !slices.Contains(storage.pendingClaims, lease) || lease.Phase != "project" {
		storage.test.Fatal("pending publication changed the original dataset, checkpoint, fence, or lease")
	}
	if err := storage.PostgresStore.PublishPendingProjection(ctx, lease, parts); err != nil {
		return err
	}
	storage.pendingPublications = append(storage.pendingPublications, lease)
	return nil
}

func (storage *wave40WorkersPGStore) FinishGraphQualification(ctx context.Context, lease domain.GraphQualificationLease, result domain.GraphQualificationResult) error {
	storage.admitted(ctx, 5*time.Second)
	if !slices.Contains(storage.qualificationClaims, lease) || storage.InTransaction(ctx) || result.GraphQualificationKey != lease.GraphQualificationKey {
		storage.test.Fatal("qualification receipt changed the claim identity or retained its comparison snapshot")
	}
	if err := storage.PostgresStore.FinishGraphQualification(ctx, lease, result); err != nil {
		return err
	}
	storage.completed = append(storage.completed, result)
	return nil
}

func (storage *wave40WorkersPGStore) FailGraphProjection(ctx context.Context, lease domain.GraphProjectionLease, reason string, retry bool) error {
	storage.admitted(ctx, 5*time.Second)
	storage.graphFailures = append(storage.graphFailures, lease)
	storage.failureReasons = append(storage.failureReasons, reason)
	return storage.PostgresStore.FailGraphProjection(ctx, lease, reason, retry)
}

func (storage *wave40WorkersPGStore) FailPendingProjection(ctx context.Context, lease domain.PendingProjectionLease, reason string, retry bool) error {
	storage.admitted(ctx, 5*time.Second)
	storage.pendingFailures = append(storage.pendingFailures, lease)
	storage.failureReasons = append(storage.failureReasons, reason)
	return storage.PostgresStore.FailPendingProjection(ctx, lease, reason, retry)
}

func (storage *wave40WorkersPGStore) FailGraphQualification(ctx context.Context, lease domain.GraphQualificationLease, reason string) error {
	storage.admitted(ctx, 5*time.Second)
	storage.qualificationFails = append(storage.qualificationFails, lease)
	storage.failureReasons = append(storage.failureReasons, reason)
	return storage.PostgresStore.FailGraphQualification(ctx, lease, reason)
}

func wave40IsolatedPG(test *testing.T) {
	test.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("CXT_TEST_DSN unset; Wave40 proofs require a local PostgreSQL DSN and a fresh owned database")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("invalid local CXT_TEST_DSN")
	}
	local := func(host string) bool {
		if host == "localhost" || strings.HasPrefix(host, "/") {
			return true
		}
		address := net.ParseIP(host)
		return address != nil && address.IsLoopback()
	}
	if !local(config.ConnConfig.Host) {
		test.Fatal("Wave40 proofs only accept a local CXT_TEST_DSN")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !local(fallback.Host) {
			test.Fatal("Wave40 proofs refuse a nonlocal fallback")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, ctx))
}

func wave40PGContext(ctx context.Context, storage *store.PostgresStore) context.Context {
	supported := ReadDocumentIdentities(storage, storage)
	ctx = inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), supported)
	return outbound.WithDocumentIdentityCompatibility(ctx, supported, supported)
}

func wave40ConfigureWorkersPG(test *testing.T, storage *store.PostgresStore, repo domain.ContentHash) {
	test.Helper()
	ctx := wave40PGContext(context.Background(), storage)
	for _, configure := range []func(context.Context, domain.ContentHash, bool) error{
		storage.ConfigurePendingProjection, storage.ConfigureGraphProjection, storage.ConfigureGraphQualification,
	} {
		if err := configure(ctx, repo, true); err != nil {
			test.Fatal(err)
		}
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(wave40PGContext(context.Background(), storage), 10*time.Second)
		defer cancel()
		for _, configure := range []func(context.Context, domain.ContentHash, bool) error{
			storage.ConfigureGraphQualification, storage.ConfigureGraphProjection, storage.ConfigurePendingProjection,
		} {
			if err := configure(cleanup, repo, false); err != nil {
				test.Error("owned projection queue cleanup failed", err)
			}
		}
		graph, graphErr := storage.ReadGraphProjection(cleanup, repo)
		pending, pendingErr := storage.ReadPendingProjection(cleanup, repo)
		if graphErr != nil || pendingErr != nil || graph.Enabled || pending.Enabled {
			test.Error("owned projection queue cleanup left active readers", graphErr, pendingErr)
		}
	})
}

func wave40NewPGWorkers(test *testing.T, storage *wave40WorkersPGStore, repo domain.ContentHash) *ProjectionWorkers {
	test.Helper()
	workers, err := NewProjectionWorkers(ProjectionWorkersConfig{Metadata: storage, Blobs: storage,
		Profile: projectionworker.Complete, PendingEnabled: true, GraphEnabled: true,
		QualificationRepositories: []domain.ContentHash{repo}})
	if err != nil || workers == nil || workers.Pending == nil || workers.Graph == nil || workers.Qualification == nil {
		test.Fatal("complete public factory did not assemble all three native PG owners", err)
	}
	return workers
}

func wave40PublishGraphPG(test *testing.T, ctx context.Context, workers *ProjectionWorkers, storage *store.PostgresStore, repo domain.ContentHash) domain.GraphProjectionRead {
	test.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		if err := workers.Graph.Process(ctx, 1); err != nil {
			test.Fatal("native graph processing failed", err)
		}
		read, err := storage.ReadGraphProjection(ctx, repo)
		if err != nil || read.NeedsAttention {
			test.Fatal("native graph candidate failed", err, read)
		}
		if read.Complete {
			return read
		}
	}
	test.Fatal("native graph did not publish within the original bounded attempts")
	return domain.GraphProjectionRead{}
}

func TestPGWave40ProjectionWorkersCaptureRestartPublishAndQualifyWithoutService(test *testing.T) {
	wave40IsolatedPG(test)
	fixture, connection := graphSourceQueryFixturePG(test)
	storage, repo := fixture.storage.PostgresStore, fixture.basis.repo.ID
	base, gate := admissionTestContext(5 * time.Second)
	ctx, cancel := context.WithTimeout(wave40PGContext(base, storage), 90*time.Second)
	defer cancel()
	for index := 0; index <= domain.DefaultRepositoryViewSourceLimit; index++ {
		collaborationSnapshot(test, storage, repo, fmt.Sprintf("wave40 graph capture %03d", index))
	}
	live := collaborationSnapshot(test, storage, repo, "wave40 useful pending", fixture.main.Target)
	if err := storage.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "wave40 live", Provider: domain.ProviderCodex, Target: live, Branch: "main", UpdatedAt: time.Now().UTC()}); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutUnsync(ctx, repo, domain.Unsync{RepoID: repo, User: "wave40 author", Branch: "held", Target: live, UpdatedAt: time.Now().UTC()}); err != nil {
		test.Fatal(err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	wave40ConfigureWorkersPG(test, storage, repo)
	wantGraph, err := fixture.basis.worker.core.GetRepositoryView(ctx, repo)
	if err != nil || wantGraph.Graph == nil || len(wantGraph.Graph.Integrations) == 0 {
		test.Fatal("independent fixture oracle lacks useful Git integration evidence", err)
	}
	expected, err := domain.GraphViewDigest(ctx, repo, wantGraph)
	if err != nil {
		test.Fatal(err)
	}
	pinned, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	observed := &wave40WorkersPGStore{PostgresStore: storage, test: test, gate: gate}
	observed.afterFirstPage = func(bound context.Context) error {
		if !storage.InReadOnlyTransaction(bound) {
			test.Fatal("interleaving escaped the first-page read snapshot")
		}
		changed, err := connection.Exec(ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, string(repo), string(fixture.main.Target), "committed after Wave40 first page")
		if err == nil && changed.RowsAffected() != 1 {
			return domain.ErrIntegrity
		}
		return err
	}
	workers := wave40NewPGWorkers(test, observed, repo)
	if len(observed.graphClaims)+len(observed.pendingClaims)+len(observed.qualificationClaims) != 0 {
		test.Fatal("public factory started worker execution during assembly")
	}
	if err := workers.Graph.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	var cursor, dataset, phase string
	if err := connection.QueryRow(ctx, `SELECT cursor,dataset::text,phase FROM graph_projection_work WHERE repo_id=$1`, string(repo)).Scan(&cursor, &dataset, &phase); err != nil || cursor == "" || phase != "capture" {
		test.Fatal("first native graph lease did not persist a resumable page", cursor, phase, err)
	}
	partial, err := storage.ReadGraphProjection(ctx, repo)
	if err != nil || partial.Complete || partial.Ready || len(partial.Parts) != 0 || len(observed.graphClaims) != 1 || len(observed.graphPublications) != 0 {
		test.Fatal("first page drained multiple claims or exposed incomplete output", partial, err)
	}
	if len(observed.graphReferences) != 1 || observed.graphReferences[0].Expected != expected || observed.graphReferences[0].Applied != pinned || observed.graphReferences[0].Dataset != dataset || observed.firstPageContext == nil || !errors.Is(observed.firstPageContext.Err(), context.Canceled) {
		test.Fatal("first page lost its independent pinned reference or snapshot cleanup", observed.graphReferences)
	}
	current, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil || current.Sequence <= pinned.Sequence {
		test.Fatal("interleaved source write did not distinguish current and captured generations", err)
	}
	wantPending, err := fixture.basis.worker.core.GetPendingState(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	if err := workers.Pending.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	pendingPartial, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || pendingPartial.Ready || len(observed.pendingClaims) != 1 || len(observed.pendingPublications) != 0 {
		test.Fatal("pending capture did not remain a bounded incomplete page", err)
	}
	var pendingDataset, pendingCursor string
	if err := connection.QueryRow(ctx, `SELECT dataset::text,cursor FROM pending_projection_work WHERE repo_id=$1`, string(repo)).Scan(&pendingDataset, &pendingCursor); err != nil || pendingCursor == "" || pendingDataset != observed.pendingClaims[0].Dataset {
		test.Fatal("pending capture lost its durable dataset or continuation", err)
	}
	assertResourcesReleased(test, gate)
	peer, err := store.NewPostgresStore(ctx, fixture.basis.dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	restartedStore := &wave40WorkersPGStore{PostgresStore: peer, test: test, gate: gate}
	restarted := wave40NewPGWorkers(test, restartedStore, repo)
	candidate := wave40PublishGraphPG(test, ctx, restarted, peer, repo)
	gotGraph, err := domain.DecodeGraphViewProjection(ctx, repo, candidate.Parts)
	if err != nil || !reflect.DeepEqual(gotGraph, wantGraph) || candidate.Dataset != dataset || candidate.Applied != pinned || candidate.Source != current || candidate.Ready {
		test.Fatal("restarted native graph changed captured content, source, or dataset identity", err, candidate)
	}
	if len(restartedStore.graphRequests) == 0 || restartedStore.graphRequests[0].Cursor != cursor || restartedStore.graphRequests[0].Limit != domain.DefaultRepositoryViewSourceLimit || restartedStore.graphRequests[0].After != nil {
		test.Fatal("restart lost the original bounded graph continuation", restartedStore.graphRequests)
	}
	var pending domain.PendingProjectionRead
	for attempt := 0; attempt < 20; attempt++ {
		if err := restarted.Pending.Process(ctx, 1); err != nil {
			test.Fatal(err)
		}
		pending, err = peer.ReadPendingProjection(ctx, repo)
		if err != nil || pending.NeedsAttention {
			test.Fatal("native pending projection failed", err)
		}
		if pending.Ready {
			break
		}
	}
	gotPending, err := domain.DecodePendingProjection(ctx, repo, pending.Parts)
	if err != nil || !pending.Ready || pending.Source != pending.Applied || pending.Dataset != pendingDataset || !reflect.DeepEqual(gotPending, wantPending) {
		test.Fatal("restarted native pending owner differs from independent direct classification", err, pending)
	}
	if _, err := peer.ReadGraphQualification(ctx, candidate.QualificationKey()); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("graph publication fabricated a qualification receipt", err)
	}
	if err := restarted.Qualification.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	receipt, err := peer.ReadGraphQualification(ctx, candidate.QualificationKey())
	if err != nil || !receipt.Matched() || receipt.GraphQualificationKey != candidate.QualificationKey() || receipt.Expected != expected || receipt.Actual != expected || len(restartedStore.qualificationClaims) != 1 || len(restartedStore.completed) != 1 {
		test.Fatal("native qualification did not persist the exact independent completion receipt", receipt, err)
	}
	served, err := peer.ReadQualifiedGraphProjection(ctx, repo)
	if err != nil || !served.Complete || served.Ready || served.Applied != pinned || served.Source != current || served.Dataset != dataset || served.ServingFence <= 0 || domain.ValidateGraphReadQualification(ctx, served) != nil {
		test.Fatal("qualified serving lost the exact completed prior-generation identity", err, served)
	}
	servedView, err := domain.DecodeGraphViewProjection(ctx, repo, served.Parts)
	if err != nil || !reflect.DeepEqual(servedView, wantGraph) {
		test.Fatal("completion receipt covered different serving content", err)
	}
	after, err := peer.ReadRepositoryGraphSourceGeneration(ctx, repo)
	if err != nil || after != current || len(restartedStore.graphPublications) != 1 || len(restartedStore.pendingPublications) != 1 || len(observed.failureReasons)+len(restartedStore.failureReasons) != 0 {
		test.Fatal("native owners rewrote source or failed to do useful exactly scoped work", after, err)
	}
	for _, table := range []string{"graph_projection_work", "pending_projection_work", "graph_qualification"} {
		var released bool
		if err := connection.QueryRow(ctx, `SELECT lease_until IS NULL FROM `+table+` WHERE repo_id=$1`, string(repo)).Scan(&released); err != nil || !released {
			test.Fatal("completed native owner retained a durable lease", table, err)
		}
	}
	assertResourcesReleased(test, gate)
}

func TestPGWave40ProjectionWorkersRejectCurrentIdentityAndRepositoryDenial(test *testing.T) {
	for _, scenario := range []string{"current-identity", "repository-denial"} {
		for _, owner := range []string{"pending", "graph", "qualification"} {
			test.Run(scenario+"/"+owner, func(test *testing.T) {
				wave40IsolatedPG(test)
				_, storage, repo := collaborationPG(test)
				base, gate := admissionTestContext(time.Second)
				ctx, cancel := context.WithTimeout(wave40PGContext(base, storage), 45*time.Second)
				defer cancel()
				collaborationSnapshot(test, storage, repo, "wave40 authority source")
				wave40ConfigureWorkersPG(test, storage, repo)
				observed := &wave40WorkersPGStore{PostgresStore: storage, test: test, gate: gate}
				workers := wave40NewPGWorkers(test, observed, repo)
				if owner == "qualification" {
					wave40PublishGraphPG(test, ctx, workers, storage, repo)
				}
				if scenario == "current-identity" {
					limitedBlobs := struct{ outbound.BlobStore }{BlobStore: storage}
					var err error
					workers, err = NewProjectionWorkers(ProjectionWorkersConfig{Metadata: observed, Blobs: limitedBlobs,
						Profile: projectionworker.Complete, PendingEnabled: true, GraphEnabled: true,
						QualificationRepositories: []domain.ContentHash{repo}})
					if err != nil {
						test.Fatal(err)
					}
					if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
						test.Fatal(err)
					}
				} else {
					observed.denyRepository = true
				}
				process := workers.Pending.Process
				switch owner {
				case "graph":
					process = workers.Graph.Process
				case "qualification":
					process = workers.Qualification.Process
				}
				want := domain.ErrForbidden
				if scenario == "current-identity" {
					want = domain.ErrDocumentIdentityUpgradeRequired
				}
				if err := process(ctx, 1); !errors.Is(err, want) {
					test.Fatal("factory owner ignored current repository authority", owner, err)
				}
				if len(observed.pendingPublications) != 0 || len(observed.completed) != 0 || owner != "qualification" && len(observed.graphPublications) != 0 {
					test.Fatal("denied owner published output or completion evidence")
				}
				if scenario == "repository-denial" {
					failures := len(observed.pendingFailures) + len(observed.graphFailures) + len(observed.qualificationFails)
					reason := "access_denied"
					if owner == "qualification" {
						reason = "read_error"
					}
					if failures != 1 || !reflect.DeepEqual(observed.failureReasons, []string{reason}) {
						test.Fatal("denied work lost its bounded durable failure", observed.failureReasons)
					}
				}
				assertResourcesReleased(test, gate)
			})
		}
	}
}

func TestPGWave40ProjectionWorkersRejectStaleFencesForeignRepairAndCancellation(test *testing.T) {
	wave40IsolatedPG(test)
	_, storage, repo := collaborationPG(test)
	base, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithTimeout(wave40PGContext(base, storage), 90*time.Second)
	defer cancel()
	collaborationSnapshot(test, storage, repo, "wave40 fenced source")
	wave40ConfigureWorkersPG(test, storage, repo)
	observed := &wave40WorkersPGStore{PostgresStore: storage, test: test, gate: gate}
	workers := wave40NewPGWorkers(test, observed, repo)
	graphLease, err := storage.ClaimGraphProjection(ctx, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	pendingLease, err := storage.ClaimPendingProjection(ctx, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	observed.graphClaims = append(observed.graphClaims, graphLease)
	observed.pendingClaims = append(observed.pendingClaims, pendingLease)
	for _, configure := range []func(context.Context, domain.ContentHash, bool) error{storage.ConfigureGraphProjection, storage.ConfigurePendingProjection} {
		for _, enabled := range []bool{false, true} {
			if err := configure(ctx, repo, enabled); err != nil {
				test.Fatal(err)
			}
		}
	}
	for _, run := range []func(context.Context) error{
		func(bound context.Context) error { return workers.Graph.RunLease(bound, graphLease) },
		func(bound context.Context) error { return workers.Pending.RunLease(bound, pendingLease) },
	} {
		if err := admittedWork(ctx, run); !errors.Is(err, domain.ErrRefConflict) {
			test.Fatal("stale native lease crossed the original PG fence", err)
		}
		assertResourcesReleased(test, gate)
	}
	candidate := wave40PublishGraphPG(test, ctx, workers, storage, repo)
	qualificationLease, err := storage.ClaimGraphQualification(ctx, repo, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	observed.qualificationClaims = append(observed.qualificationClaims, qualificationLease)
	if _, err := workers.Qualification.Compare(ctx, qualificationLease); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("comparison accepted a caller context without an owned read snapshot", err)
	}
	for _, enabled := range []bool{false, true} {
		if err := storage.ConfigureGraphQualification(ctx, repo, enabled); err != nil {
			test.Fatal(err)
		}
	}
	if err := admittedWork(ctx, func(bound context.Context) error { return workers.Qualification.RunLease(bound, qualificationLease) }); !errors.Is(err, domain.ErrRefConflict) {
		test.Fatal("stale qualification published a receipt through a changed PG fence", err)
	}
	if _, err := storage.ReadGraphQualification(ctx, candidate.QualificationKey()); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("stale qualification fabricated completion", err)
	}
	foreign := hh("wave40 unrelated repository")
	failure := &domain.GraphProjectionCorruption{RepoID: foreign, Dataset: qualificationLease.Dataset,
		Applied: qualificationLease.Applied, Rules: qualificationLease.Rules, Cause: domain.ErrIntegrity}
	failure.Applied.RepoID = foreign
	if err := admittedWork(ctx, func(bound context.Context) error {
		return workers.Qualification.Repair(bound, qualificationLease, failure)
	}); err != nil {
		test.Fatal("foreign corruption was not ignored", err)
	}
	config, err := pgxpool.ParseConfig(os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	connection, err := pgx.ConnectConfig(ctx, config.ConnConfig)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close(context.Background())
	var repairs int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM graph_projection_repairs WHERE repo_id IN ($1,$2)`, string(repo), string(foreign)).Scan(&repairs); err != nil || repairs != 0 {
		test.Fatal("foreign repair escaped its original candidate identity", repairs, err)
	}
	stateSQL := `SELECT jsonb_build_object(
		'graph',(SELECT to_jsonb(work) FROM graph_projection_work work WHERE repo_id=$1),
		'pending',(SELECT to_jsonb(work) FROM pending_projection_work work WHERE repo_id=$1),
		'qualification',(SELECT to_jsonb(work) FROM graph_qualification work WHERE repo_id=$1),
		'source',(SELECT to_jsonb(source) FROM repository_graph_source_state source WHERE repo_id=$1))::text`
	var beforeCancellation string
	if err := connection.QueryRow(ctx, stateSQL, string(repo)).Scan(&beforeCancellation); err != nil {
		test.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	for _, process := range []func(context.Context, int) error{workers.Pending.Process, workers.Graph.Process, workers.Qualification.Process} {
		if err := process(canceled, 1); !errors.Is(err, context.Canceled) {
			test.Fatal("canceled native owner attempted useful work", err)
		}
	}
	for _, run := range []func(context.Context) error{
		func(bound context.Context) error { return workers.Graph.RunLease(bound, graphLease) },
		func(bound context.Context) error { return workers.Pending.RunLease(bound, pendingLease) },
		func(bound context.Context) error { return workers.Qualification.RunLease(bound, qualificationLease) },
	} {
		if err := run(canceled); !errors.Is(err, context.Canceled) {
			test.Fatal("canceled lease lost caller cancellation", err)
		}
	}
	if len(observed.completed) != 0 || len(observed.pendingPublications) != 0 {
		test.Fatal("stale or canceled worker fabricated completion evidence")
	}
	var afterCancellation string
	if err := connection.QueryRow(ctx, stateSQL, string(repo)).Scan(&afterCancellation); err != nil || afterCancellation != beforeCancellation {
		test.Fatal("cancellation mutated source, claim, lease, fence, or completion state", err)
	}
	assertResourcesReleased(test, gate)
}
