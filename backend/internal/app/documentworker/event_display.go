package documentworker

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (worker *DisplayWorker) BuildEvent(ctx context.Context, job domain.DocDisplayJob) (bool, error) {
	storage, supported := worker.storage.(outbound.DocEventDisplayStore)
	if !supported {
		return false, domain.ErrDocFindDisplayUnavailable
	}
	progress, err := storage.LoadDocEventDisplay(ctx, job)
	if err != nil {
		return false, err
	}
	for processed := 0; processed < DisplayJobSteps && progress.Phase != "completed"; processed++ {
		encoded, err := progress.Encode()
		if err != nil {
			return false, err
		}
		var step domain.DocEventDisplayStep
		err = worker.scope.WithinRepositoryRead(ctx, job.RepoID, func(read context.Context) error {
			source, err := worker.Source(read, job.Target)
			if err != nil {
				return err
			}
			reader, err := storage.OpenDocEventDisplayWork(read, job, domain.HashContent(encoded))
			if err != nil {
				return err
			}
			var tools domain.DocToolPreparationReader
			if trees, supported := reader.(outbound.DocToolTreeWorkReader); supported && progress.ToolTree != nil {
				tools.Nodes = func(read context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
					return trees.ReadToolTreeNodes(read, *progress.ToolTree, after, limit)
				}
			}
			if orders, supported := reader.(outbound.DocToolOrderWorkReader); supported && progress.ToolTree != nil && progress.ToolKeys != nil {
				tools.OrderSource = func(read context.Context, parent int) (domain.DocToolOrderSource, error) {
					return orders.ReadToolOrderSource(read, *progress.ToolTree, *progress.ToolKeys, parent)
				}
				tools.OrderEntries = func(read context.Context, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
					return orders.ReadToolOrderEntries(read, *progress.ToolTree, *progress.ToolKeys, source, after, limit)
				}
				if progress.ToolOrders != nil {
					tools.OrderPlan = func(read context.Context, parent int) (domain.DocToolOrderPlan, error) {
						return orders.ReadToolOrderPlan(read, *progress.ToolTree, *progress.ToolKeys, *progress.ToolOrders, parent)
					}
				}
			}
			step, err = reader.PrepareStep(read, progress, func(read context.Context, offset, length int) ([]byte, error) {
				if err := read.Err(); err != nil {
					return nil, err
				}
				if offset < 0 || length <= 0 || length > domain.DocFieldDiscoveryStepBytes || offset > job.Target.Event.Length-length {
					return nil, domain.ErrIntegrity
				}
				return source.Read(job.Target.Event.Offset+offset, length)
			}, tools)
			return err
		})
		if err != nil {
			return false, err
		}
		progress, err = storage.SaveDocEventDisplay(ctx, job, step)
		if err != nil {
			return false, err
		}
	}
	if progress.Phase != "completed" {
		return false, nil
	}
	encoded, err := progress.Encode()
	if err != nil {
		return false, err
	}
	return true, storage.CompleteDocEventDisplay(ctx, job, domain.HashContent(encoded), time.Now().UTC())
}
