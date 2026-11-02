package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func legacyCanonicalMapContinuationFixture(test *testing.T) (*LegacyNormalizationPreparation, *legacyNormalizationPreparationStorage, []byte, VerifiedSessionDoc, *LegacyCanonicalEncoding, int) {
	test.Helper()
	values := make(map[string]any, 5000)
	for index := 0; index < 5000; index++ {
		values[fmt.Sprintf("key-%06d", index)] = fmt.Sprintf("value-%d <&> \ud55c🙂", index)
	}
	body, err := json.Marshal(values)
	if err != nil {
		test.Fatal(err)
	}
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","seq":9,"call_id":"map-continuation","output":`)
	raw = append(raw, body...)
	raw = append(raw, []byte(`}]}`)...)
	canonical, original, err := legacyNormalizationPreparationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	var encoding *LegacyCanonicalEncoding
	frameIndex := -1
	legacyNormalizationPreparationAdvance(test, work, func() bool {
		if work.phase != legacyNormalizationStage || work.next >= len(work.pending) {
			return false
		}
		encoding = work.pending[work.next].Encoding
		if encoding == nil || encoding.preparation == nil {
			return false
		}
		for index := 0; index < encoding.preparation.frames.length(); index++ {
			frame := encoding.preparation.frames.at(index)
			if frame.kind == 'o' && frame.phase == 0 && frame.count == len(values) && frame.position > 0 && frame.position < frame.count {
				frameIndex = index
				return true
			}
		}
		return false
	})
	return work, storage, canonical, original, encoding, frameIndex
}

func legacyCanonicalMapContinuationTake(test *testing.T, work *LegacyNormalizationPreparation, encoding *LegacyCanonicalEncoding, index int) *legacyCanonicalMapContinuation {
	test.Helper()
	ctx := context.Background()
	grant, err := work.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	frame := encoding.preparation.frames.at(index)
	initial, written, actions := frame.position, work.written, encoding.inspectWork
	capture, err := newLegacyCanonicalMapCapture(ctx, work, index, grant, 64<<20)
	if err != nil {
		test.Fatal(err)
	}
	defer capture.close()
	steps := 0
	for ; steps < 8192; steps++ {
		before := capture.fields.length()
		ready, err := capture.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if capture.fields.length()-before > legacyCanonicalEncodingActions || frame.position != initial || work.written != written || encoding.inspectWork != actions {
			test.Fatal("capturing the map replayed semantic work or escaped its bounded field quantum")
		}
		if _, err := work.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("capturing a live map did not keep the source owner frozen", err)
		}
		if ready {
			break
		}
	}
	if steps == 8192 || steps == 0 {
		test.Fatal("large map continuation did not use multiple bounded capture steps", steps)
	}
	resume, err := capture.take(ctx)
	if err != nil || resume.fields.length() != frame.count-initial || resume.position != 0 {
		test.Fatal("remaining map entries lost their exact resume geometry", err)
	}
	return resume
}

func TestLegacyCanonicalMapContinuationActualPreparationResumesAndRecaptures(test *testing.T) {
	work, storage, canonical, original, encoding, index := legacyCanonicalMapContinuationFixture(test)
	frame := encoding.preparation.frames.at(index)
	iterator := *frame.iterator
	resume := legacyCanonicalMapContinuationTake(test, work, encoding, index)
	for ordinal := 0; ordinal < resume.fields.length(); ordinal++ {
		if !iterator.Next() {
			test.Fatal("continuation invented a map entry")
		}
		field := resume.fields.at(ordinal)
		if field.key != iterator.Key().String() || field.value != iterator.Value().Interface() {
			test.Fatal("continuation repeated or reordered previously consumed map entries", ordinal)
		}
	}
	if iterator.Next() {
		test.Fatal("continuation omitted an unconsumed map entry")
	}
	frame.iterator, frame.mapResume = nil, resume
	if complete, err := work.Step(context.Background()); err != nil || complete || frame.phase != 0 || frame.mapResume == nil || frame.mapResume.position == 0 {
		test.Fatal("the real pending event did not consume the logical map continuation", complete, err)
	}
	resume = legacyCanonicalMapContinuationTake(test, work, encoding, index)
	frame.iterator, frame.mapResume = nil, resume
	complete := false
	for steps := 0; steps < 8192; steps++ {
		var err error
		complete, err = work.Step(context.Background())
		if err != nil {
			test.Fatal("resumed real event did not finish canonical publication preparation", err)
		}
		retained, err := work.RetainedBytes()
		if err != nil || retained <= 0 || retained > 64<<20 {
			test.Fatal("large pending-event continuation exceeded the worker's aggregate heap budget", retained, err)
		}
		if complete {
			break
		}
	}
	if !complete {
		test.Fatal("resumed actual event did not finish within the existing preparation step allowance")
	}
	legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
}

