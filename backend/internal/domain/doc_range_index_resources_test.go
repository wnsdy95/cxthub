package domain

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"unsafe"
)

func rangeResourceIndex(t *testing.T) VerifiedDocRangeIndex {
	t.Helper()
	manifest, bodies, _ := rootEventsFixture(t)
	var verifier CanonicalDocVerifier
	index, err := verifier.VerifyConversationRangeIndex(context.Background(), manifestProofHash(t, manifest), manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func TestDocRangeRetainedHeaderBytesCoverMetadata(t *testing.T) {
	index := rangeResourceIndex(t)
	estimate, err := index.RetainedHeaderBytes()
	if err != nil {
		t.Fatal(err)
	}
	header := index.index
	minimum := int64(unsafe.Sizeof(header)) + int64(cap(header.Manifest.Envelope)) +
		int64(cap(header.Manifest.Chunks))*int64(unsafe.Sizeof(ConversationManifestChunk{})) +
		int64(cap(header.Pages))*int64(unsafe.Sizeof(ContentHash("")))
	for _, value := range []string{string(header.Hash), header.Manifest.Identity, header.Manifest.ChunkFormat} {
		minimum += int64(len(value))
	}
	for _, chunk := range header.Manifest.Chunks {
		minimum += int64(len(chunk.Hash))
	}
	for _, hash := range header.Pages {
		minimum += int64(len(hash))
	}
	if estimate < minimum || estimate >= 16<<20 {
		t.Fatalf("header estimate=%d minimum=%d", estimate, minimum)
	}
	index.read = func(context.Context, int) ([]byte, error) {
		t.Fatal("metadata accounting read a page")
		return nil, nil
	}
	if next, err := index.RetainedHeaderBytes(); err != nil || next != estimate {
		t.Fatal("reader changed header-only accounting", next, err)
	}
	index.read = nil
	if next, err := index.RetainedHeaderBytes(); err != nil || next != estimate {
		t.Fatal("header-only accounting required a reader", next, err)
	}
}

func TestDocRangeRetainedHeaderBytesChargeCapacityAndTailStrings(t *testing.T) {
	for _, part := range []string{"envelope", "chunks", "pages", "hash", "identity", "format"} {
		t.Run(part, func(t *testing.T) {
			index := rangeResourceIndex(t)
			before, err := index.RetainedHeaderBytes()
			if err != nil {
				t.Fatal(err)
			}
			const extra = 4096
			var minimumIncrease int64
			switch part {
			case "envelope":
				prior := index.index.Manifest.Envelope
				grown := make([]byte, len(prior), cap(prior)+extra)
				copy(grown, prior)
				index.index.Manifest.Envelope = grown
				minimumIncrease = extra
			case "chunks":
				prior := index.index.Manifest.Chunks
				grown := make([]ConversationManifestChunk, cap(prior)+1)
				copy(grown, prior)
				grown[len(grown)-1].Hash = ContentHash(strings.Repeat("c", extra))
				index.index.Manifest.Chunks = grown[:len(prior)]
				minimumIncrease = extra + int64(unsafe.Sizeof(ConversationManifestChunk{}))
			case "pages":
				prior := index.index.Pages
				grown := make([]ContentHash, cap(prior)+1)
				copy(grown, prior)
				grown[len(grown)-1] = ContentHash(strings.Repeat("p", extra))
				index.index.Pages = grown[:len(prior)]
				minimumIncrease = extra + int64(unsafe.Sizeof(ContentHash("")))
			case "hash":
				index.index.Hash += ContentHash(strings.Repeat("h", extra))
				minimumIncrease = extra
			case "identity":
				index.index.Manifest.Identity += strings.Repeat("i", extra)
				minimumIncrease = extra
			case "format":
				index.index.Manifest.ChunkFormat += strings.Repeat("f", extra)
				minimumIncrease = extra
			}
			after, err := index.RetainedHeaderBytes()
			if err != nil || after-before < minimumIncrease {
				t.Fatalf("capacity/string retention uncharged: before=%d after=%d minimum increase=%d error=%v", before, after, minimumIncrease, err)
			}
		})
	}
}

func TestDocRangeRetainedHeaderBytesRejectInvalidHeader(t *testing.T) {
	for _, index := range []VerifiedDocRangeIndex{{}, {index: DocRangeIndex{Version: 1, Hash: "hash"}}, {index: DocRangeIndex{Version: 2, Hash: "hash", Pages: []ContentHash{}}}} {
		if estimate, err := index.RetainedHeaderBytes(); estimate != 0 || !errors.Is(err, ErrIntegrity) {
			t.Fatal("invalid header yielded a resource estimate", estimate, err)
		}
	}
}

func TestDocRangeHeaderHeapEstimateOverflow(t *testing.T) {
	for _, input := range []struct{ initial, count, width int64 }{
		{0, -1, 1}, {0, 1, 0}, {0, 1, -1}, {-1, 1, 1},
		{0, math.MaxInt64, 1}, {0, 2, math.MaxInt64}, {math.MaxInt64 - 64, 1, 1},
	} {
		estimate := docRangeHeaderHeapEstimate{bytes: input.initial}
		estimate.allocation(input.count, input.width)
		if !errors.Is(estimate.err, ErrIntegrity) || estimate.bytes != input.initial {
			t.Fatal("overflow mutated or accepted estimate", input, estimate)
		}
		estimate.allocation(1, 1)
		if !errors.Is(estimate.err, ErrIntegrity) || estimate.bytes != input.initial {
			t.Fatal("invalid estimate recovered", estimate)
		}
	}
	estimate := docRangeHeaderHeapEstimate{}
	estimate.allocation((math.MaxInt64-64)/2, 1)
	if estimate.err != nil || estimate.bytes != math.MaxInt64-1 {
		t.Fatal("largest supported allocation", estimate)
	}
	estimate.allocation(1, 1)
	if !errors.Is(estimate.err, ErrIntegrity) {
		t.Fatal("sum overflow accepted", estimate)
	}
}
