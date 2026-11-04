package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"unsafe"
)

type LegacyByteSource struct {
	Bytes                int
	PartBytes            int
	Parts                []ConversationManifestChunk
	Read                 func(context.Context, int, int) ([]byte, error)
	authority            uint64
	checkpointReaderHeap docRangeHeaderHeapEstimate
}

const LegacyNormalizationPartBytes = LegacyCanonicalEncodingBytes

func (source LegacyByteSource) partBytes() int {
	switch source.PartBytes {
	case 0, ConversationManifestChunkBytes:
		return ConversationManifestChunkBytes
	case LegacyNormalizationPartBytes:
		return LegacyNormalizationPartBytes
	default:
		return 0
	}
}

type LegacyByteSourceProgress uint8

const (
	LegacyByteSourcePending LegacyByteSourceProgress = iota + 1
	LegacyByteSourceVerified
	LegacyByteSourceOriginalRequired
)

type VerifiedLegacyByteSource struct {
	plan LegacyByteBackfill
	seed LegacyRangeMetadataSeed
}

func (proof VerifiedLegacyByteSource) Valid() bool {
	return proof.plan.Valid() && proof.plan.source != nil && proof.seed.valid() &&
		proof.plan.spec.Reference == proof.seed.reference && proof.plan.key == proof.seed.key && proof.plan.spec.StreamBytes == proof.seed.bytes
}

func (proof VerifiedLegacyByteSource) Plan() (LegacyByteBackfill, error) {
	if !proof.Valid() {
		return LegacyByteBackfill{}, ErrIntegrity
	}
	return proof.plan, nil
}

func (proof VerifiedLegacyByteSource) Seed() (LegacyRangeMetadataSeed, error) {
	if !proof.Valid() {
		return LegacyRangeMetadataSeed{}, ErrIntegrity
	}
	return proof.seed, nil
}

func (proof VerifiedLegacyByteSource) Source() VerifiedDocStream {
	if !proof.Valid() {
		return nil
	}
	return proof.plan.source
}

type legacySourceFrame struct {
	shape         canonicalFrameTokenState
	envelopeEnd   int
	envelopeHash  ContentHash
	envelopeState []byte
	streamStart   int
	middle        int
	closed        bool
	failure       error
}

func (frame *legacySourceFrame) add(ctx context.Context, body []byte, offset, extent int) error {
	if frame.failure != nil {
		return nil
	}
	from, to := -1, 0
	for index := 0; index < len(body); index++ {
		if index%canonicalCheckBytes == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		position := offset + index
		current := body[index]
		if position >= extent-len(canonicalDocSuffix) {
			if extent < canonicalFrameBytes+2 || current != canonicalDocSuffix[position-(extent-len(canonicalDocSuffix))] {
				frame.failure = ErrIntegrity
				return nil
			}
			continue
		}
		if position < len(canonicalDocPrefix) {
			if current != canonicalDocPrefix[position] {
				frame.failure = ErrIntegrity
				return nil
			}
			continue
		}
		if frame.streamStart != 0 {
			index += min(len(body)-index-1, extent-len(canonicalDocSuffix)-position-1)
			continue
		}
		if !frame.closed {
			if position == len(canonicalDocPrefix) && current != '{' {
				frame.failure = ErrIntegrity
				return nil
			}
			if from < 0 {
				from = index
			}
			to = index + 1
			closed, err := frame.shape.add(current)
			if err != nil {
				frame.failure = err
				return nil
			}
			frame.closed = closed
			if closed {
				frame.envelopeEnd = position + 1
			}
			continue
		}
		if frame.middle >= len(canonicalDocMiddle) || current != canonicalDocMiddle[frame.middle] {
			frame.failure = ErrIntegrity
			return nil
		}
		frame.middle++
		if frame.middle == len(canonicalDocMiddle) {
			frame.streamStart = position + 1
		}
	}
	if from >= 0 {
		var err error
		frame.envelopeState, err = legacySourceHashAppend(ctx, frame.envelopeState, body[from:to])
		if err != nil {
			return err
		}
		if frame.closed {
			frame.envelopeHash, err = legacySourceHashSum(ctx, frame.envelopeState)
			if err != nil {
				return err
			}
			frame.envelopeState = nil
		}
	}
	return ctx.Err()
}

type LegacyByteSourcePreparation struct {
	want         ContentHash
	source       LegacyByteSource
	target       VerifiedDocStream
	verifier     *CanonicalDocVerifier
	frame        legacySourceFrame
	envelope     *legacySourceEnvelope
	header       []byte
	headerCopied int
	prefix       []byte
	hashState    []byte
	actual       ContentHash
	position     int
	phase        uint8
	version      string
	spec         DocLegacyByteBackfillSpec
	key          ContentHash
	copied       int
	verified     int
	parts        []ConversationManifestChunk
	pending      []canonicalEventProof
	scanner      legacyStreamScanner
	textBytes    int64
	result       VerifiedLegacyByteSource
	status       LegacyByteSourceProgress
	failure      error
}

