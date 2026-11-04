package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyViewPreparationStore struct {
	*legacyFindGenerationStore
	outbound.DocRangeJobStore
	ready          bool
	readinessError error
	checks         int
	queued         int
	enqueue        func(context.Context) (domain.DocRangeIndexJob, error)
}

func (storage *legacyViewPreparationStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	storage.checks++
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if repo != storage.repo || storage.fixtures[hash] == nil {
		return false, domain.ErrNotFound
	}
	return storage.ready, storage.readinessError
}

func (storage *legacyViewPreparationStore) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.queued++
	if repo != storage.repo || storage.fixtures[hash] == nil || now.IsZero() {
		return domain.DocRangeIndexJob{}, domain.ErrIntegrity
	}
	return storage.enqueue(ctx)
}

func assertLegacyViewPreparationNoBody(test *testing.T, storage *legacyViewPreparationStore) {
	test.Helper()
	cost := storage.cost
	if cost.opens != 0 || cost.pages != 0 || cost.indexes != 0 || cost.whole != 0 || cost.manifests != 0 || cost.chunks != 0 {
		test.Fatal("cold preparation opened original bytes or enumerated history", cost)
	}
}

func TestDocViewLegacyColdDemandQueuesWithoutOpeningHistory(test *testing.T) {
	for _, state := range []string{"waiting", "running", "retrying", "rejected"} {
		test.Run(state, func(test *testing.T) {
			storage := &legacyViewPreparationStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test)}
			document := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "cold legacy body")})
			storage.enqueue = func(context.Context) (domain.DocRangeIndexJob, error) {
				return domain.DocRangeIndexJob{State: state, Reason: "invalid_document"}, nil
			}
			service := NewService(storage, storage, nil, nil, nil)
			want := domain.ErrDocRangeIndexPending
			if state == "rejected" {
				want = domain.ErrIntegrity
			}
			page, err := service.ReadDocViewEvents(systemTestContext(), storage.repo, document.Hash, "", 0, 1, "chat")
			if !errors.Is(err, want) || len(page.Events) != 0 || storage.queued != 1 || storage.checks != 1 {
				test.Fatal("cold body hid durable work state", page, err, storage.queued, storage.checks)
			}
			fragment, err := service.ReadDocViewFragment(systemTestContext(), storage.repo, document.Hash, 0, 0, 64)
			if !errors.Is(err, want) || fragment != (domain.DocViewFragment{}) || storage.queued != 2 || storage.checks != 2 {
				test.Fatal("cold fragment bypassed durable work", fragment, err, storage.queued, storage.checks)
			}
			assertLegacyViewPreparationNoBody(test, storage)
		})
	}
}

func TestDocViewLegacyColdReadOnlyAndFailuresNeverQueueOrFallback(test *testing.T) {
	for _, failure := range []error{nil, domain.ErrIntegrity, domain.ErrForbidden, domain.ErrConflict, domain.ErrNotFound, context.Canceled, context.DeadlineExceeded} {
		name := "read-only"
		if failure != nil {
			name = failure.Error()
		}
		test.Run(name, func(test *testing.T) {
			storage := &legacyViewPreparationStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test), readinessError: failure}
			document := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "read-only cold legacy")})
			service := NewService(storage, storage, nil, nil, nil)
			want := failure
			if want == nil {
				want = domain.ErrDocRangeIndexUnavailable
			}
			ctx := outbound.WithDocReadOnly(systemTestContext())
			if page, err := service.ReadDocViewEvents(ctx, storage.repo, document.Hash, "", 0, 1, "chat"); !errors.Is(err, want) || len(page.Events) != 0 {
				test.Fatal("cold read-only body changed error or returned content", page, err)
			}
			if fragment, err := service.ReadDocViewFragment(ctx, storage.repo, document.Hash, 0, 0, 64); !errors.Is(err, want) || fragment != (domain.DocViewFragment{}) {
				test.Fatal("cold read-only fragment changed error or returned content", fragment, err)
			}
			if storage.queued != 0 {
				test.Fatal("read-only demand scheduled work", storage.queued)
			}
			assertLegacyViewPreparationNoBody(test, storage)
		})
	}
}

func TestDocViewLegacyReauthorizesPreparationQueueAndReadyReads(test *testing.T) {
	for _, fragment := range []bool{false, true} {
		for _, phase := range []string{"prepare", "enqueue", "read"} {
			test.Run(fmt.Sprintf("%s/fragment=%t", phase, fragment), func(test *testing.T) {
				storage := &legacyViewPreparationStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test), ready: phase == "read"}
				document := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "authorization scoped legacy")})
				calls := 0
				ctx := inbound.WithDocumentReadAuthorizer(systemTestContext(), func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
					calls++
					if repo != storage.repo || phase == "prepare" || calls == 2 {
						return bound, domain.ErrForbidden
					}
					return bound, nil
				})
				service := NewService(storage, storage, nil, nil, nil)
				var err error
				if fragment {
					_, err = service.ReadDocViewFragment(ctx, storage.repo, document.Hash, 0, 0, 64)
				} else {
					_, err = service.ReadDocViewEvents(ctx, storage.repo, document.Hash, "", 0, 1, "chat")
				}
				wantCalls := 2
				if phase == "prepare" {
					wantCalls = 1
				}
				if !errors.Is(err, domain.ErrForbidden) || calls != wantCalls || storage.queued != 0 {
					test.Fatal("revoked access crossed preparation or read boundary", err, calls, storage.queued)
				}
				assertLegacyViewPreparationNoBody(test, storage)
			})
		}
	}
}

func TestDocViewLegacyReadySourceDisappearsWithoutCompatibility(test *testing.T) {
	storage := &legacyViewPreparationStore{legacyFindGenerationStore: newLegacyFindGenerationStore(test), ready: true}
	document := storage.seed(test, []domain.CIREvent{historyMessage(domain.RoleUser, "invalidated ready source")})
	storage.fixtures[document.Hash].failure = domain.ErrDocRangeIndexUnavailable
	service := NewService(storage, storage, nil, nil, nil)
	if _, err := service.ReadDocViewEvents(systemTestContext(), storage.repo, document.Hash, "", 0, 1, "chat"); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("disappearing generation changed preparation contract", err)
	}
	if storage.cost.opens != 2 || storage.cost.indexes != 0 || storage.cost.whole != 0 || storage.cost.manifests != 0 || storage.cost.chunks != 0 || storage.queued != 0 {
		test.Fatal("disappearing ready generation selected full-history compatibility", storage.cost, storage.queued)
	}
}
