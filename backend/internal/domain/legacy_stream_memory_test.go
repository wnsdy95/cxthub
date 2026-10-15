package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

type legacyFileStream struct {
	file      *os.File
	size      int
	maxAppend int
}

func (stream *legacyFileStream) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	written, err := stream.file.Write(body)
	stream.size += written
	stream.maxAppend = max(stream.maxAppend, len(body))
	if err != nil {
		return err
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	return ctx.Err()
}

func (stream *legacyFileStream) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := make([]byte, length)
	if _, err := stream.file.ReadAt(body, int64(offset)); err != nil {
		return nil, err
	}
	return body, ctx.Err()
}

func newLegacyFileStream(t testing.TB) *legacyFileStream {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "verified-stream-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return &legacyFileStream{file: file}
}

func TestLegacyStreamRetainsMetadataNotTranscript(t *testing.T) {
	if testing.Short() {
		t.Skip("disk-backed 96 MiB retained-heap regression")
	}
	const eventCount = 1536
	ctx := context.Background()
	input := newLegacyFileStream(t)
	for ordinal := 0; ordinal < eventCount; ordinal++ {
		if ordinal > 0 {
			if err := input.Append(ctx, []byte(",")); err != nil {
				t.Fatal(err)
			}
		}
		event := CIREvent{Seq: ordinal, Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: fmt.Sprintf("event %d ", ordinal) + strings.Repeat("aBcD", 16384)}}}
		body, err := canonicalDecodedEventJSON(event)
		if err != nil {
			t.Fatal(err)
		}
		if err := input.Append(ctx, body); err != nil {
			t.Fatal(err)
		}
	}
	envelope, err := canonicalJSON(CIREnvelope{CIRVersion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	manifest := DocChunkManifest{Format: ChunkFormatV2, Envelope: envelope}
	locations := map[ContentHash]canonicalSpan{}
	hasher := sha256.New()
	hasher.Write([]byte(canonicalDocPrefix))
	hasher.Write(envelope)
	hasher.Write([]byte(canonicalDocMiddle))
	for offset := 0; offset < input.size; offset += ChunkTarget {
		length := min(ChunkTarget, input.size-offset)
		body, err := input.Read(ctx, offset, length)
		if err != nil {
			t.Fatal(err)
		}
		hasher.Write(body)
		hash := HashContent(body)
		manifest.Chunks = append(manifest.Chunks, hash)
		locations[hash] = canonicalSpan{offset: offset, length: length}
	}
	hasher.Write([]byte(canonicalDocSuffix))
	want := ContentHash("sha256:" + hex.EncodeToString(hasher.Sum(nil)))
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		span := locations[hash]
		return input.Read(ctx, span.offset, span.length)
	}
	var verifier CanonicalDocVerifier
	for scan := 0; scan < 2; scan++ {
		spool := newLegacyFileStream(t)
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		proof, err := verifier.VerifyLegacyStream(ctx, want, manifest, load, spool)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := proof.PlanReadIndexContext(ctx)
		if err != nil || len(proof.events) != eventCount || len(plan.stream.parts) != 0 || plan.source != spool {
			t.Fatal("stream plan retained transcript or lost metadata", err)
		}
		if proof.size != input.size || spool.size != input.size || spool.maxAppend > canonicalCheckBytes {
			t.Fatal("streaming size/append contract violated")
		}
		index, err := plan.BuildMetadataContext(ctx)
		if err != nil || len(index.Events) != eventCount || index.Events[eventCount-1].Seq != eventCount-1 {
			t.Fatal("large spooled read plan failed", err)
		}
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		if retained > 8<<20 {
			t.Fatalf("scan %d retained %d bytes for %d-byte stream; metadata budget is 8 MiB", scan, retained, input.size)
		}
		t.Logf("scan=%d transcript=%d retained_heap_delta=%d append_max=%d events=%d", scan, input.size, retained, spool.maxAppend, plan.EventCount())
		runtime.KeepAlive(proof)
		runtime.KeepAlive(plan)
		runtime.KeepAlive(index)
		runtime.KeepAlive(&verifier)
	}
}
