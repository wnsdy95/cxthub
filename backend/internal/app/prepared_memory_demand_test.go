package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type preparedMemoryDemandProbe struct {
	pendingPreparedMemoryStore
	requested []domain.MemoryReadRequirement
	failure   error
	denied    bool
}

func (probe *preparedMemoryDemandProbe) ObserveMemoryReadRequirement(ctx context.Context, target domain.MemoryReadTarget, authorize func(context.Context) (context.Context, error)) (domain.MemoryReadRequirement, error) {
	if probe.denied && probe.observed > 0 {
		return domain.MemoryReadRequirement{}, domain.ErrForbidden
	}
	return probe.pendingPreparedMemoryStore.ObserveMemoryReadRequirement(ctx, target, authorize)
}

func (probe *preparedMemoryDemandProbe) RequestMinimumMemoryReadWork(ctx context.Context, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement) (domain.MemoryReadWork, error) {
	if err := requirement.Validate(target); err != nil {
		return domain.MemoryReadWork{}, err
	}
	probe.requested = append(probe.requested, requirement)
	return domain.MemoryReadWork{State: "waiting"}, probe.failure
}

func TestPreparedMemoryDemandRegistersFixedFloorOnlyWhenEnabled(test *testing.T) {
	target, original := preparedCursorFixture(test)
	probe := &preparedMemoryDemandProbe{pendingPreparedMemoryStore: pendingPreparedMemoryStore{requirement: original.Requirement}}
	service := &Service{meta: probe}
	request := domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	_, err := service.QueryPreparedMemory(systemTestContext(), target.RepoID, request)
	var pending *domain.MemoryReadPending
	if !errors.As(err, &pending) || len(probe.requested) != 0 {
		test.Fatal("default query unexpectedly registered demand", err)
	}
	if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	request.Cursor = pending.Cursor
	for range 3 {
		_, err := service.QueryPreparedMemory(systemTestContext(), target.RepoID, request)
		if !errors.As(err, &pending) || pending.Cursor != request.Cursor {
			test.Fatal("registration changed retry floor", err)
		}
	}
	if len(probe.requested) != 3 || probe.observed != 4 {
		test.Fatal("demand did not reauthorize in a fresh observation", len(probe.requested), probe.observed)
	}
	for _, requirement := range probe.requested {
		if requirement != original.Requirement {
			test.Fatal("minimum was replaced by preparation observation")
		}
	}
	if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{target.RepoID, "invalid"}); err == nil || !service.preparedMemoryDemands[target.RepoID] {
		test.Fatal("invalid configuration partially changed demand scope", err)
	}
	if err := service.ConfigurePreparedMemoryDemands(nil); err != nil {
		test.Fatal(err)
	}
	_, err = service.QueryPreparedMemory(systemTestContext(), target.RepoID, request)
	if !errors.As(err, &pending) || len(probe.requested) != 3 {
		test.Fatal("disabled demand still enqueued", err)
	}
	if err := (&Service{}).ConfigurePreparedMemoryDemands([]domain.ContentHash{target.RepoID}); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("demand enabled without narrow port", err)
	}
}

func TestPreparedMemoryDemandNeverMasksFailureOrRepairsPinnedReads(test *testing.T) {
	for _, scenario := range []string{"denied", "readonly", "pinned", "corrupt", "queue_full", "epoch_reset"} {
		test.Run(scenario, func(test *testing.T) {
			target, original := preparedCursorFixture(test)
			probe := &preparedMemoryDemandProbe{pendingPreparedMemoryStore: pendingPreparedMemoryStore{requirement: original.Requirement}}
			service := &Service{meta: probe}
			if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
				test.Fatal(err)
			}
			if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{target.RepoID}); err != nil {
				test.Fatal(err)
			}
			request := domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content}
			ctx := systemTestContext()
			var expected error
			attempts := 0
			switch scenario {
			case "denied":
				probe.denied, expected = true, domain.ErrForbidden
			case "readonly":
				ctx, expected = outbound.WithDocReadOnly(ctx), domain.ErrCatalogUnsupported
			case "pinned":
				var err error
				request.Cursor, err = encodePreparedMemoryCursor(original)
				if err != nil {
					test.Fatal(err)
				}
				expected = domain.ErrEffectiveMemoryCursorStale
			case "corrupt":
				probe.pendingPreparedMemoryStore.failure = errors.Join(domain.ErrMemoryReadNotReady, domain.ErrIntegrity)
				expected = domain.ErrIntegrity
			case "queue_full":
				probe.failure, expected, attempts = domain.ErrResourceOverloaded, domain.ErrResourceOverloaded, 1
			case "epoch_reset":
				probe.failure, expected, attempts = domain.ErrCatalogResetRequired, domain.ErrCatalogResetRequired, 1
			}
			_, err := service.QueryPreparedMemory(ctx, target.RepoID, request)
			var pending *domain.MemoryReadPending
			if !errors.Is(err, expected) || errors.As(err, &pending) || len(probe.requested) != attempts {
				test.Fatal("failure was masked or caused repair", err, len(probe.requested))
			}
		})
	}
}

func TestPreparedMemoryDemandPreservesCallerAuthority(test *testing.T) {
	target, original := preparedCursorFixture(test)
	probe := &preparedMemoryDemandProbe{pendingPreparedMemoryStore: pendingPreparedMemoryStore{requirement: original.Requirement}}
	service := &Service{meta: probe}
	if err := service.ConfigurePreparedMemoryReads([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	if err := service.ConfigurePreparedMemoryDemands([]domain.ContentHash{target.RepoID}); err != nil {
		test.Fatal(err)
	}
	checks, generalChecks := 0, 0
	ctx := inbound.WithRepositoryActor(context.Background(), "reader")
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
		actor, system := inbound.RepositoryActor(bound)
		if actor != "reader" || system || repo != target.RepoID {
			test.Fatal("preparation general authority changed caller or repository")
		}
		generalChecks++
		return bound, nil
	})
	ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
		actor, system := inbound.RepositoryActor(bound)
		if actor != "reader" || system || repo != target.RepoID {
			test.Fatal("preparation elevated caller authority")
		}
		checks++
		return bound, nil
	})
	_, err := service.QueryPreparedMemory(ctx, target.RepoID, domain.EffectiveMemoryRequest{Selection: target.Selection, Content: target.Content})
	var pending *domain.MemoryReadPending
	if !errors.As(err, &pending) || checks != 3 || generalChecks != 2 || len(probe.requested) != 1 {
		test.Fatal("authorized read did not register one demand", checks, generalChecks, err)
	}
}
