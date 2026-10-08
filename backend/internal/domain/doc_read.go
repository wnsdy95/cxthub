package domain

import (
	"context"
	"encoding/json"
	"strings"
)

// DocReadIndex is a rebuildable projection of verified canonical bytes. Offsets
// address the v2 event stream, not the compressed blob or the wire JSON wrapper.
type DocReadIndex struct {
	Version  int             `json:"version"`
	Hash     ContentHash     `json:"hash"`
	Envelope CIREnvelope     `json:"envelope"`
	Events   []DocEventIndex `json:"events"`
}

type DocEventIndex struct {
	Index  int         `json:"index"`
	Offset int         `json:"offset"`
	Length int         `json:"length"`
	Hash   ContentHash `json:"hash"`
	Seq    int         `json:"seq"`
	Role   string      `json:"role"`
	Text   string      `json:"text,omitempty"`
}

type DocEventPage struct {
	Hash      ContentHash `json:"hash"`
	Envelope  CIREnvelope `json:"envelope"`
	Events    []CIREvent  `json:"events"`
	Total     int         `json:"total"`
	Offset    int         `json:"offset"`
	Next      int         `json:"next"` // -1 means exhausted
	Inherited int         `json:"inherited"`
}

func SearchableEventText(e CIREvent) string {
	if e.Kind == EventReasoning {
		return e.RedactedSummary
	}
	if e.Kind != EventMessage && e.Kind != EventTurn {
		return ""
	}
	parts := []string{}
	for _, b := range e.Blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func BuildDocReadIndex(doc SessionDoc) (DocReadIndex, error) {
	verified, err := VerifySessionDoc(doc)
	if err != nil {
		return DocReadIndex{}, err
	}
	return verified.ReadIndex()
}

// ReadIndex derives offsets and searchable metadata from the same canonical
// event bytes, including when the original input was not in sequence order.
func (doc VerifiedSessionDoc) ReadIndex() (DocReadIndex, error) {
	plan, err := doc.PlanReadIndex()
	if err != nil {
		return DocReadIndex{}, err
	}
	return plan.Build(nil)
}

// DocReadPlan contains only event bytes from a verified document. Adapters may
// reuse an existing search projection keyed by its exact canonical event hash;
// document offsets, sequence and role are always derived from this document.
// The private bodies prevent a caller from changing the bytes behind the hash.
type DocReadPlan struct {
	index          DocReadIndex
	stream         canonicalSegments
	bodies         []canonicalSpan
	sequencesKnown bool // Exact sequences retained by a chunk-backed proof.
}

func (doc VerifiedSessionDoc) PlanReadIndex() (DocReadPlan, error) {
	return doc.PlanReadIndexContext(context.Background())
}

// PlanReadIndexContext cooperatively cancels projection planning. A v2 chunk
// proof already owns exact event spans/hashes, so it needs no second byte scan.
// Legacy values scan their immutable string without a cumulative JSON copy.
func (doc VerifiedSessionDoc) PlanReadIndexContext(ctx context.Context) (DocReadPlan, error) {
	if err := ctx.Err(); err != nil {
		return DocReadPlan{}, err
	}
	if !doc.Valid() {
		return DocReadPlan{}, ErrIntegrity
	}
	env, stream, err := doc.segmented()
	if err != nil {
		return DocReadPlan{}, err
	}
	out := DocReadIndex{Version: 1, Hash: doc.Hash(), Events: []DocEventIndex{}}
	if err := json.Unmarshal([]byte(env), &out.Envelope); err != nil {
		return DocReadPlan{}, err
	}
	var spans []canonicalSpan
	if doc.chunks != nil {
		out.Events = make([]DocEventIndex, 0, len(doc.chunks.events))
		spans = make([]canonicalSpan, 0, len(doc.chunks.events))
		for i, event := range doc.chunks.events {
			if err := ctx.Err(); err != nil {
				return DocReadPlan{}, err
			}
			out.Events = append(out.Events, DocEventIndex{Index: i, Offset: event.span.offset, Length: event.span.length, Hash: event.hash, Seq: event.seq})
			spans = append(spans, event.span)
		}
		if err := ctx.Err(); err != nil {
			return DocReadPlan{}, err
		}
		return DocReadPlan{index: out, stream: stream, bodies: spans, sequencesKnown: true}, nil
	}
	hasher := newCanonicalSpanHasher()
	err = stream.visit(ctx, func(span canonicalSpan) error {
		h, err := hasher.sum(ctx, stream, span)
		if err != nil {
			return err
		}
		out.Events = append(out.Events, DocEventIndex{Index: len(spans), Offset: span.offset, Length: span.length, Hash: h})
		spans = append(spans, span)
		return nil
	})
	if err != nil {
		return DocReadPlan{}, err
	}
	return DocReadPlan{index: out, stream: stream, bodies: spans}, nil
}

func (p DocReadPlan) EventHashes() []ContentHash {
	out := make([]ContentHash, len(p.index.Events))
	for i, ev := range p.index.Events {
		out[i] = ev.Hash
	}
	return out
}

// Build omits text only for event hashes already present in the adapter's
// current-version search index. That adapter must retain those rows through its
// publication transaction. This is not a persisted validation or ownership proof.
func (p DocReadPlan) Build(reusedSearch map[ContentHash]bool) (DocReadIndex, error) {
	return p.BuildContext(context.Background(), reusedSearch)
}

// BuildContext derives the full search projection with cooperative cancellation.
func (p DocReadPlan) BuildContext(ctx context.Context, reusedSearch map[ContentHash]bool) (DocReadIndex, error) {
	return p.build(ctx, reusedSearch, false)
}

// BuildMetadataContext derives locations, sequence and exact role strings from
// verified bytes without decoding event payloads or retaining searchable text.
// It is for role-aware readers, not for publishing or querying a search index.
func (p DocReadPlan) BuildMetadataContext(ctx context.Context) (DocReadIndex, error) {
	return p.build(ctx, nil, true)
}

// BuildRangesContext returns owned locations, hashes and exact sequences, with
// Role and Text empty. Chunk-backed proofs need no event-byte scan. Legacy
// plans without retained sequences use the metadata decoder rather than treating
// their zero-valued planning fields as verified event sequences.
func (p DocReadPlan) BuildRangesContext(ctx context.Context) (DocReadIndex, error) {
	if err := ctx.Err(); err != nil {
		return DocReadIndex{}, err
	}
	if p.index.Hash == "" {
		return DocReadIndex{}, ErrIntegrity
	}
	var out DocReadIndex
	if p.sequencesKnown {
		out = p.index
		out.Envelope = p.Envelope()
		out.Events = append([]DocEventIndex{}, p.index.Events...)
	} else {
		var err error
		out, err = p.BuildMetadataContext(ctx)
		if err != nil {
			return DocReadIndex{}, err
		}
	}
	for i := range out.Events {
		if err := ctx.Err(); err != nil {
			return DocReadIndex{}, err
		}
		out.Events[i].Role, out.Events[i].Text = "", ""
	}
	if err := ctx.Err(); err != nil {
		return DocReadIndex{}, err
	}
	return out, nil
}

func (p DocReadPlan) build(ctx context.Context, reusedSearch map[ContentHash]bool, metadataOnly bool) (DocReadIndex, error) {
	if err := ctx.Err(); err != nil {
		return DocReadIndex{}, err
	}
	if p.index.Hash == "" {
		return DocReadIndex{}, ErrIntegrity
	}
	out := p.index
	out.Envelope = p.Envelope()
	out.Events = append([]DocEventIndex{}, p.index.Events...)
	var body []byte
	for i, span := range p.bodies {
		if err := ctx.Err(); err != nil {
			return DocReadIndex{}, err
		}
		body = p.stream.appendRange(body[:0], span)
		if err := ctx.Err(); err != nil {
			return DocReadIndex{}, err
		}
		if metadataOnly || reusedSearch[out.Events[i].Hash] {
			var metadata struct {
				Seq  int    `json:"seq"`
				Role string `json:"role"`
			}
			if err := json.Unmarshal(body, &metadata); err != nil {
				return DocReadIndex{}, err
			}
			out.Events[i].Seq, out.Events[i].Role = metadata.Seq, metadata.Role
			continue
		}
		var ev CIREvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return DocReadIndex{}, err
		}
		out.Events[i].Seq, out.Events[i].Role, out.Events[i].Text = ev.Seq, string(ev.Role), SearchableEventText(ev)
	}
	if err := ctx.Err(); err != nil {
		return DocReadIndex{}, err
	}
	return out, nil
}

func DecodeIndexedEvent(raw []byte, item DocEventIndex) (CIREvent, error) {
	var ev CIREvent
	if len(raw) != item.Length || HashContent(raw) != item.Hash {
		return ev, ErrIntegrity
	}
	err := json.Unmarshal(raw, &ev)
	return ev, err
}

// DocEventFragment preserves canonical JSON without loading an oversized event.
type DocEventFragment struct {
	Index    int    `json:"event_index"`
	Offset   int    `json:"byte_offset"`
	Complete bool   `json:"event_complete"`
	JSON     string `json:"json_fragment"`
}
type DocFragmentPage struct {
	Fragments                    []DocEventFragment
	Total, NextIndex, NextOffset int
}
