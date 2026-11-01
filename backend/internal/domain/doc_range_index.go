package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

const DocRangePageEvents = 128
const MaxDocRangePageBytes = 128 << 10
const MaxDocRangeIndexBytes = 32 << 20
const MaxDocRangePages = (MaxConversationDocumentBytes + DocRangePageEvents - 1) / DocRangePageEvents

var ErrDocRangeIndexUnavailable = errors.New("document range index is not ready")

type DocRangeEntry struct {
	DocEventIndex
	Kind           EventKind `json:"kind"`
	ToolName       string    `json:"tool_name,omitempty"`
	CompactSummary bool      `json:"compact_summary,omitempty"`
	SeedSummary    bool      `json:"seed_summary,omitempty"`
	EmptyReasoning bool      `json:"empty_reasoning,omitempty"`
	RenderedBefore int       `json:"rendered_before,omitempty"`
}

type DocRangeIndex struct {
	Version  int                  `json:"version"`
	Hash     ContentHash          `json:"hash"`
	Manifest ConversationManifest `json:"manifest"`
	Pages    []ContentHash        `json:"pages"`
}

type VerifiedDocRangeIndex struct {
	index                 DocRangeIndex
	read                  func(context.Context, int) ([]byte, error)
	presentationAlgorithm int
}

type DocRangePageBuffer interface {
	Append(context.Context, []byte) error
	Read(context.Context, int) ([]byte, error)
	Flush(context.Context) error
}

