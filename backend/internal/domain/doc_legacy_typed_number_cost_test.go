package domain

import (
	"context"
	"errors"
	"io"
)

const legacyTypedNumberCostNumberPrefix = "-0."
const legacyTypedNumberCostNumberSuffix = "00E+0003"

type legacyTypedNumberCostSource struct {
	bytes           int
	numberBytes     int
	prefix          string
	suffix          string
	page            []byte
	parts           []ConversationManifestChunk
	visited         []bool
	readBytes       int64
	coveredBytes    int64
	loads           int
	zeroEOF         int
	oneEOF          int
	unexpectedReads int
}

func legacyTypedNumberCostNumberByte(position, extent int) byte {
	if position < len(legacyTypedNumberCostNumberPrefix) {
		return legacyTypedNumberCostNumberPrefix[position]
	}
	if position >= extent-len(legacyTypedNumberCostNumberSuffix) {
		return legacyTypedNumberCostNumberSuffix[position-(extent-len(legacyTypedNumberCostNumberSuffix))]
	}
	return "1234567890"[(position-len(legacyTypedNumberCostNumberPrefix))%10]
}

func (source *legacyTypedNumberCostSource) generate(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || length > len(source.page) {
		return nil, ErrIntegrity
	}
	if offset > source.bytes {
		return nil, io.EOF
	}
	available := min(length, source.bytes-offset)
	body := source.page[:available:available]
	for index := range body {
		if index%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		position := offset + index
		switch {
		case position < len(source.prefix):
			body[index] = source.prefix[position]
		case position < len(source.prefix)+source.numberBytes:
			body[index] = legacyTypedNumberCostNumberByte(position-len(source.prefix), source.numberBytes)
		default:
			body[index] = source.suffix[position-len(source.prefix)-source.numberBytes]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if available < length {
		return body, io.EOF
	}
	return body, nil
}

func (source *legacyTypedNumberCostSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	body, err := source.generate(ctx, offset, length)
	source.readBytes += int64(len(body))
	switch {
	case offset == source.bytes && length == 0 && err == nil && len(body) == 0:
		source.zeroEOF++
	case offset == source.bytes && length == 1 && errors.Is(err, io.EOF) && len(body) == 0:
		source.oneEOF++
	case offset >= 0 && offset < source.bytes && offset%ConversationManifestChunkBytes == 0 &&
		length == min(ConversationManifestChunkBytes, source.bytes-offset) && len(body) == length && err == nil:
		ordinal := offset / ConversationManifestChunkBytes
		source.loads++
		if !source.visited[ordinal] {
			source.visited[ordinal] = true
			source.coveredBytes += int64(length)
		}
	default:
		source.unexpectedReads++
	}
	return body, err
}

func (source *legacyTypedNumberCostSource) reset() {
	clear(source.visited)
	source.readBytes, source.coveredBytes = 0, 0
	source.loads, source.zeroEOF, source.oneEOF, source.unexpectedReads = 0, 0, 0, 0
}
