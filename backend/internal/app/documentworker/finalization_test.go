package documentworker_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type finalizationWorkerKey struct{}
type finalizationWorkKey struct{}
type finalizationReadKey struct{}
type finalizationCallerKey struct{}

type finalizationScope struct {
	workerCalls int
	workCalls   int
	readCalls   int
	withinWork  func(context.Context, func(context.Context) error) error
	withinRead  func(context.Context, domain.ContentHash, func(context.Context) error) error
	canYield    bool
	yield       func(context.Context, outbound.RootPublicationResources) error
}

func (scope *finalizationScope) WorkerContext(ctx context.Context) context.Context {
	scope.workerCalls++
	return context.WithValue(ctx, finalizationWorkerKey{}, scope)
}

func (scope *finalizationScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	scope.workCalls++
	ctx = context.WithValue(ctx, finalizationWorkKey{}, scope)
	if scope.withinWork != nil {
		return scope.withinWork(ctx, work)
	}
	return work(ctx)
}

func (scope *finalizationScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	scope.readCalls++
	ctx = context.WithValue(ctx, finalizationReadKey{}, repo)
	if scope.withinRead != nil {
		return scope.withinRead(ctx, repo, read)
	}
	return read(ctx)
}

func (scope *finalizationScope) CanYield(context.Context) bool {
	return scope.canYield
}

func (scope *finalizationScope) Yield(ctx context.Context, resources outbound.RootPublicationResources) error {
	return scope.yield(ctx, resources)
}

type finalizationChunks struct {
	get func(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error)
}

func (chunks finalizationChunks) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	return chunks.get(ctx, repo, hash)
}

type finalizationConversationChunks struct {
	finalizationChunks
	read func(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error)
}

func (chunks finalizationConversationChunks) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	return chunks.read(ctx, repo, hash, length)
}

type finalizationStore struct {
	outbound.DocJobStore
	claim   func(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.DocFinalizationJob, error)
	prepare func(context.Context, domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error)
	renew   func(context.Context, domain.DocFinalizationJob, time.Time, time.Duration) error
	finish  func(context.Context, domain.DocFinalizationJob, time.Time) error
}

func (storage *finalizationStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	return storage.claim(ctx, repo, now, lease)
}

func (storage *finalizationStore) PrepareDocJob(ctx context.Context, verified domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
	return storage.prepare(ctx, verified)
}

func (storage *finalizationStore) RenewDocJob(ctx context.Context, job domain.DocFinalizationJob, now time.Time, lease time.Duration) error {
	return storage.renew(ctx, job, now, lease)
}

func (storage *finalizationStore) FinishDocJob(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	return storage.finish(ctx, job, now)
}

type finalizationStreamingStore struct {
	*finalizationStore
	root   func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error)
	legacy func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error)
}

func (storage finalizationStreamingStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	return storage.root(ctx, job)
}

func (storage finalizationStreamingStore) PrepareLegacyDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	return storage.legacy(ctx, job)
}

type finalizationPublication struct {
	resources func() (outbound.RootPublicationResources, error)
	stage     func(context.Context, domain.DocFinalizationJob, time.Duration) error
	complete  func(context.Context, domain.DocFinalizationJob, time.Time) error
	close     func() error
}

func (publication *finalizationPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	return publication.resources()
}

func (publication *finalizationPublication) Stage(ctx context.Context, job domain.DocFinalizationJob, lease time.Duration) error {
	if publication.stage != nil {
		return publication.stage(ctx, job, lease)
	}
	return nil
}

func (publication *finalizationPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	return publication.complete(ctx, job, now)
}

func (publication *finalizationPublication) Close() error {
	if publication.close != nil {
		return publication.close()
	}
	return nil
}

