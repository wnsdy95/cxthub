package domain

import (
	"context"
	"errors"
	"io"
	"testing"
)

const legacyNativePublicRejectCostPrefix = `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","seq":1,"call_id":"huge-number","output":`
const legacyNativePublicRejectCostSuffix = `}]}`

type legacyNativePublicRejectCostSource struct {
	bytes           int
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

func newLegacyNativePublicRejectCostSource(test testing.TB, extent int) *legacyNativePublicRejectCostSource {
	test.Helper()
	if extent < len(legacyNativePublicRejectCostPrefix)+310+len(legacyNativePublicRejectCostSuffix) || extent > MaxFinalizedDocBytes {
		test.Fatal("generated overflow document has an invalid extent", extent)
	}
	count := (extent + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	source := &legacyNativePublicRejectCostSource{
		bytes: extent, page: make([]byte, ConversationManifestChunkBytes),
		parts: make([]ConversationManifestChunk, count), visited: make([]bool, count),
	}
	var total int64
	for ordinal := range source.parts {
		offset := ordinal * ConversationManifestChunkBytes
		length := min(ConversationManifestChunkBytes, extent-offset)
		body, err := source.generate(context.Background(), offset, length)
		if err != nil || len(body) != length {
			test.Fatal("could not generate a complete source part", ordinal, len(body), length, err)
		}
		source.parts[ordinal] = ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(length)}
		total += int64(length)
	}
	if total != int64(extent) || cap(source.page) > 512<<10 ||
		len(legacyNativePublicRejectCostPrefix)+len(legacyNativePublicRejectCostSuffix) > 64<<10 {
		test.Fatal("generated source changed its exact extent or bounded backing", total, extent, cap(source.page))
	}
	return source
}

func (source *legacyNativePublicRejectCostSource) generate(ctx context.Context, offset, length int) ([]byte, error) {
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
		body[index] = '9'
	}
	if offset < len(legacyNativePublicRejectCostPrefix) {
		copy(body, legacyNativePublicRejectCostPrefix[offset:])
	}
	suffixStart := source.bytes - len(legacyNativePublicRejectCostSuffix)
	if from := max(offset, suffixStart); from < offset+available {
		copy(body[from-offset:], legacyNativePublicRejectCostSuffix[from-suffixStart:])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if available < length {
		return body, io.EOF
	}
	return body, nil
}

func (source *legacyNativePublicRejectCostSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
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

func (source *legacyNativePublicRejectCostSource) reset() {
	clear(source.visited)
	source.readBytes, source.coveredBytes = 0, 0
	source.loads, source.zeroEOF, source.oneEOF, source.unexpectedReads = 0, 0, 0, 0
}

func TestLegacyNativePublicRejectCostGeneratedSourceOffsets(test *testing.T) {
	const extent = 2*ConversationManifestChunkBytes + 37
	source := newLegacyNativePublicRejectCostSource(test, extent)
	check := func(offset, length, expectedLength int, expectedError error) {
		test.Helper()
		body, err := source.Read(context.Background(), offset, length)
		if len(body) != expectedLength || !errors.Is(err, expectedError) {
			test.Fatal("generated source changed requested span or EOF", offset, length, len(body), expectedLength, err, expectedError)
		}
		for index, current := range body {
			position, expected := offset+index, byte('9')
			switch {
			case position < len(legacyNativePublicRejectCostPrefix):
				expected = legacyNativePublicRejectCostPrefix[position]
			case position >= extent-len(legacyNativePublicRejectCostSuffix):
				expected = legacyNativePublicRejectCostSuffix[position-(extent-len(legacyNativePublicRejectCostSuffix))]
			}
			if current != expected {
				test.Fatal("generated source changed an absolute byte", offset, length, position, current, expected)
			}
		}
	}
	for ordinal, part := range source.parts {
		offset := ordinal * ConversationManifestChunkBytes
		check(offset, int(part.Bytes), int(part.Bytes), nil)
		body, err := source.Read(context.Background(), offset, int(part.Bytes))
		if err != nil || HashContent(body) != part.Hash {
			test.Fatal("generated source metadata did not authenticate its real bytes", ordinal, err)
		}
	}
	for _, offset := range []int{0, 1, len(legacyNativePublicRejectCostPrefix) - 1, len(legacyNativePublicRejectCostPrefix),
		ConversationManifestChunkBytes - 17, ConversationManifestChunkBytes + 19,
		extent - len(legacyNativePublicRejectCostSuffix) - 13, extent - len(legacyNativePublicRejectCostSuffix), extent - 1, extent} {
		check(offset, 0, 0, nil)
		for _, length := range []int{1, 7, 41, canonicalCheckBytes, ConversationManifestChunkBytes} {
			expectedLength := min(length, extent-offset)
			var expectedError error
			if expectedLength < length {
				expectedError = io.EOF
			}
			check(offset, length, expectedLength, expectedError)
		}
	}
	check(-1, 1, 0, ErrIntegrity)
	check(0, -1, 0, ErrIntegrity)
	check(0, ConversationManifestChunkBytes+1, 0, ErrIntegrity)
	check(extent+1, 0, 0, io.EOF)
	check(extent+1, 1, 0, io.EOF)
	check(int(^uint(0)>>1), 1, 0, io.EOF)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if body, err := source.Read(ctx, 0, ConversationManifestChunkBytes); len(body) != 0 || !errors.Is(err, context.Canceled) {
		test.Fatal("generated source ignored the current canceled context", len(body), err)
	}
}
