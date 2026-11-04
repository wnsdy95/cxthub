package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyByteSpoolTestSource struct {
	body           []byte
	appends        []int
	reads          []canonicalSpan
	appendMode     string
	readMode       string
	appendError    error
	readError      error
	failReadAt     int
	extentError    error
	endError       error
	cancel         context.CancelFunc
	cancelAppendAt int
	cancelReadAt   int
}

func (source *legacyByteSpoolTestSource) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source.appends = append(source.appends, len(body))
	if source.appendError != nil {
		return source.appendError
	}
	start := len(source.body)
	switch source.appendMode {
	case "drop":
	case "truncate":
		source.body = append(source.body, body[:len(body)-1]...)
	default:
		source.body = append(source.body, body...)
	}
	switch source.appendMode {
	case "corrupt":
		source.body[start] ^= 1
	case "extra":
		source.body = append(source.body, 0)
	case "rewrite-first":
		if len(source.appends) == 2 {
			source.body[0] ^= 1
		}
	case "mutate-input":
		clear(body)
	}
	if source.cancel != nil && len(source.appends) == source.cancelAppendAt {
		source.cancel()
	}
	return nil
}

func (source *legacyByteSpoolTestSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source.reads = append(source.reads, canonicalSpan{offset: offset, length: length})
	if source.cancel != nil && len(source.reads) == source.cancelReadAt {
		source.cancel()
	}
	if source.readError != nil && (source.failReadAt == 0 || source.failReadAt == len(source.reads)) {
		return nil, source.readError
	}
	if length == 0 && source.endError != nil {
		return nil, source.endError
	}
	if offset < 0 || length < 0 || offset > len(source.body) || length > len(source.body)-offset {
		if source.extentError != nil {
			return nil, source.extentError
		}
		return nil, ErrIntegrity
	}
	body := source.body[offset : offset+length]
	if length == 0 {
		return body, nil
	}
	switch source.readMode {
	case "corrupt":
		body = bytes.Clone(body)
		body[0] ^= 1
	case "short":
		body = body[:len(body)-1]
	case "long":
		body = append(bytes.Clone(body), 0)
	case "empty":
		body = nil
	}
	return body, nil
}

func legacyByteSpoolRequireZero(test testing.TB, plan LegacyByteBackfill, err, expected error) {
	test.Helper()
	if !errors.Is(err, expected) || !reflect.DeepEqual(plan, LegacyByteBackfill{}) {
		test.Fatalf("failed spool retained a plan: valid=%t, error=%v; want %v", plan.Valid(), err, expected)
	}
}

func legacyByteSpoolRequireZeroBlock(test testing.TB, block VerifiedLegacyByteBlock, err, expected error) {
	test.Helper()
	if !errors.Is(err, expected) || block != (VerifiedLegacyByteBlock{}) {
		test.Fatalf("failed read returned a block: valid=%t, error=%v; want %v", block.Valid(), err, expected)
	}
}

