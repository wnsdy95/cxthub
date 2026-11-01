package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLegacyNormalizationParserProgressUsesCurrentFrozenOwner(test *testing.T) {
	work, _, buffer := legacyNormalizationParserCut(test, legacyDecoderCheckpointFixtures()[0], LegacyNormalizationPartBytes)
	reads := len(buffer.reads)
	position, err := work.ParserInputPosition(context.Background())
	if err != nil || position != work.decoder.position || position <= 0 {
		test.Fatal("progress was not the actual decoder input position", position, err)
	}
	capture, err := work.BeginCheckpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if current, err := capture.ParserInputPosition(context.Background()); err != nil || current != position {
		test.Fatal("capture did not bind progress to its frozen decoder", current, err)
	}
	if _, err := work.ParserInputPosition(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("progress reader bypassed the active capture", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := capture.ParserInputPosition(ctx); !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation lost precedence over a capture read", err)
	}
	capture.Close()
	if current, err := work.ParserInputPosition(context.Background()); err != nil || current != position || len(buffer.reads) != reads {
		test.Fatal("progress queried original bytes or changed after capture release", current, err)
	}
	work.mutation.stepping.Store(true)
	if _, err := work.ParserInputPosition(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("progress reader raced the active worker", err)
	}
	work.mutation.stepping.Store(false)
}

func TestLegacyNormalizationParserProgressUsesDistinctStageContract(test *testing.T) {
	work, _ := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(2), false)
	defer work.Close()
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	if _, err := work.ParserInputPosition(context.Background()); !errors.Is(err, ErrDocRangeIndexPending) {
		test.Fatal("Decode-only progress lookup accepted event-output Stage", err)
	}
	capture, err := work.BeginCheckpoint(context.Background())
	if err != nil {
		test.Fatal("untouched Stage batch could not be captured", err)
	}
	defer capture.Close()
	control, err := capture.Control(context.Background())
	if err != nil || !control.Staging() || control.Parsing() || !control.InputPending() || control.Validate() != nil {
		test.Fatal("Stage capture lost its distinct input-pending contract", control, err)
	}
	if position, err := capture.ParserInputPosition(context.Background()); err != nil || position != work.decoder.position {
		test.Fatal("frozen Stage capture lost the real decoder anchor", position, err)
	}
}

func TestLegacyNormalizationStageCheckpointKeepsUnfinishedPreparationPending(test *testing.T) {
	work, _ := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":{"items":[`+strings.Repeat("null,", 5000)+`null]}}`)
	if complete, err := work.Step(context.Background()); err != nil || complete || work.phase != legacyNormalizationStage ||
		work.pending[work.next].Encoding.preparation == nil || legacyCanonicalInitialState(work.pending[work.next].Encoding) {
		test.Fatal("fixture did not reach actual unfinished output preparation", complete, err)
	}
	if capture, err := work.BeginCheckpoint(context.Background()); capture != nil || !errors.Is(err, ErrDocRangeIndexPending) || work.mutation.activity.Load() != 0 {
		test.Fatal("unsupported preparation acquired a checkpoint or blocked the live worker", err)
	}
	if _, err := work.Step(context.Background()); err != nil {
		test.Fatal("pending capture did not leave the real worker resumable", err)
	}
}

func TestLegacyNormalizationOutputProgressUsesLiveLogicalSize(test *testing.T) {
	work, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":{"payload":"`+strings.Repeat("output-progress", 20000)+`"}}`)
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.files[0].size > LegacyCanonicalEncodingBytes })
	reads, buffered, size := len(source.reads), len(work.files[0].buffer), work.files[0].size
	expected := LegacyNormalizationOutputProgress{Generation: work.files[0].generation, Bytes: size}
	position, err := work.UnconsumedOutputPosition(context.Background())
	if err != nil || position != expected {
		test.Fatal("output progress did not include the live buffered suffix", position, size, err)
	}
	capture, err := work.BeginCheckpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if _, err := work.UnconsumedOutputPosition(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("output progress bypassed the frozen owner", err)
	}
	capture.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := work.UnconsumedOutputPosition(ctx); !errors.Is(err, context.Canceled) {
		test.Fatal("output progress ignored cancellation", err)
	}
	if position, err := work.UnconsumedOutputPosition(context.Background()); err != nil || position != expected ||
		len(source.reads) != reads || len(work.files[0].buffer) != buffered || work.files[0].size != size || work.files[0].generation != expected.Generation {
		test.Fatal("output progress performed IO, flushed output or advanced the worker", position, err)
	}
}
