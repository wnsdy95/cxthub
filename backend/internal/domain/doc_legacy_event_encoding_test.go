package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type legacyEventEncodingFinalizationContext struct {
	context.Context
	completed func() bool
}

func (ctx legacyEventEncodingFinalizationContext) Err() error {
	if ctx.completed() {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestLegacyEventEncodingFinalizationCancellationLatchesProvisionalWork(test *testing.T) {
	test.Run("decoder", func(test *testing.T) {
		input := &legacySourceProofBuffer{body: []byte(`{}`)}
		decoder, err := NewLegacyNormalizationDecoder(context.Background(), input.input())
		if err != nil {
			test.Fatal(err)
		}
		ctx := legacyEventEncodingFinalizationContext{Context: context.Background(), completed: func() bool { return decoder.phase == legacyNormalizationComplete }}
		for quantum := 0; quantum < 16; quantum++ {
			step, err := decoder.Step(ctx)
			if err == nil {
				continue
			}
			if !errors.Is(err, context.Canceled) || step.Complete || step.Envelope != nil || len(step.Events) != 0 || step.Reset {
				test.Fatal("canceled decoder finalization exposed output", step, err)
			}
			if repeated, err := decoder.Step(context.Background()); !errors.Is(err, context.Canceled) || repeated.Complete || repeated.Envelope != nil {
				test.Fatal("failed provisional decoder resumed as a success", repeated.Complete, err)
			}
			return
		}
		test.Fatal("decoder never reached finalization cancellation")
	})
	test.Run("preparation", func(test *testing.T) {
		work, _ := legacyNormalizationPreparationStart(test, []byte(`{}`), false)
		ctx := legacyEventEncodingFinalizationContext{Context: context.Background(), completed: func() bool { return work.phase == legacyNormalizationReady }}
		for quantum := 0; quantum < 64; quantum++ {
			complete, err := work.Step(ctx)
			if err == nil {
				continue
			}
			if !errors.Is(err, context.Canceled) || complete {
				test.Fatal("canceled output finalization returned success", complete, err)
			}
			if source, err := work.Source(); err == nil || source.Read != nil || source.Parts != nil || source.Bytes != 0 {
				test.Fatal("failed finalization retained a usable provisional source", source.Bytes, err)
			}
			if complete, err := work.Step(context.Background()); !errors.Is(err, context.Canceled) || complete {
				test.Fatal("failed output finalization resumed as a success", complete, err)
			}
			return
		}
		test.Fatal("preparation never reached finalization cancellation")
	})
}

func TestLegacyEventEncodingCompletedWorkSurvivesCanceledObservation(test *testing.T) {
	input := &legacySourceProofBuffer{body: []byte(`{}`)}
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), input.input())
	if err != nil {
		test.Fatal(err)
	}
	completed := false
	for quantum := 0; quantum < 16; quantum++ {
		step, err := decoder.Step(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if step.Complete {
			completed = true
			break
		}
	}
	if !completed {
		test.Fatal("decoder did not complete")
	}
	canceledObservation := func() context.Context {
		checks := 0
		return legacyEventEncodingFinalizationContext{Context: context.Background(), completed: func() bool {
			checks++
			return checks > 1
		}}
	}
	if step, err := decoder.Step(canceledObservation()); !errors.Is(err, context.Canceled) || step.Complete {
		test.Fatal("completed decoder ignored observation cancellation", step.Complete, err)
	}
	if step, err := decoder.Step(context.Background()); err != nil || !step.Complete {
		test.Fatal("canceled observation invalidated prior decoder success", step.Complete, err)
	}
	work, _ := legacyNormalizationPreparationStart(test, []byte(`{}`), false)
	if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
		test.Fatal(err)
	}
	if complete, err := work.Step(canceledObservation()); !errors.Is(err, context.Canceled) || complete {
		test.Fatal("completed preparation ignored observation cancellation", complete, err)
	}
	if source, err := work.Source(); err != nil || source.Read == nil || source.Bytes == 0 {
		test.Fatal("canceled observation invalidated prior prepared output", source.Bytes, err)
	}
	if complete, err := work.Step(context.Background()); err != nil || !complete {
		test.Fatal("canceled observation invalidated prior preparation success", complete, err)
	}
}

