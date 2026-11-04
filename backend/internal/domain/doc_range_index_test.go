package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

type docRangeMemoryBuffer struct {
	pages [][]byte
}

func (buffer *docRangeMemoryBuffer) Append(ctx context.Context, raw []byte) error {
	buffer.pages = append(buffer.pages, bytes.Clone(raw))
	return ctx.Err()
}

func (buffer *docRangeMemoryBuffer) Read(ctx context.Context, position int) ([]byte, error) {
	return bytes.Clone(buffer.pages[position]), ctx.Err()
}

func (buffer *docRangeMemoryBuffer) Flush(ctx context.Context) error {
	return ctx.Err()
}

type docRangeFlushFailingBuffer struct {
	docRangeMemoryBuffer
	err    error
	cancel context.CancelFunc
}

func (buffer *docRangeFlushFailingBuffer) Flush(ctx context.Context) error {
	if buffer.cancel != nil {
		buffer.cancel()
		return nil
	}
	return buffer.err
}

func TestDocRangeIndexFlushFailureNeverSealsOrAdmits(t *testing.T) {
	manifest, bodies, _ := rootEventsFixture(t)
	hash := manifestProofHash(t, manifest)
	for _, mode := range []string{"flush", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("spool flush failed")
			buffer := &docRangeFlushFailingBuffer{err: want}
			if mode == "cancel" {
				want = context.Canceled
				buffer.cancel = cancel
			}
			var verifier CanonicalDocVerifier
			index, err := verifier.VerifyConversationRangeIndex(ctx, hash, manifest, manifestProofLoad(bodies), buffer)
			if !errors.Is(err, want) || index.Header().Hash != "" || index.read != nil || len(verifier.events) != 0 || len(buffer.pages) == 0 {
				t.Fatal("failed flush became verified", err)
			}
		})
	}
}

