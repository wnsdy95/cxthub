package domain

import (
	"context"
	"errors"
	"io"
)

type legacyNormalizationInput struct {
	ctx               context.Context
	source            LegacyByteSource
	ordinals          [2]int
	pages             [2][]byte
	window            []byte
	windowStart       int
	next              int
	failure           error
	semanticBytes     int
	workBytes         int
	fields            int
	values            int
	loads             int
	unbounded         bool
	eventsRunning     *legacyNativeEventList
	eventsPending     *legacyNativeEventList
	eventsFieldLimit  int
	numberDiagnostics legacyNativeNumberDiagnostics
}

var errLegacyNormalizationYield = errors.New("legacy normalization quantum exhausted")

func (input *legacyNormalizationInput) fieldActionLimit(fallback int) int {
	if input.eventsRunning != nil && input.eventsFieldLimit > 0 {
		return input.eventsFieldLimit
	}
	return fallback
}

func (input *legacyNormalizationInput) part(ordinal int) ([]byte, error) {
	partBytes := input.source.partBytes()
	if partBytes == 0 {
		return nil, ErrIntegrity
	}
	for index, body := range input.pages {
		if body != nil && input.ordinals[index] == ordinal {
			input.window, input.windowStart = body, ordinal*partBytes
			return body, nil
		}
	}
	if !input.unbounded && input.loads >= 2 {
		return nil, errLegacyNormalizationYield
	}
	body, err := readLegacyByteSourcePart(input.ctx, input.source, ordinal)
	if err != nil {
		input.failure = errors.Join(ErrPreparedDocRangeObject, err)
		return nil, input.failure
	}
	input.loads++
	index := input.next
	input.pages[index], input.ordinals[index] = body, ordinal
	input.next = 1 - index
	input.window, input.windowStart = body, ordinal*partBytes
	return body, nil
}

func (input *legacyNormalizationInput) current(position int) (byte, error) {
	partBytes := input.source.partBytes()
	if partBytes == 0 || position < 0 || position >= input.source.Bytes {
		return 0, ErrIntegrity
	}
	offset := position - input.windowStart
	if offset < 0 || offset >= len(input.window) {
		if _, err := input.part(position / partBytes); err != nil {
			return 0, err
		}
		offset = position - input.windowStart
	}
	input.workBytes++
	return input.window[offset], nil
}

func (input *legacyNormalizationInput) scalarPart(position, end, maximum int) ([]byte, error) {
	partBytes := input.source.partBytes()
	if partBytes == 0 || position < 0 || position >= end || end > input.source.Bytes {
		return nil, ErrIntegrity
	}
	length := min(maximum, canonicalCheckBytes, (ConversationManifestChunkBytes-input.workBytes)/2, end-position)
	if length <= 0 {
		return nil, errLegacyNormalizationYield
	}
	offset := position - input.windowStart
	if offset < 0 || offset >= len(input.window) {
		if _, err := input.part(position / partBytes); err != nil {
			return nil, err
		}
		offset = position - input.windowStart
	}
	return input.window[offset : offset+min(length, len(input.window)-offset)], nil
}

type legacyNormalizationValueReader struct {
	input    *legacyNormalizationInput
	position int
	end      int
}

func (reader *legacyNormalizationValueReader) Read(body []byte) (int, error) {
	if err := reader.input.ctx.Err(); err != nil {
		return 0, err
	}
	if len(body) == 0 {
		return 0, nil
	}
	if reader.position == reader.end {
		return 0, io.EOF
	}
	partBytes := reader.input.source.partBytes()
	if partBytes == 0 {
		return 0, ErrIntegrity
	}
	part, err := reader.input.part(reader.position / partBytes)
	if err != nil {
		return 0, err
	}
	length := min(len(body), reader.end-reader.position, len(part)-reader.position%partBytes)
	copy(body[:length], part[reader.position%partBytes:])
	reader.position += length
	reader.input.workBytes += length
	return length, nil
}

func (input *legacyNormalizationInput) canDecode(from, to int) bool {
	partBytes := input.source.partBytes()
	if partBytes == 0 || from < 0 || to <= from || to > input.source.Bytes {
		return false
	}
	length := to - from
	if length > ConversationManifestChunkBytes {
		return input.workBytes == 0
	}
	if length > ConversationManifestChunkBytes-input.workBytes {
		return false
	}
	missing := 0
	for ordinal := from / partBytes; ordinal <= (to-1)/partBytes; ordinal++ {
		cached := false
		for index, body := range input.pages {
			cached = cached || body != nil && input.ordinals[index] == ordinal
		}
		if !cached {
			missing++
		}
	}
	return input.loads+missing <= 2
}
