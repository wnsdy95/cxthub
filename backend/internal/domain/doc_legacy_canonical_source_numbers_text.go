package domain

import "context"

func newLegacyCanonicalSourceNumbersCheckpoint(ctx context.Context, numbers *legacyCanonicalNumberValidation, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedNumbersCheckpoint, error) {
	work, err := newLegacyCanonicalPreparedNumbersCheckpoint(ctx, numbers, binding, store, budget)
	if err != nil {
		return nil, err
	}
	work.keepTexts = true
	return work, nil
}

func newLegacyCanonicalSourceNumbersRestore(ctx context.Context, state legacyCanonicalPreparedNumberState, binding ContentHash, store legacyNativeCheckpointIO, budget int64) (*legacyCanonicalPreparedNumbersRestore, error) {
	work, err := newLegacyCanonicalPreparedNumbersRestore(ctx, state, binding, store, budget)
	if err != nil {
		return nil, err
	}
	work.keepTexts = true
	return work, nil
}

type legacyCanonicalSourceNumberText struct {
	text     legacyNativeCheckpointText
	capacity int
}

type legacyCanonicalSourceNumberTexts struct {
	values  legacyCanonicalPages[legacyCanonicalSourceNumberText]
	payload docRangeHeaderHeapEstimate
	binding ContentHash
	ready   bool
}

func (texts *legacyCanonicalSourceNumberTexts) append(text legacyNativeCheckpointText, capacity int, admit func(docRangeHeaderHeapEstimate) error) error {
	if texts == nil || texts.ready || admit == nil {
		return ErrIntegrity
	}
	if err := (legacyNativeCheckpointStringValueRow{Capacity: capacity, Text: text}).validate(); err != nil {
		return err
	}
	_, future, err := texts.values.appendRequirements()
	if err != nil {
		return err
	}
	var current docRangeHeaderHeapEstimate
	texts.values.retained(&current)
	if current.err != nil || future.err != nil || future.bytes < current.bytes {
		return ErrIntegrity
	}
	planned := docRangeHeaderHeapEstimate{bytes: future.bytes - current.bytes}
	planned.include(legacyNativeCheckpointGoGraphPageRetirement(&texts.values, future))
	planned.allocation(int64(len(text.Tree.Ref.Hash)), 1)
	if err := admit(planned); err != nil {
		return err
	}
	if err := texts.values.append(legacyCanonicalSourceNumberText{text: text, capacity: capacity}); err != nil {
		return err
	}
	texts.payload.allocation(int64(len(text.Tree.Ref.Hash)), 1)
	return texts.payload.err
}

func (texts *legacyCanonicalSourceNumberTexts) retained(heap *docRangeHeaderHeapEstimate) {
	if texts == nil {
		heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
		return
	}
	texts.values.retained(heap)
	heap.include(texts.payload)
	heap.allocation(int64(len(texts.binding)), 1)
}

func (texts *legacyCanonicalSourceNumberTexts) validate(numbers *legacyCanonicalNumberValidation, binding ContentHash) error {
	if texts == nil || !texts.ready || texts.binding != binding || !texts.values.valid() || texts.payload.err != nil || texts.payload.bytes < 0 ||
		numbers == nil || !numbers.pending.valid() || !numbers.proven.valid() ||
		texts.values.length() != numbers.pending.length() || numbers.pending.length() != numbers.proven.length() {
		return ErrIntegrity
	}
	return legacyCanonicalPreparedNumberCursor(numbers.next, numbers.position, numbers.mode, numbers.pending.length())
}

func (work *legacyCanonicalPreparedNumbersCheckpoint) takeWithTexts(ctx context.Context) (legacyCanonicalPreparedNumberState, legacyCanonicalSourceNumberTexts, error) {
	if work == nil || !work.keepTexts {
		return legacyCanonicalPreparedNumberState{}, legacyCanonicalSourceNumberTexts{}, ErrIntegrity
	}
	texts := work.texts
	texts.binding, texts.ready = work.binding, true
	state, err := work.take(ctx)
	if err != nil {
		return legacyCanonicalPreparedNumberState{}, legacyCanonicalSourceNumberTexts{}, err
	}
	return state, texts, nil
}

func (work *legacyCanonicalPreparedNumbersRestore) takeWithTexts(ctx context.Context) (legacyCanonicalNumberValidation, docRangeHeaderHeapEstimate, legacyCanonicalSourceNumberTexts, error) {
	if work == nil || !work.keepTexts {
		return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}, legacyCanonicalSourceNumberTexts{}, ErrIntegrity
	}
	texts := work.texts
	texts.binding, texts.ready = work.binding, true
	numbers, heap, err := work.take(ctx)
	if err != nil {
		return legacyCanonicalNumberValidation{}, docRangeHeaderHeapEstimate{}, legacyCanonicalSourceNumberTexts{}, err
	}
	return numbers, heap, texts, nil
}