func finalizationFixture(test *testing.T, identity domain.DocumentIdentity) (domain.DocFinalizationJob, map[domain.ContentHash][]byte) {
	test.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: test.Name()},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: "concrete finalizer"}}}},
	}
	repo := domain.HashContent([]byte(test.Name()))
	var representation domain.DocumentRepresentation
	var bodies map[domain.ContentHash][]byte
	if identity == domain.DocumentIdentityRootV1 {
		manifest, chunks, err := domain.ConversationManifestForCIR(cir)
		if err != nil {
			test.Fatal(err)
		}
		hash, err := domain.ConversationManifestHash(manifest)
		if err != nil {
			test.Fatal(err)
		}
		raw, err := domain.CanonicalConversationManifest(manifest)
		if err != nil {
			test.Fatal(err)
		}
		representation = domain.DocumentRepresentation{Hash: hash, Identity: identity, RootManifest: raw}
		bodies = chunks
	} else {
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			test.Fatal(err)
		}
		plan, ok := domain.PlanDocChunks(raw)
		if !ok || plan.Manifest.Format != domain.ChunkFormatV2 {
			test.Fatal("fixture did not produce a v2 chunk plan")
		}
		representation = domain.DocumentRepresentation{Hash: domain.HashContent(raw), Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
		bodies = plan.Bodies
	}
	job, err := domain.NewDocFinalizationJobForRepresentation(repo, representation, time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	return job.Claim(time.Now().UTC(), documentworker.Lease), bodies
}

func newFinalizationOwner(test *testing.T, storage outbound.DocJobStore, chunks documentworker.ChunkReader, scope documentworker.Scope) *documentworker.Finalizer {
	test.Helper()
	owner, err := documentworker.NewFinalizer(storage, chunks, &domain.CanonicalDocVerifier{}, scope)
	if err != nil {
		test.Fatal(err)
	}
	return owner
}

func assertFinalizationDeadline(test *testing.T, ctx context.Context, maximum time.Duration) {
	test.Helper()
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > maximum {
		test.Error("operation lost its deadline", deadline, maximum)
	}
}

func TestFinalizationOwnerConstructionKeepsDependenciesBranchLocal(test *testing.T) {
	scope := &finalizationScope{}
	owner := newFinalizationOwner(test, nil, nil, scope)
	if owner == nil || scope.workerCalls != 0 || scope.workCalls != 0 || scope.readCalls != 0 {
		test.Fatal("construction performed worker activity", scope)
	}
	var nilScope *finalizationScope
	for _, missing := range []documentworker.Scope{nil, nilScope} {
		if owner, err := documentworker.NewFinalizer(nil, nil, &domain.CanonicalDocVerifier{}, missing); owner != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("missing scope accepted", owner, err)
		}
	}
	if owner, err := documentworker.NewFinalizer(nil, nil, nil, scope); owner != nil || !errors.Is(err, domain.ErrValidation) {
		test.Fatal("missing verifier accepted", owner, err)
	}
}

func TestFinalizationOwnerVerifyRereadsCurrentRepositoryChunks(test *testing.T) {
	job, bodies := finalizationFixture(test, domain.DocumentIdentityLegacy)
	reads, corrupt := 0, false
	chunks := finalizationChunks{get: func(_ context.Context, repo, hash domain.ContentHash) ([]byte, error) {
		reads++
		if repo != job.RepoID {
			return nil, domain.ErrNotFound
		}
		raw := append([]byte(nil), bodies[hash]...)
		if corrupt {
			raw[0] ^= 1
		}
		return raw, nil
	}}
	owner := newFinalizationOwner(test, nil, chunks, &finalizationScope{})
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := owner.Verify(context.Background(), job)
		if err != nil || !proof.Valid() || proof.Hash() != job.DocHash {
			test.Fatal("valid job verification failed", proof, err)
		}
	}
	if reads != 2*len(job.Manifest.Chunks) {
		test.Fatal("warm semantic proof skipped current reads", reads)
	}
	foreign, err := domain.NewDocFinalizationJob(domain.HashContent([]byte("foreign")), job.DocHash, job.Manifest, time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	if proof, err := owner.Verify(context.Background(), foreign); !errors.Is(err, domain.ErrNotFound) || proof.Valid() {
		test.Fatal("warm proof granted foreign ownership", proof, err)
	}
	corrupt = true
	if proof, err := owner.Verify(context.Background(), job); !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
		test.Fatal("warm proof hid current corruption", proof, err)
	}
	invalid := job
	invalid.ID = "invalid"
	before := reads
	if proof, err := owner.Verify(context.Background(), invalid); !errors.Is(err, domain.ErrValidation) || proof.Valid() || reads != before {
		test.Fatal("invalid job reached chunk loading", proof, err, reads)
	}
}

