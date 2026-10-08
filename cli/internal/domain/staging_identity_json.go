package domain

import "encoding/json"

// Identity belongs to the containing entry, so duplicate and case-folded
// aliases cannot erase a declaration before index/version validation sees it.
func (entry *StagedSession) UnmarshalJSON(raw []byte) error {
	type wire StagedSession
	next := wire(*entry)
	input := struct {
		*wire
		DocIdentity singleDocumentIdentity `json:"doc_identity"`
	}{&next, singleDocumentIdentity{value: next.DocIdentity}}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	next.DocIdentity = input.DocIdentity.value
	*entry = StagedSession(next)
	return nil
}
