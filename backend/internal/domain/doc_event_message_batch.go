package domain

import (
	"context"
	"errors"
)

const DocEventMessageBatchTransitions = 16

func (builder *docEventDisplayBuilder) messageBatch(loadRaw func(context.Context, int, int) ([]byte, error)) error {
	readBytes, objectsRead := 0, 0
	originalLoad := builder.load
	originalBudget := builder.yieldBudget
	builder.yieldBudget = true
	builder.load = func(ctx context.Context, hash ContentHash) ([]byte, error) {
		objectsRead++
		return originalLoad(ctx, hash)
	}
	defer func() {
		builder.load, builder.yieldBudget = originalLoad, originalBudget
	}()
	boundedRaw := func(ctx context.Context, offset, length int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if length <= 0 || length > DocFieldDiscoveryStepBytes {
			return nil, ErrIntegrity
		}
		if length > DocFieldDiscoveryStepBytes-readBytes {
			return nil, errDocEventDisplayBatchYield
		}
		raw, err := loadRaw(ctx, offset, length)
		if err != nil {
			return nil, err
		}
		if len(raw) != length {
			return nil, ErrIntegrity
		}
		readBytes += len(raw)
		return raw, ctx.Err()
	}
	inputReader := docMessageInputReader{}
	for transition := 0; transition < DocEventMessageBatchTransitions; transition++ {
		if transition > 0 && (objectsRead >= DocDisplayMappedReplayObjects || readBytes == DocFieldDiscoveryStepBytes || len(builder.objects) >= MaxDocEventDisplayStepObjects-8 || builder.bytes >= MaxDocEventDisplayStepBytes/2) {
			break
		}
		before, count, size := builder.state, len(builder.objects), builder.bytes
		if err := builder.messagePlan(boundedRaw, &inputReader); err != nil {
			if errors.Is(err, errDocEventDisplayBatchYield) && transition > 0 {
				clear(builder.objects[count:])
				builder.state, builder.objects, builder.bytes = before, builder.objects[:count], size
				break
			}
			return err
		}
		if builder.state.Phase != "message_plan" {
			break
		}
	}
	return builder.ctx.Err()
}
