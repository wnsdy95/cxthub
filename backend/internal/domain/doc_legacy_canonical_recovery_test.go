package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type legacyCanonicalRecoveryRead struct {
	segment string
	offset  int
	length  int
}

type legacyCanonicalRecoveryBuffer struct {
	test     testing.TB
	envelope []byte
	stream   []byte
	reads    []legacyCanonicalRecoveryRead
}

func (buffer *legacyCanonicalRecoveryBuffer) read(ctx context.Context, segment string, offset, length int) ([]byte, error) {
	buffer.test.Helper()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := buffer.envelope
	if segment == "stream" {
		body = buffer.stream
	}
	if offset < 0 || length <= 0 || offset > len(body) || length > len(body)-offset || length > 512<<10 {
		buffer.test.Fatalf("invalid %s callback range: %d + %d / %d", segment, offset, length, len(body))
	}
	buffer.reads = append(buffer.reads, legacyCanonicalRecoveryRead{segment: segment, offset: offset, length: length})
	return body[offset : offset+length], nil
}

func (buffer *legacyCanonicalRecoveryBuffer) source() LegacyCanonicalDocumentSource {
	return LegacyCanonicalDocumentSource{
		EnvelopeBytes: len(buffer.envelope), StreamBytes: len(buffer.stream),
		ReadEnvelope: func(ctx context.Context, offset, length int) ([]byte, error) {
			return buffer.read(ctx, "envelope", offset, length)
		},
		ReadStream: func(ctx context.Context, offset, length int) ([]byte, error) {
			return buffer.read(ctx, "stream", offset, length)
		},
	}
}

func legacyCanonicalRecoveryHash(body []byte) ContentHash {
	digest := sha256.Sum256(body)
	return ContentHash("sha256:" + hex.EncodeToString(digest[:]))
}

func legacyCanonicalRecoveryCheckRead(test *testing.T, buffer *legacyCanonicalRecoveryBuffer, source LegacyCanonicalDocumentSource, oracle []byte, offset, length int) []byte {
	test.Helper()
	before := len(buffer.reads)
	body, err := source.Read(context.Background(), offset, length)
	if err != nil || !bytes.Equal(body, oracle[offset:offset+length]) {
		test.Fatalf("canonical range %d + %d changed assembled bytes: %v", offset, length, err)
	}
	for _, segment := range []struct {
		name   string
		start  int
		length int
	}{
		{name: "envelope", start: len(`{"envelope":`), length: len(buffer.envelope)},
		{name: "stream", start: len(`{"envelope":`) + len(buffer.envelope) + len(`,"events":[`), length: len(buffer.stream)},
	} {
		from, to := max(offset, segment.start), min(offset+length, segment.start+segment.length)
		readBytes := 0
		for _, read := range buffer.reads[before:] {
			if read.segment != segment.name {
				continue
			}
			if read.offset+segment.start < from || read.offset+segment.start+read.length > to {
				test.Fatalf("callback escaped requested %s intersection: %+v, %d..%d", segment.name, read, from, to)
			}
			readBytes += read.length
		}
		if readBytes != max(0, to-from) {
			test.Fatalf("%s callback bytes = %d, want %d", segment.name, readBytes, max(0, to-from))
		}
	}
	return body
}

