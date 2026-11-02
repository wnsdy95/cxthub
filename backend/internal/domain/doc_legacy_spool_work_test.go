package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"unsafe"
)

type legacySpoolWorkCall struct {
	append bool
	span   canonicalSpan
}

type legacySpoolWorkSource struct {
	*legacyByteSpoolTestSource
	test  testing.TB
	ctx   context.Context
	calls []legacySpoolWorkCall
}

func (source *legacySpoolWorkSource) Append(ctx context.Context, body []byte) error {
	if source.ctx != nil && ctx != source.ctx {
		source.test.Fatal("spool write retained an earlier request context")
	}
	source.calls = append(source.calls, legacySpoolWorkCall{append: true, span: canonicalSpan{offset: len(source.body), length: len(body)}})
	return source.legacyByteSpoolTestSource.Append(ctx, body)
}

func (source *legacySpoolWorkSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if source.ctx != nil && ctx != source.ctx {
		source.test.Fatal("spool read retained an earlier request context")
	}
	source.calls = append(source.calls, legacySpoolWorkCall{span: canonicalSpan{offset: offset, length: length}})
	return source.legacyByteSpoolTestSource.Read(ctx, offset, length)
}

func legacySpoolWorkPending(test testing.TB, preparation *LegacyByteSpoolPreparation) {
	test.Helper()
	plan, err := preparation.Plan()
	legacyByteSpoolRequireZero(test, plan, err, ErrDocRangeIndexPending)
}

