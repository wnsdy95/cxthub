package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLegacyNormalizationPreparingRestartSamePendingEvents(test *testing.T) {
	for _, kind := range []byte{'a', 'o', 'r'} {
		test.Run(string(kind), func(test *testing.T) {
			work, source := legacyCanonicalSourceCheckpointStage(test, kind)
			legacyNormalizationPreparingRestartRoundTrip(test, work, source)
		})
	}
}

func TestLegacyNormalizationPreparingRestartKeepsCompletedPrefix(test *testing.T) {
	events := `{"kind":"turn","seq":7},{"kind":"tool_call","seq":7,"provider_metadata":{"create_time":1e-7},"input":{"items":[` + strings.Repeat("null,", 5000) + `null]}},{"kind":"turn","seq":8}`
	work, source := legacyCanonicalInitialCheckpointStage(test, events)
	legacyNormalizationPreparationAdvance(test, work, func() bool {
		return work.phase == legacyNormalizationStage && work.count == 1 && work.pending[work.next].Encoding.preparation != nil && !legacyCanonicalInitialState(work.pending[work.next].Encoding)
	})
	if work.count != 1 || work.written != 0 || work.files[0].size == 0 || work.files[1].size == 0 {
		test.Fatal("fixture did not complete exactly one event before preparing the next")
	}
	legacyNormalizationPreparingRestartRoundTrip(test, work, source)
}

func legacyNormalizationPreparingRestartRoundTrip(test *testing.T, work *LegacyNormalizationPreparation, source *legacySourceProofBuffer) {
	test.Helper()
	ctx := context.Background()
	position, count, next, reads := work.decoder.position, work.count, work.next, len(source.reads)
	output, records := work.files[0].size, work.files[1].size
	encoding := work.pending[next].Encoding
	if work.written != 0 || legacyCanonicalInitialState(encoding) {
		test.Fatal("test requires a progressed, unwritten event")
	}
	capture, err := work.BeginCheckpoint(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer capture.Close()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	io := LegacyNormalizationParserCheckpointIO{Put: store.Put, Read: store.Read}
	var root LegacyNormalizationParserCheckpointRef
	for _, step := range []func(context.Context, ContentHash, LegacyNormalizationParserCheckpointIO, int64) (LegacyNormalizationParserCheckpointRef, bool, error){capture.ParserStep, capture.StageStep} {
		root = LegacyNormalizationParserCheckpointRef{}
		for range legacyDecoderCheckpointSteps {
			ref, complete, err := step(ctx, HashContent(source.body), io, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal("actual Stage capture failed", err)
			}
			if retained := legacyNormalizationStageFenceRetained(test, work, capture); retained > legacyDecoderCheckpointBudget {
				test.Fatal("actual Stage capture exceeded aggregate memory budget", retained)
			}
			if complete {
				root = ref
				break
			}
		}
		if root == (LegacyNormalizationParserCheckpointRef{}) {
			test.Fatal("capture exceeded its finite step allowance")
		}
	}
	control, err := capture.Control(ctx)
	if err != nil || !control.Staging() || control.Count != count || control.Written != 0 || control.StagePending != len(work.pending)-next ||
		control.Files[0].Size != output || control.Files[1].Size != records || work.decoder.position != position || work.next != next ||
		len(source.reads) != reads || work.pending[next].Encoding != encoding || legacyCanonicalInitialState(encoding) {
		test.Fatal("capture drained/replayed the source or changed completed output", control, err)
	}
	restore, err := newLegacyNormalizationStageRestore(ctx, LegacyNormalizationParserCheckpointSource{
		Ref: root, Binding: HashContent(source.body), Store: io, Budget: legacyDecoderCheckpointBudget,
	}, control.StagePending, len(source.body), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	legacyDecoderCheckpointDrain(test, restore, legacyDecoderCheckpointBudget)
	if !restore.ready || len(restore.events) != control.StagePending || !legacyCanonicalInitialState(restore.events[0].Encoding) {
		test.Fatal("restore did not restart only the unpublished pending events")
	}
	capture.Close()
	for index, event := range restore.events {
		original := work.pending[next+index]
		if event.Sequence != original.Sequence {
			test.Fatal("restore lost event occurrence order", index)
		}
		actual, _ := collectLegacyCanonicalEncoding(test, event.Encoding)
		expected, _ := collectLegacyCanonicalEncoding(test, original.Encoding)
		if !bytes.Equal(actual, expected) || HashContent(actual) != HashContent(expected) {
			test.Fatal("restarting preparation changed canonical bytes", index)
		}
	}
}

func TestLegacyNormalizationPreparingRestartRejectsEmittedBytes(test *testing.T) {
	work, _ := legacyCanonicalSourceCheckpointStage(test, 'a')
	work.written = 1
	defer func() { work.written = 0 }()
	if capture, err := work.BeginCheckpoint(context.Background()); capture != nil || !errors.Is(err, ErrIntegrity) || work.mutation.activity.Load() != 0 {
		test.Fatal("preparation restart accepted already emitted bytes", err)
	}
}
