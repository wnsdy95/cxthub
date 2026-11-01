//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyNormalizationCheckpointBatchKey struct{}

type legacyNormalizationCheckpointBatch struct {
	owner       *legacyNormalizationCheckpointWork
	state       legacyByteCheckpoint
	transaction pgx.Tx
	protocol    bool
	deferred    bool
	closed      bool
	objects     []legacyByteCheckpointOwnedObject
	bytes       int
}

func (work *legacyNormalizationCheckpointWork) beginBatch(ctx context.Context) (context.Context, *legacyNormalizationCheckpointBatch, error) {
	transaction, state, err := work.store.openLegacyByteCheckpoint(ctx, work.job, work.state.stage, &work.state, false)
	if err != nil {
		return nil, nil, err
	}
	batch := &legacyNormalizationCheckpointBatch{owner: work, state: state, transaction: transaction}
	return context.WithValue(ctx, legacyNormalizationCheckpointBatchKey{}, batch), batch, nil
}

func (batch *legacyNormalizationCheckpointBatch) finish(ctx context.Context, failure error) error {
	defer func() {
		if batch.transaction != nil {
			rollbackPG(batch.transaction)
		}
		batch.closed = true
		batch.transaction = nil
		batch.objects = nil
		batch.bytes = 0
	}()
	if failure != nil {
		return failure
	}
	if batch.closed || batch.state != batch.owner.state {
		return domain.ErrConflict
	}
	if batch.transaction == nil {
		if !batch.deferred {
			return domain.ErrConflict
		}
		return batch.owner.confirmDeferredBatch(ctx, batch.state)
	}
	if err := batch.flush(ctx); err != nil {
		return err
	}
	if err := liveLegacyByteJob(ctx, batch.transaction, batch.owner.job); err != nil {
		return err
	}
	if err := batch.transaction.Commit(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

func (work *legacyNormalizationCheckpointWork) currentBatch(ctx context.Context) (*legacyNormalizationCheckpointBatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batch, _ := ctx.Value(legacyNormalizationCheckpointBatchKey{}).(*legacyNormalizationCheckpointBatch)
	if batch != nil {
		if batch.owner != work || batch.closed || batch.state != work.state || batch.transaction == nil && !batch.deferred {
			return nil, domain.ErrConflict
		}
		if batch.transaction == nil {
			transaction, state, err := work.store.openLegacyByteCheckpoint(ctx, work.job, batch.state.stage, &batch.state, false)
			if err != nil {
				batch.closed = true
				return nil, err
			}
			if state != batch.state || state.stage.plan != "" {
				rollbackPG(transaction)
				batch.closed = true
				return nil, domain.ErrConflict
			}
			batch.transaction = transaction
		}
	}
	return batch, nil
}

func (work *legacyNormalizationCheckpointWork) stageParserObjects(ctx context.Context, objects []legacyByteCheckpointObject) error {
	batch, err := work.currentBatch(ctx)
	if err != nil {
		return err
	}
	if batch == nil {
		return work.store.stageLegacyNormalizationCheckpointObjects(ctx, work.job, work.state, objects)
	}
	owned, err := legacyByteCheckpointObjects(ctx, objects)
	if err != nil {
		return err
	}
	if !batch.protocol {
		if err := legacyByteCheckpointProtocols(ctx, batch.transaction); err != nil {
			return err
		}
		batch.protocol = true
	}
	for _, object := range owned {
		if batch.bytes+len(object.data) > domain.LegacyCanonicalEncodingBytes {
			if err := batch.flush(ctx); err != nil {
				return err
			}
		}
		batch.objects = append(batch.objects, object)
		batch.bytes += len(object.data)
		if len(batch.objects) == domain.DocLegacyByteBackfillBatchBlocks || batch.bytes == domain.LegacyCanonicalEncodingBytes {
			if err := batch.flush(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (work *legacyNormalizationCheckpointWork) readParserObject(ctx context.Context, hash domain.ContentHash, size int) ([]byte, error) {
	batch, err := work.currentBatch(ctx)
	if err != nil {
		return nil, err
	}
	if batch == nil {
		return work.store.readLegacyNormalizationCheckpointObject(ctx, work.job, work.state, hash, size)
	}
	if err := batch.flush(ctx); err != nil {
		return nil, err
	}
	return readLegacyNormalizationCheckpointObject(ctx, batch.transaction, work.job, batch.state, hash, size)
}

func (work *legacyNormalizationCheckpointWork) readPrefix(ctx context.Context, prefix legacyNormalizationPrefix, offset, length int) ([]byte, error) {
	batch, err := work.currentBatch(ctx)
	if err != nil {
		return nil, err
	}
	if batch == nil {
		return work.store.readLegacyNormalizationPrefix(ctx, work.job, work.state, prefix, offset, length)
	}
	if err := batch.flush(ctx); err != nil {
		return nil, err
	}
	return readLegacyNormalizationPrefix(ctx, batch.transaction, work.job, batch.state, prefix, offset, length)
}
