package domain

import (
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
	index  DocReadIndex
	bodies []json.RawMessage
}

func (doc VerifiedSessionDoc) PlanReadIndex() (DocReadPlan, error) {
	if !doc.Valid() {
		return DocReadPlan{}, ErrIntegrity
	}
	env, raw, err := splitCanonicalDocBytes(doc.Bytes())
	if err != nil {
		return DocReadPlan{}, err
	}
	out := DocReadIndex{Version: 1, Hash: doc.Hash(), Events: make([]DocEventIndex, 0, len(raw))}
	if err := json.Unmarshal(env, &out.Envelope); err != nil {
		return DocReadPlan{}, err
	}
	offset := 0
	for i, body := range raw {
		out.Events = append(out.Events, DocEventIndex{Index: i, Offset: offset, Length: len(body), Hash: HashContent(body)})
		offset += len(body) + 1 // canonical comma between events
	}
	return DocReadPlan{index: out, bodies: raw}, nil
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
	if p.index.Hash == "" {
		return DocReadIndex{}, ErrIntegrity
	}
	out := p.index
	out.Events = append([]DocEventIndex{}, p.index.Events...)
	for i, body := range p.bodies {
		if reusedSearch[out.Events[i].Hash] {
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
