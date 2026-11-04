package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphRepairTransactions struct {
	*graphProjectionReadStore
	active bool
	check  func(context.Context)
}

func (storage *graphRepairTransactions) InTransaction(ctx context.Context) bool {
	return ctx.Value(graphReadTransactionKey{}) != nil
}

func (storage *graphRepairTransactions) WithinRepository(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(context.WithValue(ctx, graphReadTransactionKey{}, true))
}

func (storage *graphRepairTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		return read(ctx)
	}
	storage.active = true
	defer func() { storage.active = false }()
	return read(context.WithValue(ctx, graphReadTransactionKey{}, true))
}

func (storage *graphRepairTransactions) RequestGraphProjectionRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	if storage.check != nil {
		storage.check(ctx)
	}
	return storage.graphProjectionReadStore.RequestGraphProjectionRepair(ctx, failure)
}

func (storage *graphRepairTransactions) RequestGraphServingRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	if storage.check != nil {
		storage.check(ctx)
	}
	return storage.graphProjectionReadStore.RequestGraphServingRepair(ctx, failure)
}

func graphReadFailure(storage *graphProjectionReadStore) *domain.GraphProjectionCorruption {
	return &domain.GraphProjectionCorruption{RepoID: storage.result.Applied.RepoID, Dataset: storage.result.Dataset,
		Applied: storage.result.Applied, Rules: storage.result.Rules, Serving: true, ServingFence: storage.result.ServingFence, Cause: domain.ErrIntegrity}
}

func TestGraphRepairRunsAfterOwnedReadAndReturnsOriginalFailure(test *testing.T) {
	for _, endpoint := range []string{"graph", "view", "pending"} {
		for _, corruptStore := range []bool{false, true} {
			test.Run(endpoint+map[bool]string{false: "/codec", true: "/store"}[corruptStore], func(test *testing.T) {
				service, storage, repo, _ := newGraphProjectionRead(test)
				transactional := &graphRepairTransactions{graphProjectionReadStore: storage}
				service.meta = transactional
				if corruptStore {
					storage.readErr = graphReadFailure(storage)
				} else {
					storage.result.Parts = storage.result.Parts[:1]
				}
				ctx, gate := admissionTestContext(time.Second)
				ctx = inbound.WithSystemActor(ctx)
				transactional.check = func(bound context.Context) {
					if transactional.active || transactional.InTransaction(bound) {
						test.Fatal("repair retained read transaction")
					}
					deadline, exists := bound.Deadline()
					if !exists || time.Until(deadline) > 2*time.Second {
						test.Fatal("repair lacks bounded deadline")
					}
					permit, exists := bound.Value(resourcePermitKey{}).(*resourcePermit)
					if !exists || !permit.live.Load() {
						test.Fatal("repair bypassed admission")
					}
				}
				var err error
				switch endpoint {
				case "graph":
					_, err = service.QueryGraphState(ctx, repo, "")
				case "view":
					_, err = service.GetRepositoryView(ctx, repo)
				case "pending":
					_, err = service.GetPendingView(ctx, repo)
				}
				if !errors.Is(err, domain.ErrIntegrity) || len(storage.repairs) != 1 ||
					!reflect.DeepEqual(storage.repairs[0], *graphReadFailure(storage)) {
					test.Fatal("repair did not preserve failed generation", err, storage.repairs)
				}
				if storage.snapshotLists != 0 || storage.evidenceReads != 0 {
					test.Fatal("repair rebuilt inline")
				}
				assertResourcesReleased(test, gate)
			})
		}
	}
}

func TestGraphRepairNeverWritesThroughCallerTransaction(test *testing.T) {
	for _, ownership := range []string{"read", "write", "unknown", "readonly"} {
		test.Run(ownership, func(test *testing.T) {
			service, storage, repo, _ := newGraphProjectionRead(test)
			storage.readErr = graphReadFailure(storage)
			transactional := &graphRepairTransactions{graphProjectionReadStore: storage}
			service.meta = transactional
			ctx := systemTestContext()
			switch ownership {
			case "read":
				ctx = context.WithValue(ctx, graphReadTransactionKey{}, true)
			case "write":
				ctx = context.WithValue(ctx, afterCommitKey{}, &afterCommitActions{})
			case "unknown":
				service.meta = struct {
					*graphProjectionReadStore
					outbound.RepositoryTransactions
				}{storage, transactional}
			case "readonly":
				ctx = outbound.WithDocReadOnly(ctx)
			}
			_, err := service.GetRepositoryView(ctx, repo)
			if !errors.Is(err, domain.ErrIntegrity) || len(storage.repairs) != 0 {
				test.Fatal("repair escaped caller transaction", err, storage.repairs)
			}
		})
	}
}

func TestGraphRepairDoesNotScheduleUnidentifiedOrForeignFailure(test *testing.T) {
	for _, kind := range []string{"source", "foreign", "unknown-dataset", "canceled"} {
		test.Run(kind, func(test *testing.T) {
			service, storage, repo, _ := newGraphProjectionRead(test)
			failure := graphReadFailure(storage)
			switch kind {
			case "source":
				storage.readErr = domain.ErrIntegrity
			case "foreign":
				failure.RepoID = hh("foreign")
			case "unknown-dataset":
				failure.Dataset = ""
			case "canceled":
				failure.Cause = context.Canceled
			}
			if storage.readErr == nil {
				storage.readErr = failure
			}
			if _, err := service.GetRepositoryView(systemTestContext(), repo); err == nil || len(storage.repairs) != 0 {
				test.Fatal("unidentified output failure scheduled repair", err, storage.repairs)
			}
		})
	}
}

func TestGraphRepairReauthorizesAndReportsSchedulingFailure(test *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(test)
	storage.readErr = graphReadFailure(storage)
	checks := 0
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
		checks++
		if requested != repo || checks > 1 {
			return bound, domain.ErrForbidden
		}
		return bound, nil
	})
	_, err := service.GetRepositoryView(ctx, repo)
	if !errors.Is(err, domain.ErrIntegrity) || !errors.Is(err, domain.ErrForbidden) || checks != 2 || len(storage.repairs) != 0 {
		test.Fatal("repair ignored current authorization", err, checks, storage.repairs)
	}
	storage.repairErr = errors.New("durable repair unavailable")
	_, err = service.GetRepositoryView(systemTestContext(), repo)
	if !errors.Is(err, domain.ErrIntegrity) || !errors.Is(err, storage.repairErr) || len(storage.repairs) != 1 {
		test.Fatal("scheduling failure hidden", err, storage.repairs)
	}
}