func TestFinalizationOwnerRootVerifyRequiresBoundedCurrentReader(test *testing.T) {
	job, bodies := finalizationFixture(test, domain.DocumentIdentityRootV1)
	legacy := finalizationChunks{get: func(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
		test.Error("root verification fell back to legacy loading")
		return nil, domain.ErrIntegrity
	}}
	owner := newFinalizationOwner(test, nil, legacy, &finalizationScope{})
	if proof, err := owner.Verify(context.Background(), job); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || proof.Valid() {
		test.Fatal("root accepted an unbounded reader", proof, err)
	}
	var failure error
	reads := 0
	chunks := finalizationConversationChunks{finalizationChunks: legacy, read: func(_ context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
		reads++
		if repo != job.RepoID || length != int64(len(bodies[hash])) || length <= 0 {
			test.Error("root reader lost ownership or declared-length bound", repo, hash, length)
		}
		if failure != nil {
			return nil, failure
		}
		return append([]byte(nil), bodies[hash]...), nil
	}}
	owner = newFinalizationOwner(test, nil, chunks, &finalizationScope{})
	if proof, err := owner.Verify(context.Background(), job); err != nil || !proof.Valid() {
		test.Fatal("bounded root verification failed", proof, err)
	}
	for _, outcome := range []error{domain.ErrNotFound, domain.ErrIntegrity, context.Canceled} {
		failure = outcome
		before := reads
		if proof, err := owner.Verify(context.Background(), job); !errors.Is(err, outcome) || proof.Valid() || reads <= before {
			test.Fatal("warm root proof hid current read failure", outcome, proof, err)
		}
	}
}

func TestFinalizationOwnerStreamingPreparationOrdering(test *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy} {
		test.Run(string(identity), func(test *testing.T) {
			job, _ := finalizationFixture(test, identity)
			var order []string
			resources := outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}
			scope := &finalizationScope{canYield: true}
			scope.withinRead = func(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
				order = append(order, "read")
				if repo != job.RepoID {
					test.Error("preparation read changed repository", repo)
				}
				err := read(ctx)
				order = append(order, "read-exit")
				return err
			}
			scope.yield = func(ctx context.Context, got outbound.RootPublicationResources) error {
				order = append(order, "yield")
				if ctx.Value(finalizationReadKey{}) != nil || got != resources {
					test.Error("yield retained a read snapshot or changed resource evidence", got)
				}
				return nil
			}
			publication := &finalizationPublication{
				resources: func() (outbound.RootPublicationResources, error) {
					order = append(order, "resources")
					return resources, nil
				},
				stage: func(ctx context.Context, got domain.DocFinalizationJob, lease time.Duration) error {
					order = append(order, "stage")
					assertFinalizationDeadline(test, ctx, documentworker.WorkLimit)
					if ctx.Value(finalizationReadKey{}) != nil || got.ID != job.ID || got.Version != job.Version || lease != documentworker.Lease {
						test.Error("staging changed snapshot, job fence, or lease")
					}
					return nil
				},
				complete: func(ctx context.Context, got domain.DocFinalizationJob, now time.Time) error {
					order = append(order, "complete")
					if ctx.Value(finalizationReadKey{}) != nil || got.ID != job.ID || got.Version != job.Version || now.Location() != time.UTC {
						test.Error("completion changed snapshot, fence, or UTC time")
					}
					return nil
				},
				close: func() error { order = append(order, "close"); return nil },
			}
			prepare := func(ctx context.Context, got domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
				if ctx.Value(finalizationReadKey{}) != job.RepoID || got.ID != job.ID {
					test.Error("stream preparation escaped its repository read")
				}
				order = append(order, "prepare")
				return publication, nil
			}
			storage := finalizationStreamingStore{finalizationStore: &finalizationStore{}}
			if identity == domain.DocumentIdentityRootV1 {
				storage.root = prepare
			} else {
				storage.legacy = prepare
			}
			owner := newFinalizationOwner(test, storage, nil, scope)
			if err := owner.RunJob(context.Background(), job); err != nil {
				test.Fatal(err)
			}
			want := []string{"read", "prepare", "read-exit"}
			if identity == domain.DocumentIdentityRootV1 {
				want = append(want, "resources", "yield")
			}
			want = append(want, "stage", "complete", "close")
			if !slices.Equal(order, want) || scope.workerCalls != 0 || scope.workCalls != 0 || scope.readCalls != 1 {
				test.Fatal("direct job changed preparation, scope, or publication order", order, want, scope)
			}
		})
	}
}