func TestLegacyByteBackfillSpoolParitySeekingAndCopyBatch(test *testing.T) {
	proof, raw := legacyDocByteBackfillSizedFixture(test, 10*ChunkTarget+17)
	_, stream := chunkVerifierParts(test, raw)
	plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	source := &legacyByteSpoolTestSource{appendMode: "mutate-input"}
	frozen, err := plan.Spool(context.Background(), source)
	if err != nil || !frozen.Valid() || frozen.source != source || !bytes.Equal(source.body, stream) {
		test.Fatalf("spool changed canonical bytes or did not retain the supplied source: %v", err)
	}
	if frozen.stream.parts != nil || frozen.stream.offsets != nil || frozen.stream.size != 0 || frozen.canonicalRetainedBytes != 0 ||
		len(frozen.descriptors) != spec.Blocks || cap(frozen.descriptors) != spec.Blocks {
		test.Fatal("spooled plan retained raw canonical references or incomplete descriptors")
	}
	gotSpec, specError := frozen.Spec()
	gotKey, keyError := frozen.Key()
	if specError != nil || keyError != nil || gotSpec != spec || gotKey != key {
		test.Fatalf("spooling changed spec or key: %+v, %s, %v, %v", gotSpec, gotKey, specError, keyError)
	}
	if len(source.appends) != spec.Blocks || len(source.reads) != spec.Blocks+2 {
		test.Fatalf("spool skipped an occurrence or its extent check: appends=%d, reads=%d", len(source.appends), len(source.reads))
	}
	for ordinal, length := range source.appends {
		wantLength := min(ChunkTarget, len(stream)-ordinal*ChunkTarget)
		if length != wantLength || source.reads[ordinal] != (canonicalSpan{offset: ordinal * ChunkTarget, length: wantLength}) {
			test.Fatalf("spool used a noncanonical or unbounded block at ordinal %d", ordinal)
		}
	}
	if !reflect.DeepEqual(source.reads[spec.Blocks:], []canonicalSpan{{offset: len(stream)}, {offset: len(stream), length: 1}}) {
		test.Fatalf("spool did not verify the exact stream end: %v", source.reads[spec.Blocks:])
	}
	casBodies := make(map[ContentHash][]byte)
	for ordinal := spec.Blocks - 1; ordinal >= 0; ordinal-- {
		source.reads = nil
		block, err := frozen.Block(context.Background(), ordinal)
		if err != nil {
			test.Fatal(err)
		}
		legacyDocByteBackfillRequireBlock(test, block, spec, key, ordinal, stream)
		original, err := plan.Block(context.Background(), ordinal)
		if err != nil || block != original {
			test.Fatalf("raw and spooled block %d differ: %v", ordinal, err)
		}
		if !reflect.DeepEqual(source.reads, []canonicalSpan{{offset: ordinal * ChunkTarget, length: len(block.body)}}) {
			test.Fatalf("ordinal read loaded more than its bounded block: %v", source.reads)
		}
		if previous, exists := casBodies[block.descriptor.Hash]; exists && !bytes.Equal(previous, block.Bytes()) {
			test.Fatal("repeated CAS hash changed its bytes")
		}
		casBodies[block.descriptor.Hash] = block.Bytes()
		mutable := block.Bytes()
		clear(mutable)
		legacyDocByteBackfillRequireBlock(test, block, spec, key, ordinal, stream)
	}
	if len(casBodies) >= spec.Blocks || frozen.descriptors[1] != frozen.descriptors[2] {
		test.Fatal("fixture did not retain repeated CAS hash occurrences")
	}
	for name, candidate := range map[string]LegacyByteBackfill{"raw": plan, "spooled": frozen} {
		test.Run(name, func(test *testing.T) {
			for _, start := range []int{0, 0, 8, 3, spec.Blocks} {
				next := start
				progress, err := candidate.CopyBatch(context.Background(), start, DocLegacyByteBackfillBatchBlocks, func(ctx context.Context, block VerifiedLegacyByteBlock) error {
					legacyDocByteBackfillRequireBlock(test, block, spec, key, next, stream)
					next++
					return ctx.Err()
				})
				wantNext := min(start+DocLegacyByteBackfillBatchBlocks, spec.Blocks)
				copied := min(int64(wantNext)*ChunkTarget, spec.StreamBytes) - min(int64(start)*ChunkTarget, spec.StreamBytes)
				want := DocLegacyByteBackfillProgress{Plan: key, Next: wantNext, CopiedBytes: copied, Complete: wantNext == spec.Blocks}
				if err != nil || progress != want || next != wantNext {
					test.Fatalf("batch start %d changed acknowledgment boundaries: %+v, %v; want %+v", start, progress, err, want)
				}
			}
		})
	}
	respooled, err := frozen.Spool(context.Background(), &legacyByteSpoolTestSource{})
	if err != nil || !respooled.Valid() || !reflect.DeepEqual(respooled.descriptors, frozen.descriptors) {
		test.Fatalf("spooled plan could not spool its existing proofs again: %v", err)
	}
	if unsafe.SliceData(respooled.descriptors) == unsafe.SliceData(frozen.descriptors) {
		test.Fatal("respool reused the previous descriptor backing")
	}
	if !bytes.Equal(proof.Bytes(), raw) {
		test.Fatal("spooling changed the original immutable proof")
	}
}

