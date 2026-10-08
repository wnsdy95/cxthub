package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

const MaxDocJobManifestBytes = 256 << 10
const MaxDocJobChunks = 2048
const MaxFinalizedDocBytes = 512 << 20
const MaxPendingDocJobs = 32

// DocFinalizationJob records delivery, not a snapshot or a ref movement. The
// manifest is part of its identity so a bad upload cannot poison another upload
// claiming the same document hash. Version fences expired workers.
type DocFinalizationJob struct {
	DocIdentity  DocumentIdentity `json:"doc_identity,omitempty"`
	RootManifest json.RawMessage  `json:"root_manifest,omitempty"`
	ID           string           `json:"id"`
	RepoID       ContentHash      `json:"repo_id"`
	DocHash      ContentHash      `json:"doc_hash"`
	Manifest     DocChunkManifest `json:"manifest"`
	State        string           `json:"state"`
	Reason       string           `json:"reason,omitempty"`
	Attempts     int              `json:"attempts"`
	Version      int64            `json:"version"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	NextAttempt  time.Time        `json:"next_attempt"`
	LeaseUntil   time.Time        `json:"lease_until"`
}

func NewDocFinalizationJob(repo, hash ContentHash, manifest DocChunkManifest, now time.Time) (DocFinalizationJob, error) {
	if manifest.Format == "" {
		manifest.Format = ChunkFormatV1
	}
	j := DocFinalizationJob{RepoID: repo, DocHash: hash, Manifest: manifest, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	j.ID = j.identity()
	return j, j.Validate()
}
func (j DocFinalizationJob) identity() string {
	if j.DocIdentity != DocumentIdentityLegacy {
		id, _ := RootDocFinalizationID(j.RepoID, DocumentRepresentation{Hash: j.DocHash, Identity: j.DocIdentity, RootManifest: j.RootManifest})
		return id
	}

	b, _ := json.Marshal(struct {
		Repo     ContentHash
		Hash     ContentHash
		Manifest DocChunkManifest
	}{j.RepoID, j.DocHash, j.Manifest})
	return string(HashContent(b))
}
func (j DocFinalizationJob) Validate() error {
	if err := ValidateContentHash(j.RepoID); err != nil {
		return err
	}
	if err := ValidateContentHash(j.DocHash); err != nil {
		return err
	}
	if _, err := j.Representation(); err != nil {
		return err
	}

	if j.ID != j.identity() || j.CreatedAt.IsZero() || j.Version < 0 || j.Attempts < 0 {
		return ErrValidation
	}
	switch j.State {
	case "waiting", "running", "retrying", "completed", "rejected":
	default:
		return ErrValidation
	}
	return nil
}
func (j DocFinalizationJob) Pending() bool {
	return j.State == "waiting" || j.State == "running" || j.State == "retrying"
}
func (j DocFinalizationJob) Due(now time.Time) bool {
	return j.Pending() && !j.NextAttempt.After(now) && (j.State != "running" || !j.LeaseUntil.After(now))
}
func (j DocFinalizationJob) Claim(now time.Time, lease time.Duration) DocFinalizationJob {
	j.State = "running"
	j.Attempts++
	j.Version++
	j.UpdatedAt = now
	j.LeaseUntil = now.Add(lease)
	return j
}
func (j DocFinalizationJob) Fences(claim DocFinalizationJob, now time.Time) bool {
	if j.DocIdentity != DocumentIdentityLegacy || claim.DocIdentity != DocumentIdentityLegacy || j.RootManifest != nil || claim.RootManifest != nil {
		if j.Validate() != nil || claim.Validate() != nil || j.DocumentRef() != claim.DocumentRef() {
			return false
		}
	}

	return j.ID == claim.ID && j.RepoID == claim.RepoID && j.Version == claim.Version && j.State == "running" && j.LeaseUntil.After(now)
}

// NewDocFinalizationJobForRepresentation owns descriptor metadata only. Root
// publication still requires current byte verification and storage gates.
func NewDocFinalizationJobForRepresentation(repo ContentHash, doc DocumentRepresentation, now time.Time) (DocFinalizationJob, error) {
	if err := doc.DocumentRef().Validate(); err != nil {
		return DocFinalizationJob{}, err
	}
	if doc.Identity == DocumentIdentityLegacy {
		if doc.RootManifest != nil {
			return DocFinalizationJob{}, ErrValidation
		}
		return NewDocFinalizationJob(repo, doc.Hash, DocChunkManifest{Format: doc.Format, Envelope: bytes.Clone(doc.Envelope), Chunks: append([]ContentHash(nil), doc.Chunks...)}, now)
	}
	if _, err := doc.ConversationManifest(); err != nil {
		return DocFinalizationJob{}, err
	}
	j := DocFinalizationJob{RepoID: repo, DocHash: doc.Hash, DocIdentity: doc.Identity, RootManifest: bytes.Clone(doc.RootManifest), State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	j.ID = j.identity()
	if err := j.Validate(); err != nil {
		return DocFinalizationJob{}, err
	}
	return j, nil
}
func (j DocFinalizationJob) DocumentRef() DocumentRef {
	return DocumentRef{Hash: j.DocHash, Identity: j.DocIdentity}
}

// Representation validates the exclusive metadata variant and returns owned
// bytes. It deliberately does not imply this job is valid or completed.
func (j DocFinalizationJob) Representation() (DocumentRepresentation, error) {
	if err := j.DocumentRef().Validate(); err != nil {
		return DocumentRepresentation{}, err
	}
	if j.DocIdentity == DocumentIdentityRootV1 {
		if j.Manifest.Format != "" || j.Manifest.Envelope != nil || j.Manifest.Chunks != nil {
			return DocumentRepresentation{}, ErrValidation
		}
		doc := DocumentRepresentation{Hash: j.DocHash, Identity: j.DocIdentity, RootManifest: j.RootManifest}
		if _, err := doc.ConversationManifest(); err != nil {
			return DocumentRepresentation{}, err
		}
		doc.RootManifest = bytes.Clone(doc.RootManifest)
		return doc, nil
	}
	if j.RootManifest != nil {
		return DocumentRepresentation{}, ErrValidation
	}
	if err := validateLegacyJobManifest(j.Manifest); err != nil {
		return DocumentRepresentation{}, err
	}
	return DocumentRepresentation{Hash: j.DocHash, Format: j.Manifest.Format, Envelope: bytes.Clone(j.Manifest.Envelope), Chunks: append([]ContentHash(nil), j.Manifest.Chunks...)}, nil
}
func validateLegacyJobManifest(manifest DocChunkManifest) error {
	if !SupportedChunkFormat(manifest.Format) || manifest.Format == "" || len(manifest.Chunks) == 0 || len(manifest.Chunks) > MaxDocJobChunks {
		return fmt.Errorf("%w: invalid document manifest", ErrValidation)
	}
	if len(manifest.Envelope) == 0 || !json.Valid(manifest.Envelope) {
		return ErrValidation
	}
	b, err := json.Marshal(manifest)
	if err != nil || len(b) > MaxDocJobManifestBytes {
		return fmt.Errorf("%w: document manifest too large", ErrValidation)
	}
	for _, h := range manifest.Chunks {
		if err := ValidateContentHash(h); err != nil {
			return err
		}
	}
	return nil
}

func (j *DocFinalizationJob) UnmarshalJSON(raw []byte) error {
	type wire DocFinalizationJob
	var next wire
	input := struct {
		*wire
		Identity singleDocumentIdentity `json:"doc_identity"`
	}{wire: &next}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	next.DocIdentity = input.Identity.value
	*j = DocFinalizationJob(next)
	return nil
}

// Root jobs omit the legacy manifest completely. Emitting its zero struct would
// turn nil RawMessage into the non-nil JSON value "null" on a durable round trip.
// Legacy uses the original struct encoding and field order unchanged.
func (j DocFinalizationJob) MarshalJSON() ([]byte, error) {
	type wire DocFinalizationJob
	if j.DocIdentity == DocumentIdentityLegacy {
		return json.Marshal(wire(j))
	}
	if _, err := j.Representation(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		*wire
		Manifest *DocChunkManifest `json:"manifest,omitempty"`
	}{wire: (*wire)(&j)})
}