func TestFinalizationOwnerFallbackPreparesOutsideReadSnapshot(test *testing.T) {
	job, bodies := finalizationFixture(test, domain.DocumentIdentityLegacy)
	var claimContext context.Context
	claims, prepares, completes, closes := 0, 0, 0, 0
	scope := &finalizationScope{}
	chunks := finalizationChunks{get: func(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
		if ctx.Value(finalizationReadKey{}) != job.RepoID || repo != job.RepoID || ctx.Value(finalizationWorkKey{}) != scope {
			test.Error("fallback verification escaped admitted repository read")
		}
		return bodies[hash], nil
	}}
	publication := &finalizationPublication{
		complete: func(ctx context.Context, got domain.DocFinalizationJob, _ time.Time) error {
			completes++
			if ctx.Value(finalizationReadKey{}) != nil || ctx.Value(finalizationWorkKey{}) != scope || got.Version != job.Version {
				test.Error("fallback completion lost its work scope or fence")
			}
			return nil
		},
		close: func() error { closes++; return nil },
	}
	storage := &finalizationStore{
		claim: func(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
			claims++
			assertFinalizationDeadline(test, ctx, 5*time.Second)
			if ctx.Value(finalizationWorkerKey{}) != scope || ctx.Value(finalizationWorkKey{}) != scope || repo != "" || lease != documentworker.Lease || now.Location() != time.UTC {
				test.Error("claim lost worker admission, global selection, lease, or UTC time")
			}
			if claims > 1 {
				return domain.DocFinalizationJob{}, domain.ErrNotFound
			}
			claimContext = ctx
			return job, nil
		},
		prepare: func(ctx context.Context, proof domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
			prepares++
			assertFinalizationDeadline(test, ctx, documentworker.WorkLimit)
			if ctx.Value(finalizationReadKey{}) != nil || ctx.Value(finalizationWorkKey{}) != scope || !proof.Valid() || proof.Hash() != job.DocHash || !errors.Is(claimContext.Err(), context.Canceled) {
				test.Error("fallback preparation changed verification, snapshot, claim lifetime, or admission")
			}
			return publication, nil
		},
	}
	owner := newFinalizationOwner(test, storage, chunks, scope)
	if err := owner.Process(context.Background(), 3); err != nil {
		test.Fatal(err)
	}
	if claims != 2 || prepares != 1 || completes != 1 || closes != 0 || scope.workerCalls != 1 || scope.workCalls != 2 || scope.readCalls != 1 {
		test.Fatal("fallback processing changed claim limit, idle handling, or legacy cleanup", claims, prepares, completes, closes, scope)
	}
}