func TestLegacyByteBackfillSpoolRejectsWriteReadbackAndExtentFailures(test *testing.T) {
	proof, _ := legacyDocByteBackfillSizedFixture(test, ChunkTarget+17)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	failure := errors.New("spool source failed")
	for name, source := range map[string]*legacyByteSpoolTestSource{
		"write-error":       {appendError: failure},
		"dropped-write":     {appendMode: "drop"},
		"short-write":       {appendMode: "truncate"},
		"corrupt-write":     {appendMode: "corrupt"},
		"rewrite-earlier":   {appendMode: "rewrite-first"},
		"read-error":        {readError: failure},
		"read-hash":         {readMode: "corrupt"},
		"read-short":        {readMode: "short"},
		"read-long":         {readMode: "long"},
		"read-missing":      {readMode: "empty"},
		"end-read-error":    {endError: failure},
		"extent-read-error": {extentError: failure},
	} {
		test.Run(name, func(test *testing.T) {
			got, err := plan.Spool(context.Background(), source)
			expected := ErrIntegrity
			if source.appendError != nil || source.readError != nil || source.endError != nil || source.extentError != nil {
				expected = failure
			}
			legacyByteSpoolRequireZero(test, got, err, expected)
		})
	}
	for _, extentError := range []error{io.EOF, io.ErrUnexpectedEOF, ErrIntegrity} {
		source := &legacyByteSpoolTestSource{extentError: extentError}
		if got, err := plan.Spool(context.Background(), source); err != nil || !got.Valid() {
			test.Fatalf("known end-of-stream error rejected: %v, %v", extentError, err)
		}
	}
	first, err := plan.Block(context.Background(), 0)
	if err != nil {
		test.Fatal(err)
	}
	last, err := plan.Block(context.Background(), 1)
	if err != nil {
		test.Fatal(err)
	}
	prepopulated := &legacyByteSpoolTestSource{body: append(first.Bytes(), last.Bytes()...)}
	got, err := plan.Spool(context.Background(), prepopulated)
	legacyByteSpoolRequireZero(test, got, err, ErrIntegrity)
	shortProof, _ := legacyDocRangeGenerationFixture(test, 1)
	shortPlan, err := NewLegacyByteBackfill(context.Background(), shortProof)
	if err != nil {
		test.Fatal(err)
	}
	got, err = shortPlan.Spool(context.Background(), &legacyByteSpoolTestSource{appendMode: "extra"})
	legacyByteSpoolRequireZero(test, got, err, ErrIntegrity)
}

