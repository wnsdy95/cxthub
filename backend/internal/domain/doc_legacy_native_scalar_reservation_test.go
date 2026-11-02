package domain

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLegacyNativeScalarSmallMigrationReservesSharedQuantum(test *testing.T) {
	prior := &strings.Builder{}
	prior.Grow(ConversationManifestChunkBytes)
	prior.WriteString(strings.Repeat("x", prior.Cap()))
	scalar := &legacyNativeScalar{text: prior, position: prior.Len(), end: prior.Len() + 73, maximum: 3 * (prior.Len() + 73)}
	partial := &legacyNormalizationInput{workBytes: 1}
	ready, err := scalar.reserve(context.Background(), partial, 1)
	if err != nil || ready || scalar.text != prior || scalar.previous != nil || scalar.copied != 0 || partial.workBytes != 1 {
		test.Fatal("small migration allocated before its copy could fit the shared quantum", ready, err, partial.workBytes)
	}
	fresh := &legacyNormalizationInput{}
	ready, err = scalar.reserve(context.Background(), fresh, 1)
	if err != nil || ready || scalar.previous != nil || fresh.workBytes != prior.Len() || scalar.text.String() != prior.String() ||
		scalar.text.Cap() >= 2*prior.Cap() || scalar.text.Cap() < prior.Len()+73 {
		test.Fatal("fresh migration lost bytes or ignored the remaining source-size hint", ready, err, fresh.workBytes, scalar.text.Cap())
	}
	next := &legacyNormalizationInput{}
	ready, err = scalar.reserve(context.Background(), next, 1)
	if err != nil || !ready || next.workBytes != 0 || scalar.copyDeferred {
		test.Fatal("completed copy required more than one final migration yield", ready, err, next.workBytes)
	}
}

func TestLegacyNativeScalarExpandedTextKeepsGeometricGrowth(test *testing.T) {
	prior := &strings.Builder{}
	prior.Grow(128)
	prior.WriteString(strings.Repeat("x", prior.Cap()))
	scalar := &legacyNativeScalar{text: prior, expanded: true, position: 199, end: 200, maximum: 1024}
	input := &legacyNormalizationInput{}
	ready, err := scalar.reserve(context.Background(), input, 1)
	if err != nil || !ready || scalar.previous != nil || scalar.text.Cap() < 2*prior.Cap() || scalar.text.String() != prior.String() {
		test.Fatal("expanded UTF8 text used an undersized raw-source hint", ready, err, scalar.text.Cap())
	}
}

func TestLegacyNativeScalarExpandedOutputMatchesNative(test *testing.T) {
	value := bytes.Repeat([]byte{0xff}, 2*ConversationManifestChunkBytes+73)
	raw := append([]byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","output":"`), value...)
	raw = append(raw, []byte(`"}]}`)...)
	normalizer, input := legacyNativeScalarReviewStart(test, raw)
	got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
	legacyNativeScalarReviewCompare(test, raw, got, err, true)
	var physical int64
	for _, read := range input.reads {
		physical += int64(read[1])
	}
	test.Logf("raw bytes=%d decoded bytes=%d authenticated bytes read=%d", len(raw), 3*len(value), physical)
}

func TestLegacyNativeScalarRuneCapacityIsReservedBeforeSourceRead(test *testing.T) {
	prior := &strings.Builder{}
	prior.Grow(128)
	prior.WriteString(strings.Repeat("x", prior.Cap()-2))
	raw := append([]byte(prior.String()), 0xff, 'z')
	scalar := &legacyNativeScalar{text: prior, position: prior.Len(), end: len(raw), maximum: 3 * len(raw)}
	buffer := &legacySourceProofBuffer{body: raw}
	source := buffer.input()
	read := source.Read
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if scalar.previous != nil || scalar.text.Cap()-scalar.text.Len() < utf8.UTFMax {
			test.Fatal("source read preceded the rune-capacity migration")
		}
		return read(ctx, offset, length)
	}
	input := &legacyNormalizationInput{ctx: context.Background(), source: source}
	complete, err := scalar.step(context.Background(), input)
	if err != nil || !complete || scalar.text.String() != prior.String()+"�z" || !scalar.expanded || input.loads != 1 {
		test.Fatal("rune reservation changed native replacement bytes", complete, err, input.loads)
	}
}