func TestFinalizationOwnerFailureReceiptAndPreparedCleanup(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		failure error
		state   string
		reason  string
	}{
		{name: "temporary", failure: errors.New("prepare failed"), state: "retrying", reason: "temporary_failure"},
		{name: "missing", failure: domain.ErrNotFound, state: "retrying", reason: "temporary_failure"},
		{name: "resource", failure: domain.ErrDocumentResourceLimit, state: "retrying", reason: "resource_limit"},
		{name: "integrity", failure: domain.ErrIntegrity, state: "rejected", reason: "invalid_document"},
		{name: "validation", failure: domain.ErrValidation, state: "rejected", reason: "invalid_document"},
		{name: "version", failure: domain.ErrUnsupportedCIRVersion, state: "rejected", reason: "invalid_document"},
		{name: "manifest", failure: domain.ErrConversationManifest, state: "rejected", reason: "invalid_document"},
		{name: "identity", failure: domain.ErrUnsupportedDocumentIdentity, state: "rejected", reason: "invalid_document"},
		{name: "invalid-resource", failure: errors.Join(domain.ErrDocumentResourceLimit, domain.ErrIntegrity), state: "rejected", reason: "invalid_document"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			job, _ := finalizationFixture(test, domain.DocumentIdentityRootV1)
			job.Attempts = 12
			cleanupFailure, finishFailure := errors.New("cleanup failed"), errors.New("finish failed")
			var order []string
			publication := &finalizationPublication{close: func() error { order = append(order, "close"); return cleanupFailure }}
			storage := finalizationStreamingStore{
				finalizationStore: &finalizationStore{finish: func(ctx context.Context, got domain.DocFinalizationJob, now time.Time) error {
					order = append(order, "finish")
					assertFinalizationDeadline(test, ctx, 5*time.Second)
					if ctx.Err() != nil || ctx.Value(finalizationCallerKey{}) != "caller" || got.ID != job.ID || got.RepoID != job.RepoID || got.DocHash != job.DocHash || got.DocIdentity != job.DocIdentity || string(got.RootManifest) != string(job.RootManifest) || got.Version != job.Version || got.Attempts != job.Attempts {
						test.Error("failure receipt changed context, document identity, or fence")
					}
					if got.State != fixture.state || got.Reason != fixture.reason || !got.LeaseUntil.IsZero() || !got.UpdatedAt.Equal(now) || !got.NextAttempt.Equal(now.Add(256*time.Second)) {
						test.Error("failure classification, cleared lease, or capped backoff changed", got)
					}
					return finishFailure
				}},
				root: func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
					return publication, fmt.Errorf("preparation: %w", fixture.failure)
				},
			}
			owner := newFinalizationOwner(test, storage, nil, &finalizationScope{})
			err := owner.RunJob(context.WithValue(context.Background(), finalizationCallerKey{}, "caller"), job)
			if !errors.Is(err, fixture.failure) || !errors.Is(err, cleanupFailure) || !errors.Is(err, finishFailure) || !slices.Equal(order, []string{"finish", "close"}) {
				test.Fatal("preparation failure lost cleanup or finish errors/order", err, order)
			}
		})
	}
}

func TestFinalizationOwnerYieldSkipsIneligibleOrUnboundedPlans(test *testing.T) {
	failure := errors.New("resource evidence failed")
	resources := outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128, KeepPermit: true}
	for _, fixture := range []struct {
		name     string
		eligible bool
		bounded  bool
		failure  error
	}{
		{name: "ineligible", bounded: true},
		{name: "unbounded", eligible: true},
		{name: "resource-error", eligible: true, bounded: true, failure: failure},
		{name: "bounded", eligible: true, bounded: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			reads, yields := 0, 0
			scope := &finalizationScope{canYield: fixture.eligible, yield: func(_ context.Context, got outbound.RootPublicationResources) error {
				yields++
				if got != resources {
					test.Error("yield changed resource evidence", got)
				}
				return nil
			}}
			publication := &finalizationPublication{resources: func() (outbound.RootPublicationResources, error) {
				reads++
				return resources, fixture.failure
			}}
			var plan outbound.PreparedDocPublication = publication
			if !fixture.bounded {
				plan = struct {
					outbound.PreparedRootDocPublication
				}{publication}
			}
			owner := newFinalizationOwner(test, nil, nil, scope)
			err := owner.YieldRootPreparation(context.Background(), plan)
			wantReads, wantYields := 0, 0
			if fixture.eligible && fixture.bounded {
				wantReads = 1
				if fixture.failure == nil {
					wantYields = 1
				}
			}
			if !errors.Is(err, fixture.failure) || reads != wantReads || yields != wantYields {
				test.Fatal("yield eligibility or resource error changed", err, reads, yields)
			}
		})
	}
}

