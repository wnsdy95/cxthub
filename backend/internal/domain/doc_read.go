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
	cb, err := CanonicalBytes(doc.CIR)
	if err != nil {
		return DocReadIndex{}, err
	}
	if HashContent(cb) != doc.Hash {
		return DocReadIndex{}, ErrIntegrity
	}
	_, raw, err := splitCanonicalDocBytes(cb)
	if err != nil {
		return DocReadIndex{}, err
	}
	out := DocReadIndex{Version: 1, Hash: doc.Hash, Envelope: doc.CIR.Envelope, Events: make([]DocEventIndex, 0, len(raw))}
	offset := 0
	for i, body := range raw {
		ev := doc.CIR.Events[i]
		out.Events = append(out.Events, DocEventIndex{Index: i, Offset: offset, Length: len(body), Hash: HashContent(body), Seq: ev.Seq, Role: string(ev.Role), Text: SearchableEventText(ev)})
		offset += len(body) + 1 // canonical comma between events
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