func TestLegacyCanonicalRecoverySourceAssembleOracleBoundaries(test *testing.T) {
	for _, fixture := range []struct {
		envelopeBytes int
		streamBytes   int
	}{
		{2, 0}, {64<<10 - 1, 64<<10 + 1}, {64 << 10, 512 << 10},
		{64<<10 + 1, 512<<10 + 1}, {512<<10 - 1, 64<<10 - 1},
		{512 << 10, 0}, {512<<10 + 1, 2*(512<<10) + 17},
	} {
		test.Run(fmt.Sprintf("header-%d-stream-%d", fixture.envelopeBytes, fixture.streamBytes), func(test *testing.T) {
			envelope := []byte(`{}`)
			if fixture.envelopeBytes > 2 {
				envelope = []byte(`{"cwd":"` + strings.Repeat("h", fixture.envelopeBytes-len(`{"cwd":""}`)) + `"}`)
			}
			var stream []byte
			if fixture.streamBytes > 0 {
				stream = []byte(`{"text":"` + strings.Repeat("s", fixture.streamBytes-len(`{"text":""}`)) + `"}`)
			}
			beforeEnvelope, beforeStream := bytes.Clone(envelope), bytes.Clone(stream)
			oracle := AssembleCanonicalDoc(envelope, [][]byte{stream})
			buffer := &legacyCanonicalRecoveryBuffer{test: test, envelope: envelope, stream: stream}
			source := buffer.source()
			if extent, err := source.Size(); err != nil || extent != len(oracle) || len(buffer.reads) != 0 {
				test.Fatal("Size changed framing or eagerly read a callback", extent, len(oracle), err)
			}
			for _, width := range []int{64<<10 - 1, 64 << 10, 64<<10 + 1, 512<<10 - 1, 512 << 10} {
				var joined []byte
				for offset := 0; offset < len(oracle); offset += width {
					joined = append(joined, legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, offset, min(width, len(oracle)-offset))...)
				}
				if !bytes.Equal(joined, oracle) || legacyCanonicalRecoveryHash(joined) != legacyCanonicalRecoveryHash(oracle) {
					test.Fatal("partitioning changed canonical bytes or SHA-256", width)
				}
			}
			for _, boundary := range []int{
				0, len(`{"envelope":`), len(`{"envelope":`) + len(envelope),
				len(`{"envelope":`) + len(envelope) + len(`,"events":[`), len(oracle) - len(`]}`), len(oracle),
				64<<10 - 1, 64 << 10, 64<<10 + 1, 512<<10 - 1, 512 << 10, 512<<10 + 1,
			} {
				if boundary > len(oracle) {
					continue
				}
				offset := max(0, boundary-3)
				legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, offset, min(9, len(oracle)-offset))
				legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, boundary, 0)
			}
			if !bytes.Equal(envelope, beforeEnvelope) || !bytes.Equal(stream, beforeStream) {
				test.Fatal("canonical framing mutated callback-owned input")
			}
		})
	}
}

func TestLegacyCanonicalRecoverySourceSizeRejectsInvalidAndOverflowWithoutReads(test *testing.T) {
	calls := 0
	read := func(context.Context, int, int) ([]byte, error) {
		calls++
		return nil, errors.New("Size must not read")
	}
	frameBytes := len(AssembleCanonicalDoc(nil, nil))
	maximumInt := int(^uint(0) >> 1)
	for _, fixture := range []struct {
		name     string
		envelope int
		stream   int
		missing  string
		want     error
	}{
		{name: "minimum", envelope: 2},
		{name: "exact-finalized-limit", envelope: 2, stream: MaxFinalizedDocBytes - frameBytes - 2},
		{name: "large-header-exact-limit", envelope: MaxFinalizedDocBytes - frameBytes},
		{name: "empty-header", want: ErrIntegrity},
		{name: "one-byte-header", envelope: 1, want: ErrIntegrity},
		{name: "negative-header", envelope: -1, want: ErrIntegrity},
		{name: "negative-stream", envelope: 2, stream: -1, want: ErrIntegrity},
		{name: "nil-envelope-reader", envelope: 2, missing: "envelope", want: ErrIntegrity},
		{name: "nil-empty-stream-reader", envelope: 2, missing: "stream", want: ErrIntegrity},
		{name: "one-over-limit", envelope: 2, stream: MaxFinalizedDocBytes - frameBytes - 1, want: ErrDocumentResourceLimit},
		{name: "header-over-limit", envelope: MaxFinalizedDocBytes, want: ErrDocumentResourceLimit},
		{name: "maximum-header-int", envelope: maximumInt, want: ErrDocumentResourceLimit},
		{name: "maximum-stream-int", envelope: 2, stream: maximumInt, want: ErrDocumentResourceLimit},
		{name: "sum-wraparound", envelope: maximumInt, stream: maximumInt, want: ErrDocumentResourceLimit},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			source := LegacyCanonicalDocumentSource{EnvelopeBytes: fixture.envelope, StreamBytes: fixture.stream, ReadEnvelope: read, ReadStream: read}
			if fixture.missing == "envelope" {
				source.ReadEnvelope = nil
			}
			if fixture.missing == "stream" {
				source.ReadStream = nil
			}
			extent, err := source.Size()
			if !errors.Is(err, fixture.want) || calls != 0 {
				test.Fatal("Size changed validation or invoked a callback", extent, err, calls)
			}
			if fixture.want == nil {
				if extent != frameBytes+fixture.envelope+fixture.stream {
					test.Fatal("Size omitted a frame segment", extent)
				}
				return
			}
			body, readErr := source.Read(context.Background(), 0, 0)
			if extent != 0 || !errors.Is(readErr, fixture.want) || body != nil || calls != 0 {
				test.Fatal("invalid source accepted a zero-length read", extent, readErr, calls)
			}
		})
	}
}