func NewLegacyByteSourcePreparation(ctx context.Context, want ContentHash, source LegacyByteSource, target VerifiedDocStream, verifier *CanonicalDocVerifier) (*LegacyByteSourcePreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(want) != nil || source.Bytes < 0 || source.Bytes > MaxFinalizedDocBytes || source.Read == nil || target == nil || verifier == nil ||
		source.PartBytes != 0 && source.PartBytes != ConversationManifestChunkBytes ||
		len(source.Parts) != (source.Bytes+ConversationManifestChunkBytes-1)/ConversationManifestChunkBytes {
		return nil, ErrIntegrity
	}
	source.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
	preparation := &LegacyByteSourcePreparation{want: ContentHash(strings.Clone(string(want))), source: source, target: target, verifier: verifier, status: LegacyByteSourcePending}
	for ordinal := range source.Parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part := &source.Parts[ordinal]
		if ValidateContentHash(part.Hash) != nil || part.Bytes != int64(min(ConversationManifestChunkBytes, source.Bytes-ordinal*ConversationManifestChunkBytes)) {
			return nil, ErrIntegrity
		}
		part.Hash = ContentHash(strings.Clone(string(part.Hash)))
		preparation.textBytes += int64(len(part.Hash))
	}
	return preparation, nil
}

func (preparation *LegacyByteSourcePreparation) input(ctx context.Context, ordinal int) ([]byte, error) {
	return readLegacyByteSourcePart(ctx, preparation.source, ordinal)
}

func readLegacyByteSourcePart(ctx context.Context, source LegacyByteSource, ordinal int) ([]byte, error) {
	partBytes := source.partBytes()
	if partBytes == 0 || source.Bytes < 0 || ordinal < 0 || ordinal >= len(source.Parts) || ordinal > source.Bytes/partBytes {
		return nil, ErrIntegrity
	}
	part := source.Parts[ordinal]
	offset := ordinal * partBytes
	if offset >= source.Bytes || part.Bytes != int64(min(partBytes, source.Bytes-offset)) {
		return nil, ErrIntegrity
	}
	body, err := source.Read(ctx, offset, int(part.Bytes))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) != int(part.Bytes) {
		return nil, ErrPreparedDocRangeObject
	}
	body = bytes.Clone(body)
	hash, err := HashContentContext(ctx, body)
	if err != nil {
		return nil, err
	}
	if hash != part.Hash {
		return nil, ErrPreparedDocRangeObject
	}
	return body, nil
}

func (preparation *LegacyByteSourcePreparation) span(ctx context.Context, offset, length int) ([]byte, error) {
	if offset < 0 || length < 0 || length > ConversationManifestChunkBytes || offset > preparation.source.Bytes || length > preparation.source.Bytes-offset {
		return nil, ErrIntegrity
	}
	body := make([]byte, length)
	for copied := 0; copied < length; {
		position := offset + copied
		part, err := preparation.input(ctx, position/ConversationManifestChunkBytes)
		if err != nil {
			return nil, err
		}
		read := copy(body[copied:], part[position%ConversationManifestChunkBytes:])
		if read == 0 {
			return nil, ErrPreparedDocRangeObject
		}
		copied += read
	}
	return body, ctx.Err()
}

func legacySourceEOF(ctx context.Context, read func(context.Context, int, int) ([]byte, error), extent int) error {
	body, err := read(ctx, extent, 0)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if err != nil || len(body) != 0 {
		return errors.Join(ErrPreparedDocRangeObject, err)
	}
	body, err = read(ctx, extent, 1)
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if len(body) != 0 || err == nil {
		return ErrPreparedDocRangeObject
	}
	if errors.Is(err, ErrPreparedDocRangeObject) {
		return err
	}
	if !errors.Is(err, ErrIntegrity) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	return nil
}

func (preparation *LegacyByteSourcePreparation) hash(ctx context.Context) error {
	digest := sha256.New()
	if len(preparation.hashState) != 0 {
		if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(preparation.hashState); err != nil {
			return ErrIntegrity
		}
	}
	if preparation.position < preparation.source.Bytes {
		body, err := preparation.input(ctx, preparation.position/ConversationManifestChunkBytes)
		if err != nil {
			return err
		}
		for offset := 0; offset < len(body); offset += canonicalCheckBytes {
			if err := ctx.Err(); err != nil {
				return err
			}
			digest.Write(body[offset:min(offset+canonicalCheckBytes, len(body))])
		}
		if len(preparation.prefix) < 64 {
			preparation.prefix = append(preparation.prefix, body[:min(64-len(preparation.prefix), len(body))]...)
		}
		if err := preparation.frame.add(ctx, body, preparation.position, preparation.source.Bytes); err != nil {
			return err
		}
		preparation.position += len(body)
	}
	var err error
	preparation.hashState, err = digest.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return err
	}
	if preparation.position == preparation.source.Bytes {
		preparation.actual = ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
		preparation.phase = 1
	}
	return ctx.Err()
}