func TestFinalizationOwnerProcessPreservesMissingStoreAndLimitPrecedence(test *testing.T) {
	for _, limit := range []int{-1, 0, 1} {
		scope := &finalizationScope{}
		owner := newFinalizationOwner(test, nil, nil, scope)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := owner.Process(ctx, limit); err == nil || err.Error() != "durable document finalization unavailable" || scope.workerCalls != 1 || scope.workCalls != 0 {
			test.Fatal("missing storage lost precedence over limit/cancellation", limit, err, scope)
		}
		if limit <= 0 {
			owner = newFinalizationOwner(test, &finalizationStore{}, nil, scope)
			if err := owner.Process(outbound.WithDocReadOnly(ctx), limit); err != nil || scope.workCalls != 0 {
				test.Fatal("nonpositive limit acquired work or introduced a new rejection", limit, err, scope)
			}
		}
	}
}

func TestFinalizationOwnerProcessDistinguishesIdleFromClaimedFailure(test *testing.T) {
	job, _ := finalizationFixture(test, domain.DocumentIdentityRootV1)
	for _, claimed := range []bool{false, true} {
		finishes := 0
		storage := finalizationStreamingStore{
			finalizationStore: &finalizationStore{
				claim: func(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.DocFinalizationJob, error) {
					if claimed {
						return job, nil
					}
					return domain.DocFinalizationJob{}, domain.ErrNotFound
				},
				finish: func(context.Context, domain.DocFinalizationJob, time.Time) error { finishes++; return nil },
			},
			root: func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
				return nil, domain.ErrNotFound
			},
		}
		owner := newFinalizationOwner(test, storage, nil, &finalizationScope{})
		err := owner.Process(context.Background(), 1)
		if claimed && (!errors.Is(err, domain.ErrNotFound) || finishes != 1) || !claimed && (err != nil || finishes != 0) {
			test.Fatal("claimed failure was confused with idle claim", claimed, err, finishes)
		}
	}
	scope := &finalizationScope{withinWork: func(context.Context, func(context.Context) error) error { return domain.ErrResourceOverloaded }}
	owner := newFinalizationOwner(test, &finalizationStore{}, nil, scope)
	if err := owner.Process(context.Background(), 1); !errors.Is(err, domain.ErrResourceOverloaded) || scope.readCalls != 0 {
		test.Fatal("admission failure reached claiming or preparation", err, scope)
	}
}

func TestFinalizationOwnerClaimCancellationAndWorkerCadence(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		var claimContext context.Context
		storage := &finalizationStore{claim: func(ctx context.Context, _ domain.ContentHash, _ time.Time, _ time.Duration) (domain.DocFinalizationJob, error) {
			claimContext = ctx
			<-ctx.Done()
			return domain.DocFinalizationJob{}, ctx.Err()
		}}
		owner := newFinalizationOwner(test, storage, nil, &finalizationScope{})
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if err := owner.Process(ctx, 1); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(claimContext.Err(), context.DeadlineExceeded) {
			test.Fatal("blocked claim ignored caller cancellation", err, claimContext)
		}
	})
	synctest.Test(test, func(test *testing.T) {
		var claims atomic.Int64
		storage := &finalizationStore{claim: func(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.DocFinalizationJob, error) {
			claims.Add(1)
			return domain.DocFinalizationJob{}, domain.ErrNotFound
		}}
		owner := newFinalizationOwner(test, storage, nil, &finalizationScope{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); owner.Run(ctx) }()
		synctest.Wait()
		if count := claims.Load(); count != 1 {
			test.Fatal("worker did not process immediately", count)
		}
		time.Sleep(time.Second - time.Nanosecond)
		synctest.Wait()
		if count := claims.Load(); count != 1 {
			test.Fatal("worker bypassed its one-second delay", count)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if count := claims.Load(); count != 2 {
			test.Fatal("worker changed polling cadence", count)
		}
		cancel()
		<-done
	})
}
