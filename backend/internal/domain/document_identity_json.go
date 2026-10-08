package domain

import "encoding/json"

// The enclosing object owns this per-decode presence bit. Keeping it out of
// DocumentIdentity avoids confusing repeated keys with a reused destination.
type singleDocumentIdentity struct {
	value DocumentIdentity
	seen  bool
}

func (field *singleDocumentIdentity) UnmarshalJSON(raw []byte) error {
	if field.seen {
		return ErrUnsupportedDocumentIdentity
	}
	field.seen = true
	return json.Unmarshal(raw, &field.value)
}

func (doc *SessionDoc) UnmarshalJSON(raw []byte) error {
	type wire SessionDoc
	next := wire(*doc)
	// An explicit outer field shadows the embedded alias's field, including
	// encoding/json's case-folded aliases. CIR is decoded only once.
	input := struct {
		*wire
		Identity singleDocumentIdentity `json:"identity"`
	}{&next, singleDocumentIdentity{value: next.Identity}}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	next.Identity = input.Identity.value
	*doc = SessionDoc(next)
	return nil
}

func (snapshot *Snapshot) UnmarshalJSON(raw []byte) error {
	type wire Snapshot
	next := wire(*snapshot)
	input := struct {
		*wire
		DocIdentity singleDocumentIdentity `json:"doc_identity"`
	}{&next, singleDocumentIdentity{value: next.DocIdentity}}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	next.DocIdentity = input.DocIdentity.value
	*snapshot = Snapshot(next)
	return nil
}