func (preparation *LegacyByteSourcePreparation) decide(ctx context.Context) error {
	if preparation.envelope == nil {
		if err := legacySourceEOF(ctx, preparation.source.Read, preparation.source.Bytes); err != nil {
			return err
		}
		if preparation.actual != preparation.want || docChunkManifestCandidate(preparation.prefix) {
			preparation.status = LegacyByteSourceOriginalRequired
			return nil
		}
		if preparation.frame.failure != nil || preparation.frame.streamStart == 0 || preparation.frame.streamStart > preparation.source.Bytes-len(canonicalDocSuffix) {
			return ErrIntegrity
		}
		var err error
		preparation.envelope, err = newLegacySourceEnvelope(len(canonicalDocPrefix), preparation.frame.envelopeEnd, preparation.frame.envelopeHash)
		return err
	}
	complete, err := preparation.envelope.step(ctx, preparation.source)
	if errors.Is(err, ErrUnsupportedCIRVersion) {
		preparation.envelope.release()
		preparation.envelope = nil
		preparation.status = LegacyByteSourceOriginalRequired
		return nil
	}
	if err != nil {
		return err
	}
	if !complete {
		return nil
	}
	preparation.version = strings.Clone(preparation.envelope.version)
	preparation.envelope.release()
	preparation.envelope = nil
	streamBytes := preparation.source.Bytes - len(canonicalDocSuffix) - preparation.frame.streamStart
	preparation.spec = DocLegacyByteBackfillSpec{Version: 1, Reference: DocumentRef{Hash: preparation.want, Identity: DocumentIdentityLegacy},
		StreamBytes: int64(streamBytes), BlockBytes: ConversationManifestChunkBytes, Blocks: (streamBytes + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes}
	encoded, err := preparation.spec.Encode()
	if err != nil {
		return err
	}
	preparation.key = HashContent(encoded)
	preparation.scanner = legacyStreamScanner{limit: streamBytes}
	preparation.phase = 5
	return ctx.Err()
}

func (preparation *LegacyByteSourcePreparation) copyHeader(ctx context.Context) error {
	length := preparation.frame.envelopeEnd - len(canonicalDocPrefix)
	if length <= 0 {
		return ErrIntegrity
	}
	if preparation.header == nil {
		preparation.header = make([]byte, length)
		return ctx.Err()
	}
	if preparation.headerCopied < length {
		body, err := preparation.span(ctx, len(canonicalDocPrefix)+preparation.headerCopied, min(LegacyCanonicalEncodingBytes, length-preparation.headerCopied))
		if err != nil {
			return err
		}
		copy(preparation.header[preparation.headerCopied:], body)
		preparation.headerCopied += len(body)
		return ctx.Err()
	}
	if err := legacySourceEOF(ctx, preparation.source.Read, preparation.source.Bytes); err != nil {
		return err
	}
	preparation.phase = 2
	if preparation.spec.Blocks == 0 {
		preparation.phase = 4
	}
	return nil
}

func (preparation *LegacyByteSourcePreparation) copy(ctx context.Context) error {
	offset := preparation.copied * ConversationManifestChunkBytes
	length := min(ConversationManifestChunkBytes, int(preparation.spec.StreamBytes)-offset)
	body, err := preparation.span(ctx, preparation.frame.streamStart+offset, length)
	if err != nil {
		return err
	}
	preparation.scanner.visit = func(_ canonicalSpan, body []byte) (int, error) {
		before := len(preparation.pending)
		sequence, err := preparation.verifier.event(ctx, preparation.version, body, &preparation.pending)
		if len(preparation.pending) > before {
			proof := preparation.pending[len(preparation.pending)-1]
			preparation.textBytes += int64(len(proof.key.hash) + len(proof.key.version))
		}
		return sequence, err
	}
	err = preparation.scanner.add(ctx, body)
	preparation.scanner.visit = nil
	if err != nil {
		if errors.Is(err, ErrUnsupportedCIRVersion) {
			preparation.status = LegacyByteSourceOriginalRequired
			return nil
		}
		return err
	}
	hash, err := HashContentContext(ctx, body)
	if err != nil {
		return err
	}
	if err := preparation.target.Append(ctx, body); err != nil {
		return err
	}
	preparation.parts = append(preparation.parts, ConversationManifestChunk{Hash: hash, Bytes: int64(len(body))})
	preparation.textBytes += int64(len(hash))
	preparation.copied++
	if preparation.copied == preparation.spec.Blocks {
		if err := preparation.scanner.finish(ctx); err != nil {
			return err
		}
		preparation.scanner = legacyStreamScanner{}
		preparation.phase = 3
	}
	return ctx.Err()
}

func (preparation *LegacyByteSourcePreparation) seal(ctx context.Context) error {
	if err := legacySourceEOF(ctx, preparation.target.Read, int(preparation.spec.StreamBytes)); err != nil {
		return err
	}
	plan := LegacyByteBackfill{spec: preparation.spec, key: preparation.key, source: preparation.target, descriptors: preparation.parts, sealed: true}
	seed := LegacyRangeMetadataSeed{reference: preparation.spec.Reference, key: preparation.key,
		envelope: preparation.header, bytes: preparation.spec.StreamBytes, sealed: true}
	proof := VerifiedLegacyByteSource{plan: plan, seed: seed}
	if !proof.Valid() {
		return ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	preparation.verifier.remember(preparation.pending)
	preparation.result, preparation.status = proof, LegacyByteSourceVerified
	preparation.pending, preparation.hashState, preparation.prefix = nil, nil, nil
	preparation.source, preparation.scanner = LegacyByteSource{}, legacyStreamScanner{}
	preparation.textBytes = 0
	for _, part := range preparation.parts {
		preparation.textBytes += int64(len(part.Hash))
	}
	return nil
}

func (preparation *LegacyByteSourcePreparation) Step(ctx context.Context) (progress LegacyByteSourceProgress, failure error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if preparation == nil {
		return 0, ErrIntegrity
	}
	if preparation.failure != nil {
		return 0, preparation.failure
	}
	if preparation.status != LegacyByteSourcePending {
		return preparation.status, nil
	}
	defer func() {
		if failure != nil {
			preparation.failure = failure
			if preparation.envelope != nil {
				preparation.envelope.release()
				preparation.envelope = nil
			}
			preparation.header = nil
		}
	}()
	switch preparation.phase {
	case 0:
		failure = preparation.hash(ctx)
	case 1:
		failure = preparation.decide(ctx)
	case 2:
		failure = preparation.copy(ctx)
	case 3:
		_, failure = legacyByteBackfillRead(ctx, preparation.target, preparation.verified*ConversationManifestChunkBytes, preparation.parts[preparation.verified])
		if failure == nil {
			preparation.verified++
			if preparation.verified == preparation.spec.Blocks {
				preparation.phase = 4
			}
		}
	case 4:
		failure = preparation.seal(ctx)
	case 5:
		failure = preparation.copyHeader(ctx)
	default:
		failure = ErrIntegrity
	}
	if failure != nil {
		return 0, failure
	}
	if preparation.status == LegacyByteSourceVerified {
		return preparation.status, nil
	}
	return preparation.status, ctx.Err()
}

func (preparation *LegacyByteSourcePreparation) Result() (VerifiedLegacyByteSource, error) {
	if preparation == nil || preparation.failure != nil || !preparation.result.Valid() {
		return VerifiedLegacyByteSource{}, ErrDocRangeIndexPending
	}
	return preparation.result, nil
}

func (preparation *LegacyByteSourcePreparation) NormalizationEligible() bool {
	return preparation != nil && preparation.failure == nil && preparation.status == LegacyByteSourceOriginalRequired &&
		preparation.actual != preparation.want && !docChunkManifestCandidate(preparation.prefix)
}

func (preparation *LegacyByteSourcePreparation) RetainedBytes() (int64, error) {
	if preparation == nil || preparation.textBytes < 0 {
		return 0, ErrIntegrity
	}
	estimate := docRangeHeaderHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(*preparation)))
	estimate.allocation(preparation.textBytes, 1)
	estimate.allocation(int64(cap(preparation.header)+cap(preparation.frame.envelopeState)+cap(preparation.prefix)+cap(preparation.hashState)+cap(preparation.scanner.event)), 1)
	if preparation.envelope != nil {
		retained, err := preparation.envelope.retained()
		if err != nil {
			return 0, err
		}
		estimate.include(docRangeHeaderHeapEstimate{bytes: retained})
	}
	estimate.allocation(int64(cap(preparation.source.Parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	estimate.allocation(int64(cap(preparation.parts)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	estimate.allocation(int64(cap(preparation.pending)), int64(unsafe.Sizeof(canonicalEventProof{})))
	for _, value := range []string{string(preparation.want), string(preparation.actual), preparation.version, string(preparation.key), string(preparation.frame.envelopeHash)} {
		estimate.allocation(int64(len(value)), 1)
	}
	return estimate.bytes, estimate.err
}