func docRangeTestPage(t *testing.T, index VerifiedDocRangeIndex, position int) []byte {
	t.Helper()
	raw, err := index.Page(context.Background(), position)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerifiedDocRangePageEntriesOwnAndRevalidateBytes(t *testing.T) {
	manifest, bodies, _ := rootEventsFixture(t)
	buffer := &docRangeReusingBuffer{}
	var verifier CanonicalDocVerifier
	index, err := verifier.VerifyConversationRangeIndex(context.Background(), manifestProofHash(t, manifest), manifest, manifestProofLoad(bodies), buffer)
	if err != nil {
		t.Fatal(err)
	}
	raw, entries, err := index.pageEntries(context.Background(), 0)
	if err != nil || len(entries) == 0 {
		t.Fatal(err)
	}
	original := bytes.Clone(raw)
	originalHash := entries[0].Hash
	buffer.raw[0] = 'x'
	if !bytes.Equal(raw, original) {
		t.Fatal("decoded page retained the reader buffer")
	}
	raw[0], entries[0].Hash = 'x', "invalid"
	raw, entries, err = index.pageEntries(context.Background(), 0)
	if err != nil || !bytes.Equal(raw, original) || entries[0].Hash != originalHash {
		t.Fatal("caller mutation changed the next decoded page", err)
	}
	buffer.pages[0][0] = 'x'
	if _, _, err := index.pageEntries(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
		t.Fatal("repeat read did not authenticate current bytes", err)
	}
	entries[0].Index++
	malformed, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	buffer.pages[0] = malformed
	index.index.Pages[0] = HashContent(malformed)
	if _, _, err := index.pageEntries(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
		t.Fatal("matching hash hid invalid entry metadata", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	index.index.Pages[0] = HashContent(original)
	index.read = func(context.Context, int) ([]byte, error) {
		cancel()
		return original, nil
	}
	raw, entries, err = index.pageEntries(ctx, 0)
	if !errors.Is(err, context.Canceled) || raw != nil || entries != nil {
		t.Fatal("canceled read exposed decoded results", err)
	}
}

func TestDocRangeIndexRequiresCompleteVerificationAndAuthenticatesPages(t *testing.T) {
	manifest, bodies, _ := rootEventsFixture(t)
	hash := manifestProofHash(t, manifest)
	var verifier CanonicalDocVerifier
	index, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	header := index.Header()
	if err := header.Validate(hash); err != nil {
		t.Fatal(err)
	}
	entries, err := header.DecodePage(0, docRangeTestPage(t, index, 0))
	if err != nil || len(entries) != int(manifest.EventCount) {
		t.Fatal(entries, err)
	}
	for _, entry := range entries {
		var stream []byte
		for _, chunk := range manifest.Chunks {
			stream = append(stream, bodies[chunk.Hash]...)
		}
		if HashContent(stream[entry.Offset:entry.Offset+entry.Length]) != entry.Hash {
			t.Fatal("wrong event location")
		}
	}
	raw := docRangeTestPage(t, index, 0)
	raw[0] = 'x'
	if _, err := header.DecodePage(0, raw); !errors.Is(err, ErrIntegrity) {
		t.Fatal("corrupt page accepted", err)
	}
	if _, err := header.DecodePage(0, docRangeTestPage(t, index, 0)); err != nil {
		t.Fatal("caller mutated owned index", err)
	}
	originalEnvelope := append([]byte(nil), manifest.Envelope...)
	originalChunk := manifest.Chunks[0]
	mutatingLoad := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		manifest.Envelope[0] = 'x'
		manifest.Chunks[0].Bytes = 0
		return manifestProofLoad(bodies)(ctx, hash)
	}
	owned, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, mutatingLoad, &docRangeMemoryBuffer{})
	if err != nil || owned.Header().Validate(hash) != nil {
		t.Fatal("caller mutated verified descriptor", err)
	}
	manifest.Envelope = originalEnvelope
	manifest.Chunks[0] = originalChunk
	failed := func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound }
	if _, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, failed, &docRangeMemoryBuffer{}); !errors.Is(err, ErrNotFound) {
		t.Fatal("warm proof hid missing current source", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verifier.VerifyConversationRangeIndex(ctx, hash, manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestDocViewEventMetadataDoesNotAlterArchive(t *testing.T) {
	event := CIREvent{Kind: EventToolResult, Seq: 3, Output: "large output"}
	before, _ := json.Marshal(event)
	raw, err := json.Marshal(DocViewEvent{Event: event, Index: 7, Bytes: 40, Deferred: true})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if json.Unmarshal(raw, &wire) != nil || wire["event_index"] != float64(7) || wire["deferred"] != true || wire["output"] != nil {
		t.Fatal(string(raw))
	}
	after, _ := json.Marshal(event)
	if !bytes.Equal(before, after) || bytes.Contains(after, []byte("deferred")) {
		t.Fatal("view fields leaked into archive")
	}
}

type docRangeReusingBuffer struct {
	docRangeMemoryBuffer
	raw []byte
}

func (buffer *docRangeReusingBuffer) Read(ctx context.Context, position int) ([]byte, error) {
	buffer.raw = append(buffer.raw[:0], buffer.pages[position]...)
	return buffer.raw, ctx.Err()
}

func TestDocViewDeferredCompactionDoesNotRequireArchivePayload(t *testing.T) {
	for _, event := range []CIREvent{
		{Kind: EventCompaction, Seq: 42},
		{Kind: EventCompaction, Seq: 42, Replacement: []CIREvent{{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "private replacement"}}}}},
	} {
		raw, err := json.Marshal(DocViewEvent{Event: event, Index: 7, Deferred: true, Bytes: 900000})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["kind"]) != `"compaction"` || string(fields["deferred"]) != "true" || string(fields["event_index"]) != "7" || len(fields) != 5 || bytes.Contains(raw, []byte("private replacement")) {
			t.Fatalf("invalid compaction projection: %s", raw)
		}
	}
}

func TestDocRangePageOwnsReusedBufferBytes(t *testing.T) {
	manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: canonicalAdmissionEvents(0, DocRangePageEvents+1)})
	if err != nil {
		t.Fatal(err)
	}
	hash := manifestProofHash(t, manifest)
	buffer := &docRangeReusingBuffer{raw: make([]byte, 0, MaxDocRangePageBytes)}
	var verifier CanonicalDocVerifier
	index, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, manifestProofLoad(bodies), buffer)
	if err != nil {
		t.Fatal(err)
	}
	first := docRangeTestPage(t, index, 0)
	want := bytes.Clone(first)
	_ = docRangeTestPage(t, index, 1)
	if !bytes.Equal(first, want) {
		t.Fatal("later buffer read changed previously returned page")
	}
	buffer.pages[0][0] = 'x'
	if _, err := index.Page(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
		t.Fatal("changed buffer contents accepted", err)
	}
}

type docRangeFailingBuffer struct {
	docRangeMemoryBuffer
	err    error
	cancel context.CancelFunc
}

func (buffer *docRangeFailingBuffer) Append(ctx context.Context, raw []byte) error {
	if err := buffer.docRangeMemoryBuffer.Append(ctx, raw); err != nil {
		return err
	}
	if buffer.cancel != nil {
		buffer.cancel()
		return ctx.Err()
	}
	return buffer.err
}

func TestDocRangeIndexBufferFailureNeverSealsOrAdmits(t *testing.T) {
	manifest, bodies, _ := rootEventsFixture(t)
	hash := manifestProofHash(t, manifest)
	for _, mode := range []string{"write", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("spool write failed")
			buffer := &docRangeFailingBuffer{err: want}
			if mode == "cancel" {
				want = context.Canceled
				buffer.cancel = cancel
			}
			var verifier CanonicalDocVerifier
			index, err := verifier.VerifyConversationRangeIndex(ctx, hash, manifest, manifestProofLoad(bodies), buffer)
			if !errors.Is(err, want) || index.Header().Hash != "" || index.read != nil || len(verifier.events) != 0 || len(buffer.pages) == 0 {
				t.Fatal("failed staged page became verified", err)
			}
		})
	}
}