func TestLegacyCanonicalRecoverySourceReadRangeLimitAndEOF(test *testing.T) {
	buffer := &legacyCanonicalRecoveryBuffer{test: test, envelope: []byte(`{}`), stream: bytes.Repeat([]byte("s"), 2*(512<<10)+17)}
	source := buffer.source()
	oracle := AssembleCanonicalDoc(buffer.envelope, [][]byte{buffer.stream})
	maximumInt := int(^uint(0) >> 1)
	for _, fixture := range []struct {
		name   string
		offset int
		length int
		want   error
	}{
		{"negative-offset", -1, 0, ErrIntegrity}, {"negative-length", 0, -1, ErrIntegrity},
		{"past-EOF-zero", len(oracle) + 1, 0, ErrIntegrity}, {"EOF-one", len(oracle), 1, ErrIntegrity},
		{"past-last-byte", len(oracle) - 1, 2, ErrIntegrity}, {"offset-overflow", maximumInt, 1, ErrIntegrity},
		{"length-overflow", 1, maximumInt, ErrIntegrity}, {"sum-overflow", maximumInt, maximumInt, ErrIntegrity},
		{"over-512KiB", 0, 512<<10 + 1, ErrDocumentResourceLimit},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			before := len(buffer.reads)
			body, err := source.Read(context.Background(), fixture.offset, fixture.length)
			if !errors.Is(err, fixture.want) || body != nil || len(buffer.reads) != before {
				test.Fatal("invalid range returned bytes or called a backing reader", err, len(buffer.reads)-before)
			}
		})
	}
	for _, offset := range []int{0, len(`{"envelope":`), len(oracle) / 2, len(oracle)} {
		legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, offset, 0)
	}
	legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, 0, 512<<10)
	legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, len(oracle)-(512<<10), 512<<10)
	legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, len(oracle)-1, 1)
}

func TestLegacyCanonicalRecoverySourceOwnsOutputAndCopiesReusableCallbacks(test *testing.T) {
	envelope := []byte(`{"cwd":"한🙂\\\"\n"}`)
	stream := []byte(`{"text":"first"},{"text":"second"}`)
	wantEnvelope, wantStream := bytes.Clone(envelope), bytes.Clone(stream)
	oracle := AssembleCanonicalDoc(envelope, [][]byte{stream})
	buffer := &legacyCanonicalRecoveryBuffer{test: test, envelope: envelope, stream: stream}
	source := buffer.source()
	first := legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, 0, len(oracle))
	second := legacyCanonicalRecoveryCheckRead(test, buffer, source, oracle, 0, len(oracle))
	first[0] ^= 1
	first[len(`{"envelope":`)] ^= 1
	first[len(first)-3] ^= 1
	if !bytes.Equal(second, oracle) || !bytes.Equal(envelope, wantEnvelope) || !bytes.Equal(stream, wantStream) {
		test.Fatal("returned output aliases another output or callback input")
	}
	envelope[0] ^= 1
	stream[0] ^= 1
	if !bytes.Equal(second, oracle) {
		test.Fatal("later backing mutation changed a completed output")
	}
	copy(envelope, wantEnvelope)
	copy(stream, wantStream)
	scratch := make([]byte, max(len(envelope), len(stream)))
	source.ReadEnvelope = func(_ context.Context, offset, length int) ([]byte, error) {
		copy(scratch, envelope[offset:offset+length])
		return scratch[:length], nil
	}
	source.ReadStream = func(_ context.Context, offset, length int) ([]byte, error) {
		copy(scratch, stream[offset:offset+length])
		return scratch[:length], nil
	}
	reused, err := source.Read(context.Background(), 0, len(oracle))
	if err != nil || !bytes.Equal(reused, oracle) {
		test.Fatal("stream callback reused and corrupted the already-read envelope", err)
	}
	for index := range scratch {
		scratch[index] ^= 1
	}
	if !bytes.Equal(reused, oracle) {
		test.Fatal("completed output retained callback scratch backing")
	}
}

