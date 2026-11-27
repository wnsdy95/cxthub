package domain

import (
	"bytes"
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

// DocReadPlan retains verified event coordinates and either immutable bytes or
// a spooled source whose event length and hash are checked on every read.
// Adapters may reuse a search projection keyed by the exact canonical event hash.
type DocReadPlan struct {
	index  DocReadIndex
	stream canonicalSegments
	source VerifiedDocStream
	bodies []canonicalSpan
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
		return DocReadPlan{index: out, stream: stream, bodies: spans}, nil
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

func (p DocReadPlan) build(ctx context.Context, reusedSearch map[ContentHash]bool, metadataOnly bool) (DocReadIndex, error) {
	out := p.index
	out.Envelope = p.Envelope()
	out.Events = make([]DocEventIndex, 0, len(p.index.Events))
	err := p.visit(ctx, reusedSearch, metadataOnly, func(event DocEventIndex) error {
		out.Events = append(out.Events, event)
		return nil
	})
	if err != nil {
		return DocReadIndex{}, err
	}
	return out, nil
}

func (p DocReadPlan) visit(ctx context.Context, reusedSearch map[ContentHash]bool, metadataOnly bool, visit func(DocEventIndex) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.index.Hash == "" || visit == nil || len(p.bodies) != len(p.index.Events) {
		return ErrIntegrity
	}
	var body []byte
	for i, span := range p.bodies {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := p.index.Events[i]
		entry.Text = ""
		if p.source != nil {
			current, err := p.source.Read(ctx, span.offset, span.length)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(current) != span.length {
				return ErrIntegrity
			}
			body = bytes.Clone(current)
			hash, err := HashContentContext(ctx, body)
			if err != nil {
				return err
			}
			if hash != entry.Hash {
				return ErrIntegrity
			}
		} else {
			body = p.stream.appendRange(body[:0], span)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if metadataOnly || reusedSearch[entry.Hash] {
			var metadata struct {
				Seq  int    `json:"seq"`
				Role string `json:"role"`
			}
			if err := json.Unmarshal(body, &metadata); err != nil {
				return err
			}
			entry.Seq, entry.Role = metadata.Seq, metadata.Role
		} else {
			var ev struct {
				Kind            EventKind      `json:"kind"`
				Seq             int            `json:"seq"`
				Role            Role           `json:"role"`
				Blocks          []ContentBlock `json:"blocks"`
				RedactedSummary string         `json:"redacted_summary"`
			}
			if err := json.Unmarshal(body, &ev); err != nil {
				return err
			}
			entry.Seq, entry.Role, entry.Text = ev.Seq, string(ev.Role), SearchableEventText(CIREvent{
				Kind: ev.Kind, Blocks: ev.Blocks, RedactedSummary: ev.RedactedSummary,
			})
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(entry); err != nil {
			return err
		}
	}
	return ctx.Err()
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
