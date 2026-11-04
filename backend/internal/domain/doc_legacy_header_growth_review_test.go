package domain

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

type legacyHeaderGrowthReviewState struct {
	length         int
	position       int
	output         int
	copying        bool
	field          int
	phase          uint8
	pending        int
	scalarFrames   int
	scalarPosition int
}

func legacyHeaderGrowthReviewSnapshot(header *legacyNormalizationHeader) legacyHeaderGrowthReviewState {
	state := legacyHeaderGrowthReviewState{
		length: header.length, position: header.position, output: len(header.output), copying: header.copying,
	}
	if header.encoding != nil {
		state.field, state.phase = header.encoding.field, header.encoding.phase
		state.pending = len(header.encoding.pending)
		if scalar := header.encoding.scalar; scalar != nil {
			state.scalarFrames = scalar.frames.length()
			if state.scalarFrames != 0 {
				state.scalarPosition = scalar.frames.at(state.scalarFrames - 1).position
			}
		}
	}
	return state
}

func TestLegacyHeaderGrowthReviewEscapedScalarYieldsBeforePendingCopies(test *testing.T) {
	payload := strings.Repeat("<", 14000)
	envelope := CIREnvelope{CIRVersion: CIRVersionV2, SourceModel: payload}
	expected, err := canonicalJSON(envelope)
	if err != nil {
		test.Fatal(err)
	}
	header := &legacyNormalizationHeader{encoding: legacyEnvelopeEncodingFixture(test, envelope)}
	ctx, cancel := context.WithCancel(context.Background())
	output, complete, err := header.step(ctx)
	cancel()
	if err != nil || complete || output != nil {
		test.Fatal("growth-heavy header did not yield without provisional output", complete, len(output), err)
	}
	if header.encoding == nil || header.encoding.scalar == nil || len(header.encoding.pending) == 0 {
		test.Fatal("one header quantum consumed the escaped scalar and pending-copy phases without reserving growth migrations")
	}
	scalar := header.encoding.scalar
	text, stringRoot := scalar.root.(string)
	if !stringRoot || text != payload || scalar.frames.length() != 1 || len(header.encoding.pending) > LegacyCanonicalEncodingBytes {
		test.Fatal("yield did not preserve the actual source-model scalar and its bounded pending chunk")
	}
	frame := *scalar.frames.at(0)
	if frame.kind != 's' || frame.position <= 0 || frame.position >= len(payload) {
		test.Fatal("header yielded before real scalar work or after consuming the entire growth-heavy string", frame.kind, frame.position)
	}
	for stepIndex := 0; stepIndex < 16; stepIndex++ {
		before := legacyHeaderGrowthReviewSnapshot(header)
		stepCtx, stop := context.WithCancel(context.Background())
		output, complete, err := header.step(stepCtx)
		stop()
		if err != nil {
			test.Fatal("fresh step context lost header progress", stepIndex, err)
		}
		if retained, err := header.retained(); err != nil || retained <= 0 || retained > 16<<20 {
			test.Fatal("header lost retained ownership or exceeded the original parked heap budget", retained, err)
		}
		if !complete {
			if output != nil || legacyHeaderGrowthReviewSnapshot(header) == before {
				test.Fatal("incomplete header exposed bytes or stalled", stepIndex, len(output))
			}
			continue
		}
		if !bytes.Equal(output, expected) || !bytes.Equal(header.header, expected) {
			test.Fatal("growth-aware yields changed the native canonical envelope bytes", len(output), len(expected))
		}
		if header.encoding != nil || header.first != nil || header.last != nil || header.heap.bytes != 0 {
			test.Fatal("completed header retains encoder or staging chunks")
		}
		output[0] ^= 1
		if !bytes.Equal(header.header, expected) {
			test.Fatal("completed caller output aliases the replay header")
		}
		return
	}
	test.Fatal("growth-heavy header exhausted its fixed sixteen-step progress bound")
}
