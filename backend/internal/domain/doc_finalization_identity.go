package domain

import (
	"bytes"
	"encoding/json"
	"time"
)

// RootDocFinalizationID binds an upload's root metadata and repository. It is
// not proof of current bodies, ownership, authorization or completed delivery.
// Legacy jobs retain their preexisting, separate identity algorithm.
func RootDocFinalizationID(repo ContentHash, doc DocumentRepresentation) (string, error) {
	if err := ValidateContentHash(repo); err != nil {
		return "", err
	}
	if _, err := doc.ConversationManifest(); err != nil {
		return "", err
	}
	raw, err := canonicalJSON(struct {
		RepoID   ContentHash            `json:"repo_id"`
		Document DocumentRepresentation `json:"document"`
	}{repo, doc})
	if err != nil {
		return "", err
	}
	return string(HashContent(append([]byte("cxt-doc-finalization-root-v1\x00"), raw...))), nil
}

// DocFinalizationStatus contains identity metadata, never a body proof.
// The omitted identity is the only legacy spelling.
type DocFinalizationStatus struct {
	ID          string           `json:"id"`
	DocHash     ContentHash      `json:"doc_hash"`
	DocIdentity DocumentIdentity `json:"doc_identity,omitempty"`
	State       string           `json:"state"`
	Reason      string           `json:"reason,omitempty"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

func (s *DocFinalizationStatus) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrIntegrity
	}
	type wire DocFinalizationStatus
	var next wire
	input := struct {
		*wire
		Identity singleDocumentIdentity `json:"doc_identity"`
	}{wire: &next}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	next.DocIdentity = input.Identity.value
	*s = DocFinalizationStatus(next)
	return nil
}