func (index VerifiedDocRangeIndex) Header() DocRangeIndex {
	raw, _ := json.Marshal(index.index)
	var copy DocRangeIndex
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func (index VerifiedDocRangeIndex) RetainedHeaderBytes() (int64, error) {
	if index.index.Version != 1 || index.index.Hash == "" || index.index.Pages == nil {
		return 0, ErrIntegrity
	}
	header := index.index
	estimate := docRangeHeaderHeapEstimate{}
	estimate.allocation(1, int64(unsafe.Sizeof(header)))
	estimate.allocation(int64(cap(header.Manifest.Envelope)), 1)
	estimate.allocation(int64(cap(header.Manifest.Chunks)), int64(unsafe.Sizeof(ConversationManifestChunk{})))
	estimate.allocation(int64(cap(header.Pages)), int64(unsafe.Sizeof(ContentHash(""))))
	for _, value := range []string{string(header.Hash), header.Manifest.Identity, header.Manifest.ChunkFormat} {
		estimate.allocation(int64(len(value)), 1)
	}
	for _, chunk := range header.Manifest.Chunks[:cap(header.Manifest.Chunks)] {
		estimate.allocation(int64(len(chunk.Hash)), 1)
	}
	for _, hash := range header.Pages[:cap(header.Pages)] {
		estimate.allocation(int64(len(hash)), 1)
	}
	if estimate.err != nil {
		return 0, estimate.err
	}
	return estimate.bytes, nil
}

type docRangeHeaderHeapEstimate struct {
	bytes int64
	err   error
}

func (estimate *docRangeHeaderHeapEstimate) include(other docRangeHeaderHeapEstimate) {
	if estimate.err != nil {
		return
	}
	if other.err != nil {
		estimate.err = other.err
		return
	}
	if estimate.bytes < 0 || other.bytes < 0 || estimate.bytes > math.MaxInt64-other.bytes {
		estimate.err = ErrIntegrity
		return
	}
	estimate.bytes += other.bytes
}

func (estimate *docRangeHeaderHeapEstimate) allocation(count, width int64) {
	if estimate.err != nil {
		return
	}
	const overhead = 64
	if count < 0 || width <= 0 || estimate.bytes < 0 || estimate.bytes > math.MaxInt64-overhead || count > (math.MaxInt64-overhead-estimate.bytes)/2/width {
		estimate.err = ErrIntegrity
		return
	}
	if count != 0 {
		estimate.bytes += overhead + 2*count*width
	}
}

func (index VerifiedDocRangeIndex) Page(ctx context.Context, position int) ([]byte, error) {
	raw, _, err := index.pageEntries(ctx, position)
	return raw, err
}

func (index VerifiedDocRangeIndex) pageEntries(ctx context.Context, position int) ([]byte, []DocRangeEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if index.read == nil || position < 0 || position >= len(index.index.Pages) {
		return nil, nil, ErrIntegrity
	}
	raw, err := index.read(ctx, position)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxDocRangePageBytes {
		return nil, nil, ErrIntegrity
	}
	raw = bytes.Clone(raw)
	entries, err := index.index.DecodePage(position, raw)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return raw, entries, nil
}

func (index DocRangeIndex) Validate(hash ContentHash) error {
	got, err := ConversationManifestHash(index.Manifest)
	if err != nil || index.Version != 1 || index.Hash != hash || got != hash || index.Pages == nil ||
		len(index.Pages) != (int(index.Manifest.EventCount)+DocRangePageEvents-1)/DocRangePageEvents || len(index.Pages) > MaxDocRangePages {
		return ErrIntegrity
	}
	for _, page := range index.Pages {
		if ValidateContentHash(page) != nil {
			return ErrIntegrity
		}
	}
	return nil
}

func (index DocRangeIndex) DecodePage(position int, raw []byte) ([]DocRangeEntry, error) {
	if position < 0 || position >= len(index.Pages) {
		return nil, ErrIntegrity
	}
	return decodeDocRangeEntries(position, index.Manifest.EventCount, index.Manifest.StreamBytes, index.Pages[position], raw)
}

func decodeDocRangeEntries(position int, events, streamBytes int64, expected ContentHash, raw []byte) ([]DocRangeEntry, error) {
	if position < 0 || int64(position) >= (events+DocRangePageEvents-1)/DocRangePageEvents || len(raw) > MaxDocRangePageBytes || HashContent(raw) != expected {
		return nil, ErrIntegrity
	}
	var entries []DocRangeEntry
	if json.Unmarshal(raw, &entries) != nil || len(entries) != min(DocRangePageEvents, int(events)-position*DocRangePageEvents) {
		return nil, ErrIntegrity
	}
	for offset, entry := range entries {
		if entry.Index != position*DocRangePageEvents+offset || entry.Offset < 0 || entry.Length <= 0 ||
			int64(entry.Offset) > streamBytes-int64(entry.Length) || ValidateContentHash(entry.Hash) != nil ||
			entry.RenderedBefore < 0 || entry.RenderedBefore > entry.Index ||
			(entry.EmptyReasoning && entry.Kind != EventReasoning) || (entry.SeedSummary && entry.Kind != EventMessage) {
			return nil, ErrIntegrity
		}
		if offset > 0 && entry.Offset != entries[offset-1].Offset+entries[offset-1].Length+1 {
			return nil, ErrIntegrity
		}
	}
	return entries, nil
}

type docRangeBuilder struct {
	result   VerifiedDocRangeIndex
	entries  []DocRangeEntry
	position int
	rendered int
	buffer   DocRangePageBuffer
	ctx      context.Context
}

func docRangeLabel(value string) string {
	count := 0
	for offset := range value {
		if count == 128 {
			return strings.Clone(value[:offset])
		}
		count++
	}
	return value
}

type docRangeJSONView []byte

func (view *docRangeJSONView) UnmarshalJSON(raw []byte) error {
	*view = raw
	return nil
}

func docRangeJSONValue(raw []byte) ([]byte, []byte) {
	value, rest, _ := docRangeJSONValueContext(nil, raw)
	return value, rest
}

func docRangeJSONValueContext(ctx context.Context, raw []byte) ([]byte, []byte, error) {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	depth := 0
	quoted, escaped := false, false
	for offset, character := range raw {
		if offset%canonicalCheckBytes == 0 && ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		if quoted {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				quoted = false
				if depth == 0 {
					return raw[:offset+1], raw[offset+1:], nil
				}
			}
			continue
		}
		switch character {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return raw[:offset], raw[offset:], nil
			}
			depth--
			if depth == 0 {
				return raw[:offset+1], raw[offset+1:], nil
			}
		case ',', ':', ' ', '\t', '\r', '\n':
			if depth == 0 {
				return raw[:offset], raw[offset:], nil
			}
		}
	}
	return raw, nil, nil
}

func docRangeJSONField(raw []byte, name string) []byte {
	value, _, _ := docRangeJSONFieldPosition(raw, name)
	return value
}

func docRangeJSONFieldPosition(raw []byte, name string) ([]byte, int, int) {
	originalLength := len(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, 0, 0
	}
	var result []byte
	from, to := 0, 0
	for raw = raw[1:]; len(raw) > 0; {
		raw = bytes.TrimLeft(raw, ", \t\r\n")
		if len(raw) == 0 || raw[0] == '}' {
			break
		}
		key, rest := docRangeJSONValue(raw)
		value, rest := docRangeJSONValue(bytes.TrimLeft(rest, ": \t\r\n"))
		if docRangeJSONTextEquals(key, name) {
			result = value
			from = originalLength - len(value) - len(rest)
			to = from + len(value)
		}
		raw = rest
	}
	return result, from, to
}

