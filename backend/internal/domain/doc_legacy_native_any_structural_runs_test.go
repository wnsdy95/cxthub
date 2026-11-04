package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyNativeAnyStructuralRunYieldAndFreshSource(test *testing.T) {
	const depth = 1000
	raw := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
	parser, err := newLegacyNativeAny(0, len(raw), 0, 64)
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := buffer.input()
	var scanner legacyJSONValueScanner
	input := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - 2*depth}
	ready, err := parser.step(input.ctx, input, &scanner)
	if ready || err != nil || parser.openingRemaining != depth || parser.frame != nil || parser.position != 0 ||
		len(buffer.reads) != 1 || input.workBytes > ConversationManifestChunkBytes {
		test.Fatal("authenticated structural run did not yield before frame construction", ready, err, parser.openingRemaining, input.workBytes)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	input = &legacyNormalizationInput{ctx: canceled, source: source}
	if ready, err := parser.step(canceled, input, &scanner); ready || !errors.Is(err, context.Canceled) ||
		parser.openingRemaining != depth || parser.frame != nil || input.workBytes != 0 || len(buffer.reads) != 1 {
		test.Fatal("canceled structural work consumed facts or reloaded source", ready, err)
	}
	buffer.body[depth] = '1'
	frameCost := int(unsafe.Sizeof(legacyNativeAnyFrame{})) + int(unsafe.Sizeof(legacyNativeAnyArray{}))
	input = &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - frameCost}
	ready, err = parser.step(input.ctx, input, &scanner)
	if ready || err != nil || parser.openingRemaining != depth-1 || parser.depth != 1 || parser.position != 1 ||
		len(buffer.reads) != 1 || input.workBytes != ConversationManifestChunkBytes {
		test.Fatal("metadata-only progress reconsumed source or escaped its exact quantum", ready, err, parser.openingRemaining)
	}
	input = &legacyNormalizationInput{ctx: context.Background(), source: source}
	ready, err = parser.step(input.ctx, input, &scanner)
	if ready || !errors.Is(err, ErrPreparedDocRangeObject) || parser.finished || parser.pending ||
		parser.openingRemaining != 0 || parser.position != depth || len(buffer.reads) != 2 || input.loads != 0 {
		test.Fatal("next source consumption trusted a retained page or missed mutation", ready, err, parser.position, len(buffer.reads), input.loads)
	}
}

func TestLegacyNativeAnyStructuralRunResetAndFences(test *testing.T) {
	parser := &legacyNativeAny{finished: true, openingRemaining: 3, closingRemaining: 2, closingByte: ']'}
	if err := parser.start(0, 2, 0, 64); err != nil || parser.openingRemaining != 0 || parser.closingRemaining != 0 || parser.closingByte != 0 {
		test.Fatal("native restart retained structural facts from the previous value", err)
	}
	for _, fixture := range []struct {
		name  string
		alter func(*legacyNativeAny)
	}{
		{name: "over-depth-opening-run", alter: func(parser *legacyNativeAny) { parser.openingRemaining = 10001 }},
		{name: "unowned-closing-run", alter: func(parser *legacyNativeAny) { parser.closingRemaining, parser.closingByte = 1, ']' }},
		{name: "invalid-closing-kind", alter: func(parser *legacyNativeAny) {
			array, _ := newLegacyNativeAnyArray(0)
			parser.frame, parser.depth = &legacyNativeAnyFrame{array: array, phase: legacyNativeAnyFirst}, 1
			parser.closingRemaining, parser.closingByte = 1, '?'
		}},
		{name: "finished-with-unapplied-facts", alter: func(parser *legacyNativeAny) { parser.finished, parser.openingRemaining = true, 1 }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			parser, err := newLegacyNativeAny(0, 2, 0, 64)
			if err != nil {
				test.Fatal(err)
			}
			fixture.alter(parser)
			buffer := &legacySourceProofBuffer{body: []byte("[]")}
			input := &legacyNormalizationInput{ctx: context.Background(), source: buffer.input()}
			var scanner legacyJSONValueScanner
			if ready, err := parser.step(input.ctx, input, &scanner); ready || !errors.Is(err, ErrIntegrity) || len(buffer.reads) != 0 || input.workBytes != 0 {
				test.Fatal("invalid structural state consumed source or published a value", ready, err, len(buffer.reads), input.workBytes)
			}
		})
	}
}

func TestLegacyNativeAnyStructuralRunPaidRootClose(test *testing.T) {
	for _, raw := range []string{"[]", "{}"} {
		test.Run(raw, func(test *testing.T) {
			parser, err := newLegacyNativeAny(0, len(raw), 0, 64)
			if err != nil {
				test.Fatal(err)
			}
			input := &legacyNormalizationInput{ctx: context.Background()}
			if ready, err := parser.open(raw[0], input); !ready || err != nil {
				test.Fatal("could not open actual root for exact closing boundary", ready, err)
			}
			buffer := &legacySourceProofBuffer{body: []byte(raw)}
			source := buffer.input()
			input = &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - 1}
			var scanner legacyJSONValueScanner
			ready, err := parser.step(input.ctx, input, &scanner)
			if ready || err != nil || input.workBytes != ConversationManifestChunkBytes || !parser.finished || parser.frame != nil ||
				parser.depth != 0 || parser.position != len(raw) || !parser.rootReserved || parser.closingRemaining != 0 || parser.closingByte != 0 {
				test.Fatal("already-paid final closer did not preserve finished-before-handoff boundary", ready, err, parser.finished)
			}
			input = &legacyNormalizationInput{ctx: context.Background(), source: source}
			if ready, err := parser.step(input.ctx, input, &scanner); !ready || err != nil || input.workBytes != 0 || len(buffer.reads) != 1 {
				test.Fatal("finished root required another source read or closing action", ready, err, input.workBytes, len(buffer.reads))
			}
		})
	}
}