func TestLegacyCanonicalRecoverySourceCallbackErrorsLengthsAndContextPriority(test *testing.T) {
	sentinel := errors.New("recovery callback failure")
	for _, segment := range []string{"envelope", "stream"} {
		for _, response := range []string{"nil", "short", "long", "error", "EOF", "unexpected-EOF", "exact"} {
			for _, cancelInside := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/%s/cancel-%t", segment, response, cancelInside), func(test *testing.T) {
					buffer := &legacyCanonicalRecoveryBuffer{test: test, envelope: []byte(`{"cwd":"header"}`), stream: []byte(`{"text":"stream"}`)}
					source := buffer.source()
					ctx, cancel := context.WithCancel(context.WithValue(context.Background(), legacyCanonicalRecoveryRead{}, "current-read"))
					defer cancel()
					calls := 0
					callback := func(current context.Context, offset, length int) ([]byte, error) {
						calls++
						if current != ctx {
							test.Fatal("callback did not receive the current read context")
						}
						if cancelInside {
							cancel()
						}
						body := buffer.envelope
						if segment == "stream" {
							body = buffer.stream
						}
						body = body[offset : offset+length]
						switch response {
						case "nil":
							return nil, nil
						case "short":
							return body[:len(body)-1], nil
						case "long":
							return append(bytes.Clone(body), 'x'), nil
						case "error":
							return body, sentinel
						case "EOF":
							return body, io.EOF
						case "unexpected-EOF":
							return body, io.ErrUnexpectedEOF
						default:
							return body, nil
						}
					}
					if segment == "envelope" {
						source.ReadEnvelope = callback
					} else {
						source.ReadStream = callback
					}
					want := ErrPreparedDocRangeObject
					switch response {
					case "error":
						want = sentinel
					case "EOF":
						want = io.EOF
					case "unexpected-EOF":
						want = io.ErrUnexpectedEOF
					case "exact":
						want = nil
					}
					if cancelInside {
						want = context.Canceled
					}
					oracle := AssembleCanonicalDoc(buffer.envelope, [][]byte{buffer.stream})
					body, err := source.Read(ctx, 0, len(oracle))
					if !errors.Is(err, want) || calls != 1 || want != nil && body != nil || want == nil && !bytes.Equal(body, oracle) {
						test.Fatal("callback failure/length/cancellation precedence changed", calls, err, want)
					}
					if segment == "envelope" && want != nil && len(buffer.reads) != 0 {
						test.Fatal("failed envelope callback did not stop the stream read")
					}
				})
			}
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stopDeadline := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stopDeadline()
	buffer := &legacyCanonicalRecoveryBuffer{test: test, envelope: []byte(`{}`), stream: []byte(`{}`)}
	for _, ctx := range []context.Context{canceled, expired} {
		for _, source := range []LegacyCanonicalDocumentSource{{}, buffer.source()} {
			for _, selected := range [][2]int{{0, 0}, {-1, -1}, {0, 512<<10 + 1}} {
				body, err := source.Read(ctx, selected[0], selected[1])
				if !errors.Is(err, ctx.Err()) || body != nil || len(buffer.reads) != 0 {
					test.Fatal("current context did not precede source/range/limit validation", err)
				}
			}
		}
	}
}

type legacyCanonicalRecoveryVerified struct {
	seed     LegacyRangeMetadataSeed
	plan     LegacyByteBackfill
	proof    VerifiedSessionDoc
	raw      []byte
	envelope []byte
	stream   []byte
}

func legacyCanonicalRecoveryFixture(test testing.TB, headerPadding, eventPadding, eventCount int) legacyCanonicalRecoveryVerified {
	test.Helper()
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2, SourceProvider: ProviderCodex, Fidelity: FidelityFull,
		Cwd: "한🙂/\"\\\n" + strings.Repeat("h", headerPadding), SourceModels: []string{"same", "same", "last"}}, Events: []CIREvent{}}
	for ordinal := 0; ordinal < eventCount; ordinal++ {
		document.Events = append(document.Events, CIREvent{Kind: EventMessage, Seq: 7 + 4*ordinal, Role: RoleUser,
			Blocks: []ContentBlock{{Type: "text", Text: fmt.Sprintf("recovery-%d:", ordinal) + strings.Repeat("x", eventPadding) + "한🙂\"\\\n"}}})
	}
	proof, raw := legacyDocByteBackfillDocumentFixture(test, document)
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	seed, err := NewLegacyRangeMetadataSeed(ctx, proof, plan)
	cancel()
	if err != nil {
		test.Fatal(err)
	}
	envelope, stream := chunkVerifierParts(test, raw)
	return legacyCanonicalRecoveryVerified{seed: seed, plan: plan, proof: proof, raw: raw, envelope: envelope, stream: stream}
}