func TestLegacyCanonicalMapContinuationKeepsCaptureFences(test *testing.T) {
	work, _, _, _, _, index := legacyCanonicalMapContinuationFixture(test)
	ctx := context.Background()
	grant, err := work.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			grant.release()
		}
	}()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if capture, err := newLegacyCanonicalMapCapture(canceled, work, index, grant, 64<<20); capture != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation did not precede snapshot setup", err)
	}
	foreign := &legacyNormalizationMutation{}
	foreignGrant, err := foreign.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	if capture, err := newLegacyCanonicalMapCapture(ctx, work, index, foreignGrant, 64<<20); capture != nil || !errors.Is(err, ErrConflict) {
		test.Fatal("another owner's capture grant authorized the current event", err)
	}
	foreignGrant.release()
	if capture, err := newLegacyCanonicalMapCapture(ctx, work, index, grant, 1); capture != nil || !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("map continuation ignored the existing aggregate heap budget", err)
	}
	capture, err := newLegacyCanonicalMapCapture(ctx, work, index, grant, 64<<20)
	if err != nil {
		test.Fatal(err)
	}
	defer capture.close()
	if ready, err := capture.step(canceled); ready || !errors.Is(err, context.Canceled) || capture.fields.length() != 0 {
		test.Fatal("canceled capture advanced map state", ready, err)
	}
	grant.release()
	released = true
	if ready, err := capture.step(ctx); ready || !errors.Is(err, ErrConflict) || capture.fields.length() != 0 {
		test.Fatal("released source authority continued capturing a map", ready, err)
	}
}

func TestLegacyCanonicalMapContinuationAdmitsPageGrowthBeforeAdvancing(test *testing.T) {
	work, _, _, _, encoding, index := legacyCanonicalMapContinuationFixture(test)
	ctx := context.Background()
	grant, err := work.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	var pages legacyCanonicalPages[legacyCanonicalKeyField]
	for ordinal := 0; ordinal < legacyCanonicalPageValues; ordinal++ {
		if err := pages.append(legacyCanonicalKeyField{}); err != nil {
			test.Fatal(err)
		}
	}
	_, forecast, err := pages.appendRequirements()
	if err != nil {
		test.Fatal(err)
	}
	capture, err := newLegacyCanonicalMapCapture(ctx, work, index, grant, 64<<20)
	if err != nil {
		test.Fatal(err)
	}
	budget := capture.baseHeap + forecast.bytes - 1
	capture.close()
	capture, err = newLegacyCanonicalMapCapture(ctx, work, index, grant, budget)
	if err != nil {
		test.Fatal(err)
	}
	defer capture.close()
	position := encoding.preparation.frames.at(index).position
	if ready, err := capture.step(ctx); ready || !errors.Is(err, ErrDocumentResourceLimit) || capture.fields.length() != legacyCanonicalPageValues {
		test.Fatal("a page growth crossed its aggregate allocation budget before rejection", ready, err, capture.fields.length())
	}
	if encoding.preparation.frames.at(index).position != position {
		test.Fatal("failed map growth advanced the actual frozen event")
	}
	if resume, err := capture.take(ctx); resume != nil || !errors.Is(err, ErrDocRangeIndexPending) {
		test.Fatal("over-budget map continuation acquired resume authority", err)
	}
}