func docRangeJSONRune(raw []byte) (rune, int) {
	if raw[0] != '\\' {
		return utf8.DecodeRune(raw)
	}
	switch raw[1] {
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'u':
		value, _ := strconv.ParseUint(string(raw[2:6]), 16, 16)
		character := rune(value)
		if utf16.IsSurrogate(character) {
			if len(raw) >= 12 && raw[6] == '\\' && raw[7] == 'u' {
				next, _ := strconv.ParseUint(string(raw[8:12]), 16, 16)
				if decoded := utf16.DecodeRune(character, rune(next)); decoded != utf8.RuneError {
					return decoded, 12
				}
			}
			return utf8.RuneError, 6
		}
		return character, 6
	default:
		return rune(raw[1]), 2
	}
}

func docRangeJSONTextEquals(raw []byte, value string) bool {
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	raw = raw[1 : len(raw)-1]
	for _, expected := range value {
		if len(raw) == 0 {
			return false
		}
		character, width := docRangeJSONRune(raw)
		if character != expected {
			return false
		}
		raw = raw[width:]
	}
	return len(raw) == 0
}

func docRangePresentationSpace(character rune) bool {
	switch character {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return character >= '\u2000' && character <= '\u200a'
}

func docRangeJSONText(raw []byte, limit int, trimStart bool) string {
	if len(raw) < 2 || raw[0] != '"' {
		return ""
	}
	var prefix [128 * utf8.UTFMax]byte
	length, count := 0, 0
	for raw = raw[1 : len(raw)-1]; len(raw) > 0 && count < min(limit, 128); {
		character, width := docRangeJSONRune(raw)
		raw = raw[width:]
		if trimStart && count == 0 && docRangePresentationSpace(character) {
			continue
		}
		length += utf8.EncodeRune(prefix[length:], character)
		count++
	}
	return string(prefix[:length])
}

func docRangeSeedSummary(raw []byte) bool {
	const branchPrefix = "[cxt seed] Branch-switch context:"
	const resumedPrefix = "[cxt] This session was resumed from a branch context seed."
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	for raw = raw[1:]; len(raw) > 0; {
		raw = bytes.TrimLeft(raw, ", \t\r\n")
		if len(raw) == 0 || raw[0] == ']' {
			break
		}
		block, rest := docRangeJSONValue(raw)
		prefix := docRangeJSONText(docRangeJSONField(block, "text"), len(resumedPrefix), true)
		if prefix != "" {
			return strings.HasPrefix(prefix, branchPrefix) || strings.HasPrefix(prefix, resumedPrefix)
		}
		raw = rest
	}
	return false
}

func docRangeEntry(position, offset int, body []byte, sequence int) (DocRangeEntry, error) {
	return docRangeEntryWithSequence(position, offset, body, sequence, true)
}

type docRangeMetadata = struct {
	Seq             docRangeJSONView `json:"seq"`
	Kind            EventKind        `json:"kind"`
	Role            string           `json:"role"`
	ToolName        string           `json:"tool_name"`
	CompactSummary  bool             `json:"compact_summary"`
	Blocks          docRangeJSONView `json:"blocks"`
	RedactedSummary docRangeJSONView `json:"redacted_summary"`
}

func docRangeEntryWithSequence(position, offset int, body []byte, sequence int, sequenceKnown bool) (DocRangeEntry, error) {
	var metadata docRangeMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return DocRangeEntry{}, err
	}
	return docRangeMetadataEntry(nil, metadata, position, offset, body, sequence, sequenceKnown)
}