func TestLegacyByteBackfillSpooledBlocksRejectMutableSourceAndOwnBytes(test *testing.T) {
	proof, raw := legacyDocByteBackfillSizedFixture(test, 2*ChunkTarget+17)
	_, stream := chunkVerifierParts(test, raw)
	plan, spec, key := legacyDocByteBackfillRequirePlan(test, proof, stream)
	source := &legacyByteSpoolTestSource{}
	frozen, err := plan.Spool(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	original, err := frozen.Block(context.Background(), 1)
	if err != nil {
		test.Fatal(err)
	}
	source.body[ChunkTarget] ^= 1
	block, err := frozen.Block(context.Background(), 1)
	legacyByteSpoolRequireZeroBlock(test, block, err, ErrIntegrity)
	legacyDocByteBackfillRequireBlock(test, original, spec, key, 1, stream)
	unchanged, err := frozen.Block(context.Background(), 0)
	if err != nil {
		test.Fatal(err)
	}
	legacyDocByteBackfillRequireBlock(test, unchanged, spec, key, 0, stream)
	source.body[ChunkTarget] ^= 1
	failure := errors.New("current source read failed")
	for name, readError := range map[string]error{"unknown": failure, "missing": ErrNotFound, "integrity": ErrIntegrity, "canceled": context.Canceled} {
		test.Run(name, func(test *testing.T) {
			source.readError = readError
			block, err := frozen.Block(context.Background(), 1)
			legacyByteSpoolRequireZeroBlock(test, block, err, readError)
		})
	}
	source.readError = nil
	for _, mode := range []string{"short", "long", "empty", "corrupt"} {
		source.readMode = mode
		block, err := frozen.Block(context.Background(), 1)
		legacyByteSpoolRequireZeroBlock(test, block, err, ErrIntegrity)
	}
	source.readMode = ""
	source.body = source.body[:len(source.body)-1]
	block, err = frozen.Block(context.Background(), spec.Blocks-1)
	legacyByteSpoolRequireZeroBlock(test, block, err, ErrIntegrity)
	source.body = append(source.body, stream[len(stream)-1])
	source.reads = nil
	source.readError, source.failReadAt = failure, 2
	calls := 0
	progress, err := frozen.CopyBatch(context.Background(), 0, 8, func(ctx context.Context, block VerifiedLegacyByteBlock) error {
		legacyDocByteBackfillRequireBlock(test, block, spec, key, calls, stream)
		calls++
		return ctx.Err()
	})
	want := DocLegacyByteBackfillProgress{Plan: key, Next: 1, CopiedBytes: ChunkTarget}
	if !errors.Is(err, failure) || progress != want || calls != 1 {
		test.Fatalf("read failure acknowledged an unread block: %+v, %v, calls=%d", progress, err, calls)
	}
	source.readError = nil
	for _, ordinal := range []int{-1, spec.Blocks, int(^uint(0) >> 1)} {
		source.reads = nil
		block, err := frozen.Block(context.Background(), ordinal)
		legacyByteSpoolRequireZeroBlock(test, block, err, ErrValidation)
		if len(source.reads) != 0 {
			test.Fatal("unknown ordinal read the source")
		}
	}
}

func TestLegacyByteBackfillSpoolCancellationDuringWriteAndRead(test *testing.T) {
	proof, _ := legacyDocByteBackfillSizedFixture(test, 2*ChunkTarget+17)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	for _, cancelAppendAt := range []int{1, plan.spec.Blocks} {
		ctx, cancel := context.WithCancel(context.Background())
		source := &legacyByteSpoolTestSource{cancel: cancel, cancelAppendAt: cancelAppendAt}
		got, err := plan.Spool(ctx, source)
		cancel()
		legacyByteSpoolRequireZero(test, got, err, context.Canceled)
		if len(source.appends) != cancelAppendAt || len(source.reads) != 0 {
			test.Fatal("write cancellation continued writing or began readback")
		}
	}
	for _, cancelReadAt := range []int{1, plan.spec.Blocks, plan.spec.Blocks + 1, plan.spec.Blocks + 2} {
		ctx, cancel := context.WithCancel(context.Background())
		source := &legacyByteSpoolTestSource{cancel: cancel, cancelReadAt: cancelReadAt}
		got, err := plan.Spool(ctx, source)
		cancel()
		legacyByteSpoolRequireZero(test, got, err, context.Canceled)
		if len(source.reads) != cancelReadAt {
			test.Fatal("readback cancellation continued reading")
		}
	}
	source := &legacyByteSpoolTestSource{}
	frozen, err := plan.Spool(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source.reads, source.appends = nil, nil
	got, err := plan.Spool(ctx, source)
	legacyByteSpoolRequireZero(test, got, err, context.Canceled)
	block, err := frozen.Block(ctx, 0)
	legacyByteSpoolRequireZeroBlock(test, block, err, context.Canceled)
	if len(source.reads) != 0 || len(source.appends) != 0 {
		test.Fatal("already canceled call reached the source")
	}
	ctx, cancel = context.WithCancel(context.Background())
	source.cancel, source.cancelReadAt = cancel, 1
	block, err = frozen.Block(ctx, 0)
	cancel()
	legacyByteSpoolRequireZeroBlock(test, block, err, context.Canceled)
}

func TestLegacyByteBackfillSpoolEmptyAndRejectsReconstructedOrRootProofs(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, 0)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	source := &legacyByteSpoolTestSource{}
	frozen, err := plan.Spool(context.Background(), source)
	if err != nil || !frozen.Valid() || frozen.spec != plan.spec || frozen.key != plan.key || len(source.appends) != 0 || len(source.body) != 0 {
		test.Fatalf("empty spool acquired canonical blocks or changed identity: %v", err)
	}
	progress, err := frozen.CopyBatch(context.Background(), 0, 8, nil)
	if err != nil || progress != (DocLegacyByteBackfillProgress{Plan: plan.key, Complete: true}) {
		test.Fatalf("empty spool changed terminal progress: %+v, %v", progress, err)
	}
	block, err := frozen.Block(context.Background(), 0)
	legacyByteSpoolRequireZeroBlock(test, block, err, ErrValidation)
	got, err := plan.Spool(context.Background(), &legacyByteSpoolTestSource{body: []byte("unexpected")})
	legacyByteSpoolRequireZero(test, got, err, ErrIntegrity)
	got, err = plan.Spool(context.Background(), nil)
	legacyByteSpoolRequireZero(test, got, err, ErrIntegrity)
	legacy, raw := legacyDocRangeGenerationFixture(test, 1)
	valid, err := NewLegacyByteBackfill(context.Background(), legacy)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := valid.spec.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocLegacyByteBackfillSpec(valid.key, encoded)
	if err != nil {
		test.Fatal(err)
	}
	original, err := valid.Block(context.Background(), 0)
	if err != nil {
		test.Fatal(err)
	}
	descriptorBytes, err := json.Marshal([]ConversationManifestChunk{original.Descriptor()})
	if err != nil {
		test.Fatal(err)
	}
	var descriptors []ConversationManifestChunk
	if err := json.Unmarshal(descriptorBytes, &descriptors); err != nil {
		test.Fatal(err)
	}
	for name, invalid := range map[string]LegacyByteBackfill{
		"zero":                {},
		"serialized-metadata": {spec: decoded, key: valid.key, source: &legacyByteSpoolTestSource{body: original.Bytes()}, descriptors: descriptors},
		"raw-without-seal":    {spec: decoded, key: valid.key, stream: valid.stream, canonicalRetainedBytes: valid.canonicalRetainedBytes},
	} {
		test.Run(name, func(test *testing.T) {
			if invalid.Valid() {
				test.Fatal("metadata recreated a verified plan")
			}
			source := &legacyByteSpoolTestSource{}
			got, err := invalid.Spool(context.Background(), source)
			legacyByteSpoolRequireZero(test, got, err, ErrIntegrity)
			block, err := invalid.Block(context.Background(), 0)
			legacyByteSpoolRequireZeroBlock(test, block, err, ErrIntegrity)
			if retained, err := invalid.RetainedBytes(); !errors.Is(err, ErrIntegrity) || retained != 0 {
				test.Fatalf("invalid plan returned retained accounting: %d, %v", retained, err)
			}
			if len(source.appends) != 0 || len(source.reads) != 0 {
				test.Fatal("invalid plan reached the supplied source")
			}
		})
	}
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	root, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(test, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		test.Fatal(err)
	}
	rejected, err := NewLegacyByteBackfill(context.Background(), root)
	legacyByteSpoolRequireZero(test, rejected, err, ErrUnsupportedDocumentIdentity)
	got, err = rejected.Spool(context.Background(), &legacyByteSpoolTestSource{})
	legacyByteSpoolRequireZero(test, got, err, ErrIntegrity)
	rejected, err = NewLegacyByteBackfill(context.Background(), VerifiedSessionDoc{})
	legacyByteSpoolRequireZero(test, rejected, err, ErrIntegrity)
}

func TestLegacyByteBackfillRetainedBytesOwnsMetadataAndExcludesSourceStorage(test *testing.T) {
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{strings.Repeat("envelope", 16<<10)}}, Events: []CIREvent{
		{Kind: EventMessage, Role: RoleAssistant, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("x", 3*ChunkTarget)}}},
	}}
	proof, raw := legacyDocByteBackfillDocumentFixture(test, document)
	envelope, stream := chunkVerifierParts(test, raw)
	plan, _, _ := legacyDocByteBackfillRequirePlan(test, proof, stream)
	retained, err := plan.RetainedBytes()
	if err != nil || retained < int64(len(raw)) || plan.canonicalRetainedBytes != int64(len(raw)) {
		test.Fatalf("raw plan undercounted its full canonical backing: retained=%d, canonical=%d, raw=%d, error=%v", retained, plan.canonicalRetainedBytes, len(raw), err)
	}
	input := chunkVerifierPlan(envelope, stream, ChunkTarget-1)
	var verifier CanonicalDocVerifier
	chunked, err := verifier.VerifyChunks(context.Background(), proof.Hash(), input.Manifest, chunkVerifierLoad(input))
	if err != nil {
		test.Fatal(err)
	}
	chunkedPlan, err := NewLegacyByteBackfill(context.Background(), chunked)
	if err != nil {
		test.Fatal(err)
	}
	chunkedRetained, err := chunkedPlan.RetainedBytes()
	if err != nil || chunkedRetained < int64(len(stream)) || chunkedPlan.canonicalRetainedBytes != int64(len(stream)) {
		test.Fatalf("chunked plan undercounted its canonical segments: %d, %v", chunkedRetained, err)
	}
	hashBacking := strings.Repeat("h", len(raw)) + string(plan.spec.Reference.Hash)
	keyBacking := strings.Repeat("k", len(raw)) + string(plan.key)
	plan.spec.Reference.Hash = ContentHash(hashBacking[len(raw):])
	plan.key = ContentHash(keyBacking[len(raw):])
	source := &legacyByteSpoolTestSource{}
	frozen, err := plan.Spool(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	if unsafe.StringData(string(frozen.spec.Reference.Hash)) == unsafe.StringData(string(plan.spec.Reference.Hash)) ||
		unsafe.StringData(string(frozen.key)) == unsafe.StringData(string(plan.key)) {
		test.Fatal("spooled plan retained transcript-sized hash or key backing")
	}
	if frozen.stream.parts != nil || frozen.stream.offsets != nil || frozen.stream.size != 0 || frozen.canonicalRetainedBytes != 0 || frozen.source != source {
		test.Fatal("spooled plan retained its original plan, canonical strings, or a source wrapper")
	}
	frozenRetained, err := frozen.RetainedBytes()
	wantMetadata := int64(unsafe.Sizeof(frozen)) + int64(len(frozen.spec.Reference.Hash)+len(frozen.spec.Reference.Identity)+len(frozen.key))
	wantMetadata += int64(cap(frozen.descriptors)) * int64(unsafe.Sizeof(ConversationManifestChunk{}))
	for _, descriptor := range frozen.descriptors {
		wantMetadata += int64(len(descriptor.Hash))
	}
	sourceOwned := int64(unsafe.Sizeof(*source)) + int64(cap(source.body))
	sourceOwned += int64(cap(source.reads))*int64(unsafe.Sizeof(canonicalSpan{})) + int64(cap(source.appends))*int64(unsafe.Sizeof(int(0)))
	if err != nil || frozenRetained != wantMetadata || frozenRetained >= ChunkTarget/16 || sourceOwned < int64(len(stream)) {
		test.Fatalf("plan accounting included source-owned transcript storage or omitted metadata: plan=%d, metadata=%d, source=%d, error=%v", frozenRetained, wantMetadata, sourceOwned, err)
	}
	source.body = append(source.body, bytes.Repeat([]byte("source-owned"), ChunkTarget)...)
	if repeated, err := frozen.RetainedBytes(); err != nil || repeated != frozenRetained {
		test.Fatalf("source growth changed returned-plan accounting: %d, %v", repeated, err)
	}
}
