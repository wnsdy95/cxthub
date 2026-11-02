package domain

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
	"unsafe"
)

type legacyNativeScalar struct {
	text         *strings.Builder
	previous     *strings.Builder
	position     int
	end          int
	maximum      int
	copied       int
	pending      [12]byte
	buffered     int
	expanded     bool
	copyDeferred bool
}

func (scalar *legacyNativeScalar) reserve(ctx context.Context, input *legacyNormalizationInput, additional int) (bool, error) {
	if scalar.previous == nil && (scalar.text == nil || scalar.text.Cap()-scalar.text.Len() < additional) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if scalar.text != nil && scalar.text.Len() <= ConversationManifestChunkBytes && scalar.text.Len() > ConversationManifestChunkBytes-input.workBytes {
			scalar.copyDeferred = true
			return false, nil
		}
		capacity := max(128, additional)
		if scalar.text == nil && scalar.end-scalar.position > LegacyCanonicalEncodingBytes {
			capacity = max(capacity, min(ConversationManifestChunkBytes, scalar.end-scalar.position))
		}
		needed := additional
		if scalar.text != nil {
			needed += scalar.text.Len()
			growth := scalar.maximum
			if scalar.text.Cap() <= scalar.maximum/2 {
				growth = 2 * scalar.text.Cap()
			}
			if !scalar.expanded {
				growth = min(growth, scalar.text.Len()+scalar.end-scalar.position)
			}
			capacity = max(capacity, growth, needed)
		}
		capacity = min(capacity, scalar.maximum)
		if capacity < needed {
			return false, ErrIntegrity
		}
		next := &strings.Builder{}
		next.Grow(capacity)
		scalar.previous, scalar.text, scalar.copied = scalar.text, next, 0
		if scalar.previous != nil && scalar.previous.Len() == 0 {
			scalar.previous = nil
		}
	}
	for scalar.previous != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		length := min(scalar.previous.Len()-scalar.copied, canonicalCheckBytes, ConversationManifestChunkBytes-input.workBytes)
		if length <= 0 {
			scalar.copyDeferred = true
			return false, nil
		}
		_, _ = scalar.text.WriteString(scalar.previous.String()[scalar.copied : scalar.copied+length])
		scalar.copied += length
		input.workBytes += length
		if scalar.copied == scalar.previous.Len() {
			scalar.previous = nil
			if scalar.copyDeferred {
				scalar.copyDeferred = false
				return false, nil
			}
		}
	}
	return true, nil
}

func (scalar *legacyNativeScalar) fill(input *legacyNormalizationInput) (bool, error) {
	for scalar.buffered < len(scalar.pending) && scalar.position+scalar.buffered < scalar.end {
		if input.workBytes >= ConversationManifestChunkBytes {
			return false, nil
		}
		current, err := input.current(scalar.position + scalar.buffered)
		if errors.Is(err, errLegacyNormalizationYield) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		scalar.pending[scalar.buffered] = current
		scalar.buffered++
		input.semanticBytes++
	}
	return true, nil
}

func (scalar *legacyNativeScalar) step(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	checked := input.workBytes
	for input.workBytes < ConversationManifestChunkBytes {
		if input.workBytes-checked >= canonicalCheckBytes {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			checked = input.workBytes
		}
		if scalar.position == scalar.end && scalar.buffered == 0 && scalar.previous == nil {
			if scalar.text == nil {
				scalar.text = &strings.Builder{}
			}
			return true, ctx.Err()
		}
		remaining := scalar.maximum
		if scalar.text != nil {
			remaining -= scalar.text.Len()
		}
		if remaining <= 0 {
			return false, ErrIntegrity
		}
		ready, err := scalar.reserve(ctx, input, min(utf8.UTFMax, remaining))
		if err != nil || !ready {
			return false, err
		}
		if scalar.buffered == 0 {
			raw, err := input.scalarPart(scalar.position, scalar.end, scalar.text.Cap()-scalar.text.Len())
			if errors.Is(err, errLegacyNormalizationYield) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			length := 0
			for length < len(raw) && raw[length] >= 0x20 && raw[length] < utf8.RuneSelf && raw[length] != '\\' && raw[length] != '"' {
				length++
			}
			if length > 0 {
				_, _ = scalar.text.Write(raw[:length])
				scalar.position += length
				input.workBytes += 2 * length
				input.semanticBytes += length
				continue
			}
		}
		ready, err = scalar.fill(input)
		if err != nil || !ready {
			return false, err
		}
		var character rune
		var width int
		if scalar.pending[0] >= utf8.RuneSelf {
			character, width = utf8.DecodeRune(scalar.pending[:scalar.buffered])
		} else {
			character, width, _, err = decodeDocDisplayStringRune(scalar.pending[:scalar.buffered])
			if err != nil {
				return false, err
			}
		}
		encoded := utf8.RuneLen(character)
		scalar.expanded = scalar.expanded || encoded > width
		if encoded > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		ready, err = scalar.reserve(ctx, input, encoded)
		if err != nil || !ready {
			return false, err
		}
		if encoded > ConversationManifestChunkBytes-input.workBytes {
			return false, nil
		}
		_, _ = scalar.text.WriteRune(character)
		input.workBytes += encoded
		scalar.position += width
		copy(scalar.pending[:], scalar.pending[width:scalar.buffered])
		scalar.buffered -= width
	}
	return false, nil
}

func (scalar *legacyNativeScalar) retained(estimate *docRangeHeaderHeapEstimate) {
	estimate.allocation(1, int64(unsafe.Sizeof(*scalar)))
	for _, builder := range []*strings.Builder{scalar.text, scalar.previous} {
		if builder != nil {
			estimate.allocation(1, int64(unsafe.Sizeof(*builder)))
			estimate.allocation(int64(builder.Cap()), 1)
		}
	}
}
