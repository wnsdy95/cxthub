package documentquery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type viewPreparationReadKey struct{}

type viewPreparationProbe struct {
	LegacyReader
	DocRangeReader
	repo, hash, base domain.ContentHash
	ready            bool
	state            string
	checks, headers  int
	ensures, bodies  int
	queued, active   int
	admissions       int
}

func (probe *viewPreparationProbe) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if repo != probe.repo || (hash != probe.hash && hash != probe.base) || ctx.Value(viewPreparationReadKey{}) != true {
		return domain.Snapshot{}, domain.ErrIntegrity
	}
	return domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1}, nil
}

func (probe *viewPreparationProbe) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	probe.bodies++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (probe *viewPreparationProbe) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	probe.checks++
	if repo != probe.repo || (hash != probe.hash && hash != probe.base) || ctx.Value(viewPreparationReadKey{}) != true {
		return false, domain.ErrIntegrity
	}
	return probe.ready, nil
}

func (probe *viewPreparationProbe) DocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocRangeIndex, error) {
	probe.headers++
	return domain.DocRangeIndex{}, domain.ErrDocRangeIndexUnavailable
}

func (probe *viewPreparationProbe) EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error {
	probe.ensures++
	return domain.ErrIntegrity
}

func (probe *viewPreparationProbe) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, _ time.Time) (domain.DocRangeIndexJob, error) {
	if repo != probe.repo || hash != probe.hash || ctx.Value(viewPreparationReadKey{}) == true {
		return domain.DocRangeIndexJob{}, domain.ErrIntegrity
	}
	probe.queued++
	return domain.DocRangeIndexJob{State: probe.state, Reason: "invalid_document"}, nil
}

func (*viewPreparationProbe) InReadOnlyTransaction(ctx context.Context) bool {
	return ctx.Value(viewPreparationReadKey{}) == true
}

func (*viewPreparationProbe) DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error) {
	return 0, domain.ErrIntegrity
}

func (probe *viewPreparationProbe) EnqueueDocRangeBoundary(ctx context.Context, repo, hash, base domain.ContentHash, _ time.Time) (domain.DocRangeIndexJob, error) {
	if repo != probe.repo || hash != probe.hash || base != probe.base || ctx.Value(viewPreparationReadKey{}) == true {
		return domain.DocRangeIndexJob{}, domain.ErrIntegrity
	}
	probe.queued++
	return domain.DocRangeIndexJob{State: "completed"}, nil
}

func (probe *viewPreparationProbe) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	probe.admissions++
	probe.active++
	return ctx, func() { probe.active-- }, nil
}

func (*viewPreparationProbe) AcquireBulk(context.Context) (context.Context, func(), error) {
	panic("readiness preparation must not acquire bulk resources")
}

type viewPreparationScope struct{}

func (viewPreparationScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (viewPreparationScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	if ctx.Value(viewPreparationReadKey{}) == true {
		return ctx, domain.ErrConflict
	}
	return ctx, ctx.Err()
}

func (viewPreparationScope) WithinDocumentRead(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(context.WithValue(ctx, viewPreparationReadKey{}, true))
}

func (scope viewPreparationScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.WithinDocumentRead(ctx, repo, read)
}

func newViewPreparationQueries(test *testing.T) (*Queries, *viewPreparationProbe) {
	test.Helper()
	probe := &viewPreparationProbe{repo: domain.HashContent([]byte("preparation-repo")), hash: domain.HashContent([]byte("preparation-document")), base: domain.HashContent([]byte("preparation-base"))}
	queries, err := New(Config{Profile: LegacyCompatibility, Scope: viewPreparationScope{}, Snapshots: probe, Legacy: probe,
		View: &ViewDependencies{Range: probe, LegacyPreparation: probe, RangeReadiness: probe, RangeJobs: probe, Boundaries: probe, Resources: probe}})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if probe.active != 0 || probe.admissions == 0 || probe.headers != 0 || probe.ensures != 0 || probe.bodies != 0 {
			test.Fatal("preparation leaked admission or read/built document data", probe)
		}
	})
	return queries, probe
}

func TestDocViewWarmReadinessDoesNotLoadHeaderOrBody(test *testing.T) {
	for _, state := range []string{"waiting", "running", "retrying", "rejected"} {
		test.Run(state, func(test *testing.T) {
			queries, probe := newViewPreparationQueries(test)
			probe.state = state
			ctx := context.Background()
			want := domain.ErrDocRangeIndexPending
			if state == "rejected" {
				want = domain.ErrIntegrity
			}
			if prepared, err := queries.prepareDocViewSource(ctx, probe.repo, probe.hash, docRangeLegacyBytes); !prepared || !errors.Is(err, want) || probe.queued != 1 {
				test.Fatal("cold preparation lost queue state", prepared, err, probe.queued)
			}
			if _, err := queries.prepareDocViewSource(outbound.WithDocReadOnly(ctx), probe.repo, probe.hash, docRangeLegacyBytes); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || probe.queued != 1 {
				test.Fatal("read-only preparation enqueued work", err, probe.queued)
			}
			probe.ready = true
			before := probe.checks
			if prepared, err := queries.prepareDocViewSource(ctx, probe.repo, probe.hash, docRangeLegacyBytes); !prepared || err != nil || probe.checks != before+1 || probe.queued != 1 {
				test.Fatal("warm preparation skipped readiness or enqueued work", prepared, err, probe.checks-before, probe.queued)
			}
		})
	}
}