func TestLegacyCanonicalRecoverySeedEnvelopeReadBoundsAndOwnership(test *testing.T) {
	fixture := legacyCanonicalRecoveryFixture(test, 512<<10+17, 512<<10+19, 1)
	seed := fixture.seed
	before := bytes.Clone(fixture.raw)
	if seed.EnvelopeBytes() != len(fixture.envelope) || seed.Reference() != fixture.proof.DocumentRef() || seed.PlanKey() != fixture.plan.key {
		test.Fatal("seed envelope extent included canonical frame/stream or changed identity")
	}
	for _, width := range []int{64<<10 - 1, 64 << 10} {
		var joined []byte
		for offset := 0; offset < seed.EnvelopeBytes(); offset += width {
			length := min(width, seed.EnvelopeBytes()-offset)
			body, err := seed.ReadEnvelope(context.Background(), offset, length)
			if err != nil || !bytes.Equal(body, fixture.envelope[offset:offset+length]) {
				test.Fatal("seed changed exact canonical envelope bytes", offset, length, err)
			}
			joined = append(joined, body...)
			body[0] ^= 1
			repeated, err := seed.ReadEnvelope(context.Background(), offset, length)
			if err != nil || !bytes.Equal(repeated, fixture.envelope[offset:offset+length]) {
				test.Fatal("returned envelope aliases the sealed seed or another output", err)
			}
		}
		if !bytes.Equal(joined, fixture.envelope) || legacyCanonicalRecoveryHash(joined) != legacyCanonicalRecoveryHash(fixture.envelope) {
			test.Fatal("64KiB seed partition changed envelope SHA-256")
		}
	}
	maximumInt := int(^uint(0) >> 1)
	for _, fixtureRange := range []struct {
		offset int
		length int
		want   error
	}{
		{-1, 0, ErrIntegrity}, {0, -1, ErrIntegrity}, {seed.EnvelopeBytes() + 1, 0, ErrIntegrity},
		{seed.EnvelopeBytes(), 1, ErrIntegrity}, {seed.EnvelopeBytes() - 1, 2, ErrIntegrity},
		{maximumInt, 1, ErrIntegrity}, {1, maximumInt, ErrIntegrity}, {maximumInt, maximumInt, ErrIntegrity},
		{0, 64<<10 + 1, ErrDocumentResourceLimit}, {64 << 10, 64<<10 + 1, ErrDocumentResourceLimit},
	} {
		body, err := seed.ReadEnvelope(context.Background(), fixtureRange.offset, fixtureRange.length)
		if !errors.Is(err, fixtureRange.want) || body != nil {
			test.Fatal("seed range/overflow/64KiB validation changed", fixtureRange.offset, fixtureRange.length, err)
		}
	}
	for _, offset := range []int{0, 64<<10 - 1, 64 << 10, 64<<10 + 1, seed.EnvelopeBytes()} {
		body, err := seed.ReadEnvelope(context.Background(), offset, 0)
		if err != nil || len(body) != 0 {
			test.Fatal("seed rejected an in-range zero-length/EOF read", offset, err)
		}
		if offset < seed.EnvelopeBytes() {
			body, err = seed.ReadEnvelope(context.Background(), offset, 1)
			if err != nil || !bytes.Equal(body, fixture.envelope[offset:offset+1]) {
				test.Fatal("seed boundary seek changed a byte", offset, err)
			}
		}
	}
	first, err := seed.ReadEnvelope(context.Background(), 1, 17)
	if err != nil {
		test.Fatal(err)
	}
	second, err := seed.ReadEnvelope(context.Background(), 1, 17)
	if err != nil {
		test.Fatal(err)
	}
	first[0] ^= 1
	if !bytes.Equal(second, fixture.envelope[1:18]) {
		test.Fatal("independent seed reads share mutable output backing")
	}
	if !bytes.Equal(fixture.raw, before) || !bytes.Equal(fixture.proof.Bytes(), before) || seed.EnvelopeBytes() != len(fixture.envelope) {
		test.Fatal("seed reads mutated original bytes, proof, or declared envelope extent")
	}
}