func docRangeMetadataEntry(ctx context.Context, metadata docRangeMetadata, position, offset int, body []byte, sequence int, sequenceKnown bool) (DocRangeEntry, error) {
	if !sequenceKnown && len(metadata.Seq) > 0 {
		if err := json.Unmarshal(metadata.Seq, &sequence); err != nil {
			return DocRangeEntry{}, err
		}
	}
	if len(metadata.RedactedSummary) > 0 && metadata.RedactedSummary[0] != '"' && !bytes.Equal(metadata.RedactedSummary, []byte("null")) ||
		len(metadata.Blocks) > 0 && metadata.Blocks[0] != '[' && !bytes.Equal(metadata.Blocks, []byte("null")) {
		return DocRangeEntry{}, ErrIntegrity
	}
	var hash ContentHash
	if ctx == nil {
		hash = HashContent(body)
	} else {
		var err error
		hash, err = HashContentContext(ctx, body)
		if err != nil {
			return DocRangeEntry{}, err
		}
	}
	return DocRangeEntry{
		DocEventIndex: DocEventIndex{Index: position, Offset: offset, Length: len(body), Hash: hash, Seq: sequence, Role: docRangeLabel(metadata.Role)},
		Kind:          metadata.Kind, ToolName: docRangeLabel(metadata.ToolName), CompactSummary: metadata.CompactSummary,
		SeedSummary:    metadata.Kind == EventMessage && docRangeSeedSummary(metadata.Blocks),
		EmptyReasoning: metadata.Kind == EventReasoning && docRangeJSONText(metadata.RedactedSummary, 1, true) == "",
	}, nil
}

func (builder *docRangeBuilder) add(position int64, body []byte, sequence int) error {
	entry, err := docRangeEntry(int(position), builder.position, body, sequence)
	if err != nil {
		return err
	}
	return builder.addEntry(entry)
}

func (builder *docRangeBuilder) addVerified(position int64, body []byte, sequence int) error {
	entry, err := canonicalDocRangeEntry(builder.ctx, int(position), builder.position, body, sequence, true)
	if err != nil {
		return err
	}
	return builder.addEntry(entry)
}

func (builder *docRangeBuilder) addEntry(entry DocRangeEntry) error {
	entry.RenderedBefore = builder.rendered
	if !entry.EmptyReasoning {
		builder.rendered++
	}
	builder.entries = append(builder.entries, entry)
	builder.position += entry.Length + 1
	if len(builder.entries) == DocRangePageEvents {
		return builder.flush()
	}
	return nil
}

func (builder *docRangeBuilder) flush() error {
	if len(builder.entries) == 0 {
		return nil
	}
	raw, err := json.Marshal(builder.entries)
	if err != nil {
		return err
	}
	if len(raw) > MaxDocRangePageBytes || len(builder.result.index.Pages) >= MaxDocRangePages {
		return fmt.Errorf("%w: document range metadata exceeds limit", ErrValidation)
	}
	hash := HashContent(raw)
	if err := builder.buffer.Append(builder.ctx, raw); err != nil {
		return err
	}
	builder.result.index.Pages = append(builder.result.index.Pages, hash)
	builder.entries = builder.entries[:0]
	return nil
}

func (builder *docRangeBuilder) seal() (VerifiedDocRangeIndex, error) {
	if err := builder.flush(); err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	if err := builder.buffer.Flush(builder.ctx); err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	if err := builder.ctx.Err(); err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	raw, err := json.Marshal(builder.result.index)
	if err != nil || len(raw) > MaxDocRangeIndexBytes {
		return VerifiedDocRangeIndex{}, fmt.Errorf("%w: document range header exceeds limit", ErrValidation)
	}
	if err := builder.ctx.Err(); err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	builder.result.read = builder.buffer.Read
	builder.result.presentationAlgorithm = 1
	return builder.result, nil
}

type verifiedDocRangePages struct {
	doc            VerifiedSessionDoc
	renderedBefore []int
}

func (pages *verifiedDocRangePages) Flush(ctx context.Context) error {
	return ctx.Err()
}

func (pages *verifiedDocRangePages) Append(ctx context.Context, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var entries []struct {
		RenderedBefore int `json:"rendered_before"`
	}
	if len(raw) > MaxDocRangePageBytes || json.Unmarshal(raw, &entries) != nil || len(entries) == 0 || len(entries) > DocRangePageEvents {
		return ErrIntegrity
	}
	pages.renderedBefore = append(pages.renderedBefore, entries[0].RenderedBefore)
	return ctx.Err()
}

func (pages *verifiedDocRangePages) Read(ctx context.Context, position int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if position < 0 || position >= len(pages.renderedBefore) || position >= (len(pages.doc.chunks.events)+DocRangePageEvents-1)/DocRangePageEvents {
		return nil, ErrIntegrity
	}
	start := position * DocRangePageEvents
	rendered := pages.renderedBefore[position]
	entries := make([]DocRangeEntry, 0, DocRangePageEvents)
	for ordinal := start; ordinal < min(start+DocRangePageEvents, len(pages.doc.chunks.events)); ordinal++ {
		event := pages.doc.chunks.events[ordinal]
		body, err := pages.doc.EventStreamRange(ctx, event.span.offset, event.span.length)
		if err != nil {
			return nil, err
		}
		entry, err := canonicalDocRangeEntry(ctx, ordinal, event.span.offset, body, event.seq, true)
		if err != nil {
			return nil, err
		}
		entry.RenderedBefore = rendered
		if !entry.EmptyReasoning {
			rendered++
		}
		entries = append(entries, entry)
	}
	return json.Marshal(entries)
}