func TestDocRangeIndexFinalCancellationDoesNotSealOrAdmit(t *testing.T) {
	manifest, bodies, _ := rootEventsFixture(t)
	hash := manifestProofHash(t, manifest)
	checks := 0
	var baseline CanonicalDocVerifier
	if _, err := baseline.VerifyConversationRangeIndex(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, hash, manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	var verifier CanonicalDocVerifier
	index, err := verifier.VerifyConversationRangeIndex(canonicalAdmissionContext{Context: ctx, check: func() {
		seen++
		if seen == checks {
			cancel()
		}
	}}, hash, manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
	if !errors.Is(err, context.Canceled) || index.Header().Hash != "" || index.read != nil || len(verifier.events) != 0 {
		t.Fatal("final cancellation returned proof or admitted cache entries", err)
	}
}

func TestDocRangeIndexBoundsRoleLabelsWithoutChangingArchive(t *testing.T) {
	for _, role := range []string{strings.Repeat("\x00", 128<<10), strings.Repeat("\x00", 128), strings.Repeat("\U0001f642", 129), "user", "assistant"} {
		t.Run(string(HashContent([]byte(role))), func(t *testing.T) {
			count := 1
			if len(role) < 1024 {
				count = DocRangePageEvents + 1
			}
			events := make([]CIREvent, count)
			for position := range events {
				events[position] = CIREvent{Kind: EventTurn, Role: Role(role), Seq: position}
			}
			manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: events})
			if err != nil {
				t.Fatal(err)
			}
			hash := manifestProofHash(t, manifest)
			var verifier CanonicalDocVerifier
			proof, err := verifier.VerifyConversationManifestDoc(context.Background(), hash, manifest, manifestProofLoad(bodies))
			if err != nil {
				t.Fatal(err)
			}
			before := proof.Bytes()
			lazy, err := BuildDocRangeIndex(context.Background(), proof)
			if err != nil {
				t.Fatal(err)
			}
			streamed, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
			if err != nil {
				t.Fatal(err)
			}
			header := streamed.Header()
			for position := range header.Pages {
				raw := docRangeTestPage(t, streamed, position)
				if len(raw) > MaxDocRangePageBytes || !bytes.Equal(raw, docRangeTestPage(t, lazy, position)) {
					t.Fatal("bounded label encoding differs")
				}
				entries, err := header.DecodePage(position, raw)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Role != docRangeLabel(role) || utf8.RuneCountInString(entry.Role) > 128 || !utf8.ValidString(entry.Role) {
						t.Fatal("role label was not bounded")
					}
					body, err := proof.EventStreamRange(context.Background(), entry.Offset, entry.Length)
					if err != nil || HashContent(body) != entry.Hash {
						t.Fatal("original event bytes changed", err)
					}
					var event CIREvent
					if err := json.Unmarshal(body, &event); err != nil || string(event.Role) != role {
						t.Fatal("original role was truncated", err)
					}
				}
			}
			if !bytes.Equal(before, proof.Bytes()) {
				t.Fatal("indexing changed archive")
			}
		})
	}
}

func TestDocRangeHeaderAllowsMoreThan32768Pages(t *testing.T) {
	const count = 32768*DocRangePageEvents + 1
	remaining := int64(count * 40)
	chunkHash := HashContent([]byte("descriptor-only fixture"))
	chunks := []ConversationManifestChunk{}
	for remaining > 0 {
		length := min(remaining, int64(ConversationManifestChunkBytes))
		chunks = append(chunks, ConversationManifestChunk{Hash: chunkHash, Bytes: length})
		remaining -= length
	}
	envelope, err := canonicalJSON(CIREnvelope{CIRVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewConversationManifest(envelope, chunks, count)
	if err != nil {
		t.Fatal(err)
	}
	hash := manifestProofHash(t, manifest)
	header := DocRangeIndex{Version: 1, Hash: hash, Manifest: manifest, Pages: make([]ContentHash, (count+DocRangePageEvents-1)/DocRangePageEvents)}
	for position := range header.Pages {
		header.Pages[position] = chunkHash
	}
	if err := header.Validate(hash); err != nil {
		t.Fatal("header imposed the obsolete aggregate-derived page count", err)
	}
	raw, err := json.Marshal(header)
	if err != nil || len(raw) > MaxDocRangeIndexBytes {
		t.Fatal("fixture exceeded actual header byte limit", err)
	}
}