func TestLegacyCanonicalRecoverySeedRejectsSerializedAuthorityAndCanceledReads(test *testing.T) {
	fixture := legacyCanonicalRecoveryFixture(test, 64<<10+17, 32, 1)
	encoded, err := json.Marshal(fixture.seed)
	if err != nil {
		test.Fatal(err)
	}
	var decoded LegacyRangeMetadataSeed
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		test.Fatal(err)
	}
	spoofedBytes, err := json.Marshal(map[string]any{
		"reference": fixture.seed.Reference(), "key": fixture.seed.PlanKey(), "envelope": json.RawMessage(fixture.envelope),
		"bytes": len(fixture.stream), "sealed": true, "EnvelopeBytes": len(fixture.envelope),
	})
	if err != nil {
		test.Fatal(err)
	}
	var spoofed LegacyRangeMetadataSeed
	if err := json.Unmarshal(spoofedBytes, &spoofed); err != nil {
		test.Fatal(err)
	}
	unsealed := fixture.seed
	unsealed.sealed = false
	wrongReference, wrongKey, missingEnvelope, negativeStream, rootIdentity := fixture.seed, fixture.seed, fixture.seed, fixture.seed, fixture.seed
	wrongReference.reference.Hash = "invalid"
	wrongKey.key = "invalid"
	missingEnvelope.envelope = nil
	negativeStream.bytes = -1
	rootIdentity.reference.Identity = DocumentIdentityRootV1
	for _, seed := range []LegacyRangeMetadataSeed{{}, decoded, spoofed, unsealed, wrongReference, wrongKey, missingEnvelope, negativeStream, rootIdentity} {
		if seed.valid() || seed.EnvelopeBytes() != 0 || seed.Reference() != (DocumentRef{}) || seed.PlanKey() != "" {
			test.Fatal("persisted bytes or unsealed fields minted seed authority")
		}
		body, err := seed.ReadEnvelope(context.Background(), 0, 0)
		if !errors.Is(err, ErrIntegrity) || body != nil {
			test.Fatal("invalid seed accepted even a zero-length read", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stopDeadline := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stopDeadline()
	for _, ctx := range []context.Context{canceled, expired} {
		for _, seed := range []LegacyRangeMetadataSeed{{}, fixture.seed} {
			for _, selected := range [][2]int{{0, 0}, {-1, -1}, {0, 64<<10 + 1}} {
				body, err := seed.ReadEnvelope(ctx, selected[0], selected[1])
				if !errors.Is(err, ctx.Err()) || body != nil {
					test.Fatal("seed validation took precedence over the current context", err)
				}
			}
		}
	}
}

func legacyCanonicalRecoveryPreparation(test *testing.T, source LegacyCanonicalDocumentSource, descriptorBytes []byte, want ContentHash, verifier *CanonicalDocVerifier) (*LegacyByteSourcePreparation, *legacySourceProofBuffer) {
	test.Helper()
	extent, err := source.Size()
	if err != nil || extent != len(descriptorBytes) {
		test.Fatal("recovery fixture descriptor extent mismatch", extent, len(descriptorBytes), err)
	}
	input := LegacyByteSource{Bytes: extent, Read: source.Read}
	for offset := 0; offset < extent; offset += 512 << 10 {
		body := descriptorBytes[offset:min(offset+(512<<10), extent)]
		input.Parts = append(input.Parts, ConversationManifestChunk{Hash: legacyCanonicalRecoveryHash(body), Bytes: int64(len(body))})
	}
	output := &legacySourceProofBuffer{}
	preparation, err := NewLegacyByteSourcePreparation(context.Background(), want, input, output, verifier)
	if err != nil {
		test.Fatal(err)
	}
	return preparation, output
}

func legacyCanonicalRecoveryDrain(test *testing.T, preparation *LegacyByteSourcePreparation, verifier *CanonicalDocVerifier) (LegacyByteSourceProgress, error) {
	test.Helper()
	for stepIndex := 0; stepIndex < 6000; stepIndex++ {
		proof, resultErr := preparation.Result()
		if proof.Valid() || !errors.Is(resultErr, ErrDocRangeIndexPending) {
			test.Fatal("unverified recovery bytes acquired a publication proof", stepIndex, resultErr)
		}
		progress, err := preparation.Step(context.Background())
		if verifier.events != nil || cap(verifier.order) != 0 || verifier.next != 0 || cap(preparation.pending) != 0 {
			test.Fatal("uncached recovery retained event cache/order/pending storage", stepIndex)
		}
		if err != nil || progress != LegacyByteSourcePending {
			return progress, err
		}
	}
	test.Fatal("finite recovery fixture did not terminate")
	return 0, ErrIntegrity
}

func TestLegacyCanonicalRecoveryFreshUncachedProofPreservesOriginalBytesPlanAndSeed(test *testing.T) {
	for _, fixtureSize := range []struct {
		name   string
		header int
		event  int
		count  int
	}{
		{"empty-stream", 0, 0, 0}, {"large-header-empty-stream", 512<<10 + 17, 0, 0},
		{"large-header-and-stream", 512<<10 + 17, 512<<10 + 19, 2},
	} {
		test.Run(fixtureSize.name, func(test *testing.T) {
			fixture := legacyCanonicalRecoveryFixture(test, fixtureSize.header, fixtureSize.event, fixtureSize.count)
			buffer := &legacyCanonicalRecoveryBuffer{test: test, envelope: fixture.envelope, stream: fixture.stream}
			source := buffer.source()
			seedReads := 0
			source.ReadEnvelope = func(ctx context.Context, offset, length int) ([]byte, error) {
				body := make([]byte, 0, length)
				for copied := 0; copied < length; {
					part, err := fixture.seed.ReadEnvelope(ctx, offset+copied, min(64<<10, length-copied))
					if err != nil {
						return nil, err
					}
					seedReads++
					body = append(body, part...)
					copied += len(part)
				}
				return body, nil
			}
			verifier := NewCanonicalDocVerifierWithoutCache()
			preparation, output := legacyCanonicalRecoveryPreparation(test, source, fixture.raw, fixture.proof.Hash(), verifier)
			progress, err := legacyCanonicalRecoveryDrain(test, preparation, verifier)
			if err != nil || progress != LegacyByteSourceVerified || seedReads == 0 {
				test.Fatal("fresh uncached recovery failed", progress, err, seedReads)
			}
			proof, err := preparation.Result()
			if err != nil || !proof.Valid() || proof.Source() != output || !bytes.Equal(output.body, fixture.stream) {
				test.Fatal("recovery proof lost its checked stream backing", err)
			}
			plan, err := proof.Plan()
			if err != nil {
				test.Fatal(err)
			}
			spec, err := plan.Spec()
			wantSpec, wantErr := fixture.plan.Spec()
			key, keyErr := plan.Key()
			if err != nil || wantErr != nil || keyErr != nil || spec != wantSpec || key != fixture.seed.PlanKey() {
				test.Fatal("recovery changed original reference, spec, or plan key", err, wantErr, keyErr)
			}
			seed, err := proof.Seed()
			if err != nil || seed.EnvelopeBytes() != len(fixture.envelope) || seed.Reference() != fixture.seed.Reference() || seed.PlanKey() != key {
				test.Fatal("recovery changed the sealed envelope identity", err)
			}
			var envelope []byte
			for offset := 0; offset < seed.EnvelopeBytes(); offset += 64 << 10 {
				part, err := seed.ReadEnvelope(context.Background(), offset, min(64<<10, seed.EnvelopeBytes()-offset))
				if err != nil {
					test.Fatal(err)
				}
				envelope = append(envelope, part...)
			}
			canonical := AssembleCanonicalDoc(envelope, [][]byte{output.body})
			if !bytes.Equal(canonical, fixture.raw) || legacyCanonicalRecoveryHash(canonical) != fixture.proof.Hash() || !bytes.Equal(fixture.proof.Bytes(), fixture.raw) {
				test.Fatal("fresh recovery changed original canonical bytes or SHA-256")
			}
			encoded, err := json.Marshal(proof)
			if err != nil {
				test.Fatal(err)
			}
			var decoded VerifiedLegacyByteSource
			if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Valid() {
				test.Fatal("serialized recovery proof restored a trust seal", err)
			}
		})
	}
}

func TestLegacyCanonicalRecoveryArbitraryBytesAndSHAWithoutSemanticsCannotMintProof(test *testing.T) {
	fixture := legacyCanonicalRecoveryFixture(test, 0, 32, 2)
	for _, name := range []string{"stale-part-hash", "stale-document-hash", "regressing-sequence", "noncanonical-envelope", "truncated-event"} {
		test.Run(name, func(test *testing.T) {
			envelope, stream := bytes.Clone(fixture.envelope), bytes.Clone(fixture.stream)
			want := fixture.proof.Hash()
			switch name {
			case "stale-part-hash", "stale-document-hash":
				stream = bytes.Replace(stream, []byte("recovery-0"), []byte("Recovery-0"), 1)
			case "regressing-sequence":
				stream = bytes.Replace(stream, []byte(`"seq":11`), []byte(`"seq":-1`), 1)
			case "noncanonical-envelope":
				envelope = append(bytes.Clone(envelope[:len(envelope)-1]), ' ', '}')
			case "truncated-event":
				stream = stream[:len(stream)-1]
			}
			canonical := AssembleCanonicalDoc(envelope, [][]byte{stream})
			if bytes.Equal(canonical, fixture.raw) {
				test.Fatal("corruption fixture failed to change source bytes")
			}
			descriptors := canonical
			if name == "stale-part-hash" {
				descriptors = fixture.raw
			} else if name != "stale-document-hash" {
				want = legacyCanonicalRecoveryHash(canonical)
				oracle := NewCanonicalDocVerifierWithoutCache()
				if proof, err := oracle.Verify(context.Background(), want, canonical); !errors.Is(err, ErrIntegrity) || proof.Valid() {
					test.Fatal("semantic corruption fixture passed the original verifier", err)
				}
			}
			buffer := &legacyCanonicalRecoveryBuffer{test: test, envelope: envelope, stream: stream}
			verifier := NewCanonicalDocVerifierWithoutCache()
			preparation, _ := legacyCanonicalRecoveryPreparation(test, buffer.source(), descriptors, want, verifier)
			progress, err := legacyCanonicalRecoveryDrain(test, preparation, verifier)
			switch name {
			case "stale-part-hash":
				if !errors.Is(err, ErrPreparedDocRangeObject) {
					test.Fatal("recovery ignored the authenticated canonical part hash", progress, err)
				}
			case "stale-document-hash":
				if err != nil || progress != LegacyByteSourceOriginalRequired {
					test.Fatal("part hashes substituted for the original complete-CIR SHA", progress, err)
				}
			default:
				if !errors.Is(err, ErrIntegrity) {
					test.Fatal("self-consistent SHA/descriptors bypassed semantic verification", progress, err)
				}
			}
			if proof, err := preparation.Result(); proof.Valid() || !errors.Is(err, ErrDocRangeIndexPending) {
				test.Fatal("untrusted recovery bytes minted a sealed source/seed", err)
			}
		})
	}
}

func TestLegacyCanonicalRecoveryUncachedVerifierChecksEveryEventWithoutRetainingProofs(test *testing.T) {
	fixture := legacyCanonicalRecoveryFixture(test, 0, 32, 1)
	verifier := NewCanonicalDocVerifierWithoutCache()
	key := canonicalEventKey{hash: legacyCanonicalRecoveryHash(fixture.stream), version: CIRVersionV2}
	var pending []canonicalEventProof
	invalid := fixture.stream[:len(fixture.stream)-1]
	for attempt := 0; attempt < 3; attempt++ {
		sequence, err := verifier.event(context.Background(), CIRVersionV2, fixture.stream, &pending)
		if err != nil || sequence != 7 {
			test.Fatal("uncached mode changed original event verification", sequence, err)
		}
		if _, err := verifier.eventWithKey(context.Background(), key, invalid, &pending); !errors.Is(err, ErrIntegrity) {
			test.Fatal("a previously valid key authorized repeated malformed event bytes", attempt, err)
		}
		if _, err := verifier.event(context.Background(), CIRVersionV2, invalid, &pending); !errors.Is(err, ErrIntegrity) {
			test.Fatal("uncached mode accepted repeated malformed bytes", attempt, err)
		}
		proof, err := verifier.Verify(context.Background(), fixture.proof.Hash(), fixture.raw)
		if err != nil || !proof.Valid() || !bytes.Equal(proof.Bytes(), fixture.raw) {
			test.Fatal("uncached mode skipped or changed the complete-document trust seal", err)
		}
		verifier.remember([]canonicalEventProof{{key: key, seq: 7}})
		if verifier.events != nil || cap(verifier.order) != 0 || verifier.next != 0 || cap(pending) != 0 {
			test.Fatal("uncached event/Verify/remember retained cache, order, or pending storage")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verifier.event(canceled, CIRVersionV2, fixture.stream, &pending); !errors.Is(err, context.Canceled) {
		test.Fatal("uncached event verification ignored the current context", err)
	}
	var cached CanonicalDocVerifier
	var cachedPending []canonicalEventProof
	if _, err := cached.event(context.Background(), CIRVersionV2, fixture.stream, &cachedPending); err != nil || len(cachedPending) != 1 {
		test.Fatal("the original zero-value verifier lost event caching", err)
	}
	cached.remember(cachedPending)
	if len(cached.events) != 1 || len(cached.order) != 1 {
		test.Fatal("the original zero-value cache admission changed")
	}
}