func BuildDocRangeIndex(ctx context.Context, doc VerifiedSessionDoc) (VerifiedDocRangeIndex, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	manifest, ok := doc.ConversationManifest()
	if !doc.Valid() || !ok || doc.chunks == nil {
		return VerifiedDocRangeIndex{}, ErrUnsupportedDocumentIdentity
	}
	builder := docRangeBuilder{ctx: ctx, buffer: &verifiedDocRangePages{doc: doc}, result: VerifiedDocRangeIndex{index: DocRangeIndex{Version: 1, Hash: doc.Hash(), Manifest: manifest, Pages: []ContentHash{}}}}
	for position, event := range doc.chunks.events {
		body, err := doc.EventStreamRange(ctx, event.span.offset, event.span.length)
		if err != nil {
			return VerifiedDocRangeIndex{}, err
		}
		if err := builder.addVerified(int64(position), body, event.seq); err != nil {
			return VerifiedDocRangeIndex{}, err
		}
	}
	return builder.seal()
}

func (verifier *CanonicalDocVerifier) VerifyConversationRangeIndex(ctx context.Context, hash ContentHash, manifest ConversationManifest, load func(context.Context, ContentHash) ([]byte, error), buffer DocRangePageBuffer) (VerifiedDocRangeIndex, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	if buffer == nil {
		return VerifiedDocRangeIndex{}, ErrValidation
	}
	manifest.Envelope = append(json.RawMessage(nil), manifest.Envelope...)
	if manifest.Chunks != nil {
		chunks := make([]ConversationManifestChunk, len(manifest.Chunks))
		copy(chunks, manifest.Chunks)
		manifest.Chunks = chunks
	}
	builder := docRangeBuilder{ctx: ctx, buffer: buffer, result: VerifiedDocRangeIndex{index: DocRangeIndex{Version: 1, Hash: hash, Manifest: manifest, Pages: []ContentHash{}}}}
	var pending []canonicalEventProof
	check := func(ctx context.Context, version string, body []byte) (int, error) {
		return verifier.eventWithKey(ctx, canonicalEventKey{hash: HashContent(body), version: version}, body, &pending)
	}
	if err := verifyConversationManifest(ctx, hash, manifest, load, check, builder.addVerified); err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	index, err := builder.seal()
	if err != nil {
		return VerifiedDocRangeIndex{}, err
	}
	verifier.remember(pending)
	return index, nil
}

type DocViewEvent struct {
	Event    CIREvent
	Index    int
	Deferred bool
	Bytes    int
}

func (event DocViewEvent) MarshalJSON() ([]byte, error) {
	if event.Deferred {
		return json.Marshal(struct {
			Kind           EventKind `json:"kind"`
			Seq            int       `json:"seq"`
			Role           Role      `json:"role,omitempty"`
			ToolName       string    `json:"tool_name,omitempty"`
			CompactSummary bool      `json:"compact_summary,omitempty"`
			Index          int       `json:"event_index"`
			Bytes          int       `json:"event_bytes"`
			Deferred       bool      `json:"deferred"`
		}{event.Event.Kind, event.Event.Seq, event.Event.Role, event.Event.ToolName, event.Event.CompactSummary, event.Index, event.Bytes, true})
	}
	raw, err := json.Marshal(event.Event)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["event_index"], _ = json.Marshal(event.Index)
	fields["event_bytes"], _ = json.Marshal(event.Bytes)
	return json.Marshal(fields)
}

type DocViewPage struct {
	Hash        ContentHash      `json:"hash"`
	DocIdentity DocumentIdentity `json:"doc_identity"`
	Envelope    CIREnvelope      `json:"envelope"`
	Events      []DocViewEvent   `json:"events"`
	Total       int              `json:"total"`
	Offset      int              `json:"offset"`
	Next        int              `json:"next"`
	Inherited   int              `json:"inherited"`
}

type DocViewFragment struct {
	Index  int    `json:"event_index"`
	Offset int    `json:"byte_offset"`
	Next   int    `json:"next"`
	Total  int    `json:"total_bytes"`
	JSON   string `json:"json_fragment"`
}