func legacySpoolWorkFinish(test testing.TB, preparation *LegacyByteSpoolPreparation, blocks int) LegacyByteBackfill {
	test.Helper()
	for step := 0; step < max(1, 2*blocks); step++ {
		legacySpoolWorkPending(test, preparation)
		complete, err := preparation.Step(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if complete {
			plan, err := preparation.Plan()
			if err != nil || !plan.Valid() {
				test.Fatal("completed spool did not expose its sealed plan", err)
			}
			return plan
		}
	}
	test.Fatal("spool preparation exceeded its two bounded phases")
	return LegacyByteBackfill{}
}

func legacyPreparationRequireReleasedInputs(test testing.TB, input any, allowSource bool) {
	test.Helper()
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	sourceType := reflect.TypeOf((*VerifiedDocStream)(nil)).Elem()
	proofType := reflect.TypeOf(VerifiedSessionDoc{})
	segmentsType := reflect.TypeOf(canonicalSegments{})
	var inspect func(reflect.Value)
	inspect = func(value reflect.Value) {
		if !value.IsValid() || value.IsZero() {
			return
		}
		if value.Type().Implements(sourceType) {
			if !allowSource {
				test.Fatal("metadata seed retained a stream source")
			}
			return
		}
		if value.Type().Implements(contextType) || value.Type() == proofType || value.Type() == segmentsType || value.Kind() == reflect.Func {
			test.Fatalf("completed preparation or seed retained request input of type %s", value.Type())
		}
		switch value.Kind() {
		case reflect.Pointer, reflect.Interface:
			inspect(value.Elem())
		case reflect.Struct:
			for field := 0; field < value.NumField(); field++ {
				inspect(value.Field(field))
			}
		case reflect.Slice, reflect.Array:
			for position := 0; position < value.Len(); position++ {
				inspect(value.Index(position))
			}
		}
	}
	inspect(reflect.ValueOf(input))
}

func TestLegacyByteSpoolPreparationBoundsPhasesAndDrainedParity(test *testing.T) {
	if ConversationManifestChunkBytes != 512<<10 {
		test.Fatal("spool work block is no longer the original 512 KiB block")
	}
	proof, original := legacyDocByteBackfillSizedFixture(test, 9*ConversationManifestChunkBytes+17)
	_, stream := chunkVerifierParts(test, original)
	plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	originalRetained, err := plan.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	constructorCtx, cancelConstructor := context.WithCancel(context.Background())
	source := &legacySpoolWorkSource{legacyByteSpoolTestSource: &legacyByteSpoolTestSource{appendMode: "mutate-input"}, test: test, ctx: constructorCtx}
	preparation, err := NewLegacyByteSpoolPreparation(constructorCtx, plan, source)
	cancelConstructor()
	if err != nil || preparation == nil || len(source.calls) != 0 || preparation.SourceBytes() != 0 || preparation.Reference() != proof.DocumentRef() || preparation.PlanKey() != key {
		test.Fatal("constructor touched the source, lost identity, or exposed progress", err)
	}
	for step := 0; step < 2*spec.Blocks; step++ {
		legacySpoolWorkPending(test, preparation)
		before := *preparation
		calls, written := len(source.calls), preparation.SourceBytes()
		retained, err := preparation.RetainedBytes()
		if err != nil || retained <= 0 || retained > originalRetained+2*ConversationManifestChunkBytes+(64<<10) {
			test.Fatalf("step %d retained unbounded preparation storage: %d, %v", step, retained, err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		complete, err := preparation.Step(canceled)
		after, retainedError := preparation.RetainedBytes()
		if complete || !errors.Is(err, context.Canceled) || retainedError != nil || after != retained || preparation.SourceBytes() != written || len(source.calls) != calls || !reflect.DeepEqual(before, *preparation) {
			test.Fatal("pre-canceled step mutated or poisoned pending spool work", err, retainedError)
		}
		ctx, cancel := context.WithCancel(context.Background())
		source.ctx = ctx
		complete, err = preparation.Step(ctx)
		cancel()
		if err != nil || complete != (step == 2*spec.Blocks-1) {
			test.Fatalf("step %d changed exact two-phase completion: complete=%t, error=%v", step, complete, err)
		}
		ordinal := step
		if step < spec.Blocks {
			wantLength := min(ConversationManifestChunkBytes, len(stream)-ordinal*ConversationManifestChunkBytes)
			want := legacySpoolWorkCall{append: true, span: canonicalSpan{offset: ordinal * ConversationManifestChunkBytes, length: wantLength}}
			if len(source.calls) != calls+1 || source.calls[calls] != want || len(source.reads) != 0 || preparation.SourceBytes() != int64(min((ordinal+1)*ConversationManifestChunkBytes, len(stream))) {
				test.Fatalf("write step %d did not append exactly one original block before all readback", step)
			}
		} else {
			ordinal -= spec.Blocks
			want := []legacySpoolWorkCall{{span: canonicalSpan{offset: ordinal * ConversationManifestChunkBytes, length: min(ConversationManifestChunkBytes, len(stream)-ordinal*ConversationManifestChunkBytes)}}}
			if complete {
				want = append(want, legacySpoolWorkCall{span: canonicalSpan{offset: len(stream)}}, legacySpoolWorkCall{span: canonicalSpan{offset: len(stream), length: 1}})
			}
			if !reflect.DeepEqual(source.calls[calls:], want) || len(source.appends) != spec.Blocks || preparation.SourceBytes() != spec.StreamBytes {
				test.Fatalf("verification step %d exceeded one block or misplaced exact-end checks: %v", ordinal, source.calls[calls:])
			}
		}
	}
	frozen, err := preparation.Plan()
	if err != nil || !frozen.Valid() || frozen.spec != spec || frozen.key != key || frozen.source != source || !bytes.Equal(source.body, stream) || !bytes.Equal(proof.Bytes(), original) {
		test.Fatal("completed spool changed canonical source, reference, key, or source ownership", err)
	}
	legacyPreparationRequireReleasedInputs(test, preparation, true)
	retained, err := preparation.RetainedBytes()
	if err != nil || retained <= 0 || retained >= ConversationManifestChunkBytes/8 {
		test.Fatal("completed spool retained original canonical input or source-owned storage", retained, err)
	}
	drainedSource := &legacySpoolWorkSource{legacyByteSpoolTestSource: &legacyByteSpoolTestSource{appendMode: "mutate-input"}, test: test}
	drained, err := plan.Spool(context.Background(), drainedSource)
	if err != nil || drained.spec != frozen.spec || drained.key != frozen.key || !reflect.DeepEqual(drained.descriptors, frozen.descriptors) || !reflect.DeepEqual(drainedSource.calls, source.calls) || !bytes.Equal(drainedSource.body, source.body) {
		test.Fatal("Spool no longer drains the identical phased state, bytes, or callback trace", err)
	}
	if unsafe.SliceData(drained.descriptors) == unsafe.SliceData(frozen.descriptors) {
		test.Fatal("independent spool preparations shared descriptor backing")
	}
	source.ctx = context.Background()
	for ordinal := spec.Blocks - 1; ordinal >= 0; ordinal-- {
		block, err := frozen.Block(source.ctx, ordinal)
		if err != nil {
			test.Fatal(err)
		}
		legacyDocByteBackfillRequireBlock(test, block, spec, key, ordinal, stream)
		mutable := block.Bytes()
		clear(mutable)
		legacyDocByteBackfillRequireBlock(test, block, spec, key, ordinal, stream)
	}
	calls := len(source.calls)
	for repeat := 0; repeat < 2; repeat++ {
		complete, err := preparation.Step(context.Background())
		if err != nil || !complete || len(source.calls) != calls || preparation.SourceBytes() != spec.StreamBytes {
			test.Fatal("completed step repeated source work or changed progress", err)
		}
	}
	source.body = append(source.body, bytes.Repeat([]byte("source-owned"), ConversationManifestChunkBytes)...)
	if after, err := preparation.RetainedBytes(); err != nil || after != retained {
		test.Fatal("source growth changed spool-owned retained accounting", after, err)
	}
}

func TestLegacyByteSpoolPreparationEmptyCompletesWithExtentChecksInOneStep(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 0)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	for name, source := range map[string]*legacyByteSpoolTestSource{
		"integrity-end": {}, "eof-end": {extentError: io.EOF}, "unexpected-eof-end": {extentError: io.ErrUnexpectedEOF},
		"extra-byte": {body: []byte("x")}, "zero-read-error": {endError: errors.New("zero extent failed")},
	} {
		test.Run(name, func(test *testing.T) {
			preparation, err := NewLegacyByteSpoolPreparation(context.Background(), plan, source)
			if err != nil || preparation == nil || len(source.reads) != 0 {
				test.Fatal("empty spool constructor performed extent reads", err)
			}
			legacySpoolWorkPending(test, preparation)
			complete, err := preparation.Step(context.Background())
			if len(source.appends) != 0 || preparation.SourceBytes() != 0 {
				test.Fatal("empty spool wrote a block")
			}
			if name == "extra-byte" || name == "zero-read-error" {
				if complete || err == nil {
					test.Fatal("empty spool ignored an extent failure")
				}
				failed, planError := preparation.Plan()
				legacyByteSpoolRequireZero(test, failed, planError, ErrIntegrity)
				return
			}
			got, planError := preparation.Plan()
			if err != nil || !complete || planError != nil || !got.Valid() || got.spec != plan.spec || got.key != plan.key || !reflect.DeepEqual(source.reads, []canonicalSpan{{}, {length: 1}}) {
				test.Fatal("empty spool did not seal after exactly one step and both extent checks", err, planError)
			}
			legacyPreparationRequireReleasedInputs(test, preparation, true)
		})
	}
}

func TestLegacyByteSpoolPreparationFaultsPoisonAndPreserveErrorClass(test *testing.T) {
	proof, _ := legacyDocByteBackfillSizedFixture(test, ConversationManifestChunkBytes+17)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	failure := errors.New("spool work callback failed")
	for name, fixture := range map[string]struct {
		source   legacyByteSpoolTestSource
		want     error
		prepared bool
	}{
		"append-error":                     {source: legacyByteSpoolTestSource{appendError: failure}, want: failure},
		"append-integrity":                 {source: legacyByteSpoolTestSource{appendError: ErrIntegrity}, want: ErrIntegrity},
		"drop":                             {source: legacyByteSpoolTestSource{appendMode: "drop"}, want: ErrIntegrity, prepared: true},
		"truncate":                         {source: legacyByteSpoolTestSource{appendMode: "truncate"}, want: ErrIntegrity, prepared: true},
		"corrupt":                          {source: legacyByteSpoolTestSource{appendMode: "corrupt"}, want: ErrIntegrity, prepared: true},
		"rewrite-first-after-later-append": {source: legacyByteSpoolTestSource{appendMode: "rewrite-first"}, want: ErrIntegrity, prepared: true},
		"read-error":                       {source: legacyByteSpoolTestSource{readError: failure}, want: failure},
		"read-integrity":                   {source: legacyByteSpoolTestSource{readError: ErrIntegrity}, want: ErrIntegrity, prepared: true},
		"read-short":                       {source: legacyByteSpoolTestSource{readMode: "short"}, want: ErrIntegrity, prepared: true},
		"read-long":                        {source: legacyByteSpoolTestSource{readMode: "long"}, want: ErrIntegrity, prepared: true},
		"read-empty":                       {source: legacyByteSpoolTestSource{readMode: "empty"}, want: ErrIntegrity, prepared: true},
		"read-hash":                        {source: legacyByteSpoolTestSource{readMode: "corrupt"}, want: ErrIntegrity, prepared: true},
		"zero-extent-error":                {source: legacyByteSpoolTestSource{endError: failure}, want: failure},
		"zero-extent-integrity":            {source: legacyByteSpoolTestSource{endError: ErrIntegrity}, want: ErrIntegrity},
		"extra-extent-error":               {source: legacyByteSpoolTestSource{extentError: failure}, want: failure},
	} {
		test.Run(name, func(test *testing.T) {
			source := &fixture.source
			preparation, err := NewLegacyByteSpoolPreparation(context.Background(), plan, source)
			if err != nil || preparation == nil {
				test.Fatal(err)
			}
			var complete bool
			for step := 0; step < 2*plan.spec.Blocks; step++ {
				legacySpoolWorkPending(test, preparation)
				complete, err = preparation.Step(context.Background())
				if err != nil || complete {
					break
				}
			}
			if complete || !errors.Is(err, fixture.want) || errors.Is(err, ErrPreparedDocRangeObject) != fixture.prepared {
				test.Fatal("failed spool changed source-vs-original integrity classification or completed", err)
			}
			failed, planError := preparation.Plan()
			legacyByteSpoolRequireZero(test, failed, planError, ErrIntegrity)
			appends, reads := len(source.appends), len(source.reads)
			source.appendError, source.readError, source.endError, source.extentError = nil, nil, nil, nil
			source.appendMode, source.readMode = "", ""
			complete, err = preparation.Step(context.Background())
			if complete || err == nil || len(source.appends) != appends || len(source.reads) != reads {
				test.Fatal("repairing callbacks revived poisoned spool work", err)
			}
		})
	}
}

func TestLegacyByteSpoolPreparationDetectsMutationBeforeVerification(test *testing.T) {
	proof, raw := legacyDocByteBackfillSizedFixture(test, 2*ConversationManifestChunkBytes+17)
	_, stream := chunkVerifierParts(test, raw)
	plan, _, _ := legacyDocByteBackfillRequirePlan(test, proof, stream)
	for _, mutation := range []string{"rewrite-first", "truncate-last", "extend", "prepopulated"} {
		test.Run(mutation, func(test *testing.T) {
			source := &legacyByteSpoolTestSource{}
			if mutation == "prepopulated" {
				source.body = bytes.Clone(stream)
			}
			preparation, err := NewLegacyByteSpoolPreparation(context.Background(), plan, source)
			if err != nil {
				test.Fatal(err)
			}
			for ordinal := 0; ordinal < plan.spec.Blocks; ordinal++ {
				complete, err := preparation.Step(context.Background())
				if complete || err != nil || len(source.reads) != 0 {
					test.Fatal("write phase performed verification or exposed a plan", err)
				}
				legacySpoolWorkPending(test, preparation)
			}
			switch mutation {
			case "rewrite-first":
				source.body[0] ^= 1
			case "truncate-last":
				source.body = source.body[:len(source.body)-1]
			case "extend":
				source.body = append(source.body, 0)
			}
			var complete bool
			for ordinal := 0; ordinal < plan.spec.Blocks; ordinal++ {
				complete, err = preparation.Step(context.Background())
				if complete || err != nil {
					break
				}
			}
			if complete || !errors.Is(err, ErrIntegrity) {
				test.Fatal("verification accepted source mutation after its writes", err)
			}
			failed, planError := preparation.Plan()
			legacyByteSpoolRequireZero(test, failed, planError, ErrIntegrity)
		})
	}
}

func TestLegacyByteSpoolPreparationActiveCancellationPoisonsEveryPhase(test *testing.T) {
	proof, _ := legacyDocByteBackfillSizedFixture(test, ConversationManifestChunkBytes+17)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	for _, phase := range []string{"write", "read"} {
		count := plan.spec.Blocks
		if phase == "read" {
			count += 2
		}
		for cancelAt := 1; cancelAt <= count; cancelAt++ {
			test.Run(fmt.Sprintf("%s-%d", phase, cancelAt), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source := &legacyByteSpoolTestSource{cancel: cancel}
				if phase == "write" {
					source.cancelAppendAt = cancelAt
				} else {
					source.cancelReadAt = cancelAt
				}
				preparation, err := NewLegacyByteSpoolPreparation(context.Background(), plan, source)
				if err != nil {
					test.Fatal(err)
				}
				var complete bool
				for step := 0; step < 2*plan.spec.Blocks; step++ {
					complete, err = preparation.Step(ctx)
					if complete || err != nil {
						break
					}
				}
				if complete || !errors.Is(err, context.Canceled) || phase == "write" && (len(source.appends) != cancelAt || len(source.reads) != 0) || phase == "read" && len(source.reads) != cancelAt {
					test.Fatal("active cancellation continued source callbacks or sealed a plan", err)
				}
				failed, planError := preparation.Plan()
				legacyByteSpoolRequireZero(test, failed, planError, ErrIntegrity)
				appends, reads := len(source.appends), len(source.reads)
				source.cancel = nil
				complete, err = preparation.Step(context.Background())
				if complete || err == nil || len(source.appends) != appends || len(source.reads) != reads {
					test.Fatal("fresh request context revived actively canceled spool work", err)
				}
			})
		}
	}
}

func TestLegacyByteSpoolPreparationRejectsUnsealedAndCanceledConstruction(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 1)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		test.Fatal(err)
	}
	var decoded LegacyByteBackfill
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		test.Fatal(err)
	}
	unsealed := plan
	unsealed.sealed = false
	for name, candidate := range map[string]LegacyByteBackfill{"zero": {}, "serialized": decoded, "copied-without-seal": unsealed} {
		test.Run(name, func(test *testing.T) {
			source := &legacyByteSpoolTestSource{}
			preparation, err := NewLegacyByteSpoolPreparation(context.Background(), candidate, source)
			if !errors.Is(err, ErrIntegrity) || preparation != nil || len(source.reads) != 0 || len(source.appends) != 0 {
				test.Fatal("unsealed plan acquired spool work or reached its source", err)
			}
		})
	}
	if preparation, err := NewLegacyByteSpoolPreparation(context.Background(), plan, nil); !errors.Is(err, ErrIntegrity) || preparation != nil {
		test.Fatal("nil source acquired spool work", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &legacyByteSpoolTestSource{}
	if preparation, err := NewLegacyByteSpoolPreparation(ctx, plan, source); !errors.Is(err, context.Canceled) || preparation != nil || len(source.appends) != 0 || len(source.reads) != 0 {
		test.Fatal("pre-canceled constructor reached the source or returned work", err)
	}
	for _, preparation := range []*LegacyByteSpoolPreparation{nil, {}} {
		if complete, err := preparation.Step(context.Background()); complete || !errors.Is(err, ErrIntegrity) {
			test.Fatal("zero spool preparation admitted work", err)
		}
		invalid, err := preparation.Plan()
		legacyByteSpoolRequireZero(test, invalid, err, ErrIntegrity)
		if preparation.Reference() != (DocumentRef{}) || preparation.PlanKey() != "" || preparation.SourceBytes() != 0 {
			test.Fatal("zero spool preparation exposed source metadata")
		}
		if retained, err := preparation.RetainedBytes(); retained != 0 || !errors.Is(err, ErrIntegrity) {
			test.Fatal("zero spool preparation exposed retained accounting", retained, err)
		}
	}
}