func TestDocViewBoundaryPreparationDoesNotRetryTerminalSecondRead(test *testing.T) {
	for _, failure := range []error{domain.ErrIntegrity, domain.ErrNotFound, domain.ErrDocRangeIndexUnavailable, context.Canceled} {
		test.Run(failure.Error(), func(test *testing.T) {
			queries, probe := newViewPreparationQueries(test)
			probe.ready = true
			calls := 0
			_, err := preparedDocViewRead(context.Background(), queries, probe.repo, probe.hash, probe.base, func() (domain.DocViewPage, error) {
				calls++
				if calls == 1 {
					return domain.DocViewPage{}, errDocViewBoundaryNeedsPreparation
				}
				return domain.DocViewPage{}, failure
			})
			if !errors.Is(err, failure) || calls != 2 || probe.queued != 1 {
				test.Fatal("terminal second read retried or changed failure", err, calls, probe.queued)
			}
		})
	}
}

func TestDocViewPendingPairDeclaresDependenciesBeforeYield(test *testing.T) {
	for _, sample := range []struct {
		name                             string
		sourceFailure, baseFailure       error
		sourceUnprepared, baseUnprepared bool
		wantSources, wantBoundaries      int
		wantFailure                      error
	}{
		{name: "source-pending", sourceFailure: domain.ErrDocRangeIndexPending, wantSources: 2, wantBoundaries: 1, wantFailure: domain.ErrDocRangeIndexPending},
		{name: "base-pending", baseFailure: domain.ErrDocRangeIndexPending, wantSources: 2, wantBoundaries: 1, wantFailure: domain.ErrDocRangeIndexPending},
		{name: "both-pending", sourceFailure: domain.ErrDocRangeIndexPending, baseFailure: domain.ErrDocRangeIndexPending, wantSources: 2, wantBoundaries: 1, wantFailure: domain.ErrDocRangeIndexPending},
		{name: "source-corrupt", sourceFailure: domain.ErrIntegrity, wantSources: 1, wantFailure: domain.ErrIntegrity},
		{name: "base-corrupt", sourceFailure: domain.ErrDocRangeIndexPending, baseFailure: domain.ErrIntegrity, wantSources: 2, wantFailure: domain.ErrIntegrity},
		{name: "base-revoked", sourceFailure: domain.ErrDocRangeIndexPending, baseFailure: domain.ErrUnauthorized, wantSources: 2, wantFailure: domain.ErrUnauthorized},
		{name: "base-absent", sourceFailure: domain.ErrDocRangeIndexPending, baseFailure: domain.ErrNotFound, wantSources: 2, wantFailure: domain.ErrNotFound},
		{name: "source-not-durable", sourceFailure: domain.ErrDocRangeIndexPending, sourceUnprepared: true, wantSources: 2, wantFailure: domain.ErrDocRangeIndexPending},
		{name: "base-not-durable", sourceFailure: domain.ErrDocRangeIndexPending, baseUnprepared: true, wantSources: 2, wantFailure: domain.ErrDocRangeIndexPending},
	} {
		test.Run(sample.name, func(test *testing.T) {
			queries, probe := newViewPreparationQueries(test)
			var sources []domain.ContentHash
			prepare := func(ctx context.Context, repo, target domain.ContentHash) (bool, error) {
				sources = append(sources, target)
				failure, prepared := sample.sourceFailure, !sample.sourceUnprepared
				if target == probe.base {
					failure, prepared = sample.baseFailure, !sample.baseUnprepared
				}
				return prepared, queries.interactiveViewWork(ctx, func(context.Context) error { return failure })
			}
			reads := 0
			page, err := preparedDocumentProjectionRead(context.Background(), queries, probe.repo, probe.hash, probe.base, prepare, func() (domain.DocViewPage, error) {
				reads++
				return domain.DocViewPage{Hash: probe.hash}, nil
			})
			if !errors.Is(err, sample.wantFailure) || len(sources) != sample.wantSources || probe.queued != sample.wantBoundaries || reads != 0 || page.Hash != "" {
				test.Fatal("cold demand lost a dependency, hid a terminal error, or read unfinished data", sources, probe.queued, reads, page, err)
			}
		})
	}
}