func TestLegacyEventEncodingStagesBoundedChunksBeforeRecord(test *testing.T) {
	payload := strings.Repeat("raw <>& 한🙂\n\\\"\u2028", 18000)
	raw := []byte(fmt.Sprintf(`{"events":[{"kind":"message","role":"user","seq":-3,"blocks":[{"type":"text","text":%q}]}],"envelope":{"cir_version":"2"}}`, payload))
	canonical, original, err := legacyNormalizationPreparationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	if len(work.pending) != 1 || work.pending[0].Encoding == nil {
		test.Fatal("decoder did not transfer an owned event encoding")
	}
	steps := 0
	for steps < 4096 && work.phase == legacyNormalizationStage {
		before := work.files[0].size
		if complete, err := work.Step(context.Background()); err != nil || complete {
			test.Fatal("event encoding did not stay provisional", complete, err)
		}
		steps++
		if work.files[0].size-before > LegacyCanonicalEncodingBytes {
			test.Fatal("one unfinished event copied more than one bounded encoding chunk", work.files[0].size-before)
		}
		if work.phase == legacyNormalizationStage && (work.count != 0 || work.files[1].size != 0) {
			test.Fatal("unfinished event acquired a fixed order record")
		}
	}
	if steps < 4 || work.phase != legacyNormalizationDecode || work.count != 1 || work.files[1].size != legacyNormalizationRecordBytes || work.pending != nil {
		test.Fatal("event did not release its encoding and commit exactly one original record", steps, work.phase, work.count)
	}
	if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
		test.Fatal(err)
	}
	legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
}

func TestLegacyEventEncodingDigestPredatesBodyStorageMutation(test *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","role":"user","blocks":[{"type":"text","text":"` + strings.Repeat("original", 24000) + `"}]}]}`)
	work, storage := legacyNormalizationPreparationStart(test, raw, false)
	mutated := false
	storage.appendHook = func(slot int, raw []byte) error {
		if slot == 0 && !mutated && len(raw) > 0 {
			raw[0] ^= 1
			mutated = true
		}
		return nil
	}
	_, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
	if !mutated || !errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("storage callback redefined an event digest", mutated, err)
	}
	legacyNormalizationPreparationPending(test, work)
}

func TestLegacyEventEncodingCancellationDoesNotCommitPartialRecord(test *testing.T) {
	raw := []byte(`{"events":[{"kind":"tool_result","output":"` + strings.Repeat("body", 150000) + `"}]}`)
	work, storage := legacyNormalizationPreparationStart(test, raw, false)
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	if complete, err := work.Step(context.Background()); complete || err != nil || work.written == 0 || work.count != 0 {
		test.Fatal("fixture failed to stop within event output", complete, err, work.written)
	}
	before, hash := work.files[0].size, sha256.Sum256(storage.slots[0])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if complete, err := work.Step(ctx); complete || !errors.Is(err, context.Canceled) || work.count != 0 || work.files[1].size != 0 || work.files[0].size != before || hash != sha256.Sum256(storage.slots[0]) {
		test.Fatal("canceled event output committed bytes or a record", complete, err)
	}
	legacyNormalizationPreparationPending(test, work)
}

func TestLegacyEventEncodingEmptyContainersMatchOriginalUnion(test *testing.T) {
	for _, raw := range []string{
		`{"events":[{"kind":"message","blocks":null}]}`,
		`{"events":[{"kind":"tool_call","input":null}]}`,
		`{"events":[{"kind":"tool_result","output":null}]}`,
		`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[],"replacement_complete":false}]}`,
	} {
		canonical, original, err := legacyNormalizationPreparationOracle([]byte(raw))
		if err != nil {
			test.Fatal(err)
		}
		work, storage := legacyNormalizationPreparationStart(test, []byte(raw), true)
		if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
			test.Fatal(err)
		}
		if !bytes.Equal(storage.slots[3], canonical) {
			test.Fatal("incremental output changed an original empty container")
		}
		legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
	}
}
