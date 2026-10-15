package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
}

type DocRangeIndex struct {
	Version  int                  `json:"version"`
	Hash     ContentHash          `json:"hash"`
	Manifest ConversationManifest `json:"manifest"`
	Pages    []ContentHash        `json:"pages"`
}

type VerifiedDocRangeIndex struct {
	index DocRangeIndex
	read  func(context.Context, int) ([]byte, error)
}

type DocRangePageBuffer interface {
	Append(context.Context, []byte) error
	Read(context.Context, int) ([]byte, error)
}

func (index VerifiedDocRangeIndex) Header() DocRangeIndex {
	raw, _ := json.Marshal(index.index)
	var copy DocRangeIndex
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func (index VerifiedDocRangeIndex) Page(ctx context.Context, position int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if index.read == nil || position < 0 || position >= len(index.index.Pages) {
		return nil, ErrIntegrity
	}
	raw, err := index.read(ctx, position)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxDocRangePageBytes {
		return nil, ErrIntegrity
	}
	raw = bytes.Clone(raw)
	if _, err := index.index.DecodePage(position, raw); err != nil {
		return nil, err
	}
	return raw, ctx.Err()
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
	if position < 0 || position >= len(index.Pages) || len(raw) > MaxDocRangePageBytes || HashContent(raw) != index.Pages[position] {
		return nil, ErrIntegrity
	}
	var entries []DocRangeEntry
	if json.Unmarshal(raw, &entries) != nil || len(entries) != min(DocRangePageEvents, int(index.Manifest.EventCount)-position*DocRangePageEvents) {
		return nil, ErrIntegrity
	}
	for offset, entry := range entries {
		if entry.Index != position*DocRangePageEvents+offset || entry.Offset < 0 || entry.Length <= 0 ||
			int64(entry.Offset) > index.Manifest.StreamBytes-int64(entry.Length) || ValidateContentHash(entry.Hash) != nil {
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

func docRangeEntry(position, offset int, body []byte, sequence int) (DocRangeEntry, error) {
	var metadata struct {
		Kind           EventKind `json:"kind"`
		Role           string    `json:"role"`
		ToolName       string    `json:"tool_name"`
		CompactSummary bool      `json:"compact_summary"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return DocRangeEntry{}, err
	}
	return DocRangeEntry{DocEventIndex: DocEventIndex{Index: position, Offset: offset, Length: len(body), Hash: HashContent(body), Seq: sequence, Role: docRangeLabel(metadata.Role)}, Kind: metadata.Kind, ToolName: docRangeLabel(metadata.ToolName), CompactSummary: metadata.CompactSummary}, nil
}

func (builder *docRangeBuilder) add(position int64, body []byte, sequence int) error {
	entry, err := docRangeEntry(int(position), builder.position, body, sequence)
	if err != nil {
		return err
	}
	builder.entries = append(builder.entries, entry)
	builder.position += len(body) + 1
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
	return builder.result, nil
}

type verifiedDocRangePages struct {
	doc VerifiedSessionDoc
}

func (pages verifiedDocRangePages) Append(ctx context.Context, _ []byte) error {
	return ctx.Err()
}

func (pages verifiedDocRangePages) Read(ctx context.Context, position int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if position < 0 || position >= (len(pages.doc.chunks.events)+DocRangePageEvents-1)/DocRangePageEvents {
		return nil, ErrIntegrity
	}
	start := position * DocRangePageEvents
	entries := make([]DocRangeEntry, 0, DocRangePageEvents)
	for ordinal := start; ordinal < min(start+DocRangePageEvents, len(pages.doc.chunks.events)); ordinal++ {
		event := pages.doc.chunks.events[ordinal]
		body, err := pages.doc.EventStreamRange(ctx, event.span.offset, event.span.length)
		if err != nil {
			return nil, err
		}
		entry, err := docRangeEntry(ordinal, event.span.offset, body, event.seq)
		if err != nil {
			return nil, err
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
	builder := docRangeBuilder{ctx: ctx, buffer: verifiedDocRangePages{doc: doc}, result: VerifiedDocRangeIndex{index: DocRangeIndex{Version: 1, Hash: doc.Hash(), Manifest: manifest, Pages: []ContentHash{}}}}
	for position, event := range doc.chunks.events {
		body, err := doc.EventStreamRange(ctx, event.span.offset, event.span.length)
		if err != nil {
			return VerifiedDocRangeIndex{}, err
		}
		if err := builder.add(int64(position), body, event.seq); err != nil {
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
	if err := verifyConversationManifest(ctx, hash, manifest, load, check, builder.add); err != nil {
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
	Hash      ContentHash    `json:"hash"`
	Envelope  CIREnvelope    `json:"envelope"`
	Events    []DocViewEvent `json:"events"`
	Total     int            `json:"total"`
	Offset    int            `json:"offset"`
	Next      int            `json:"next"`
	Inherited int            `json:"inherited"`
}

type DocViewFragment struct {
	Index  int    `json:"event_index"`
	Offset int    `json:"byte_offset"`
	Next   int    `json:"next"`
	Total  int    `json:"total_bytes"`
	JSON   string `json:"json_fragment"`
}
