package domain

import (
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
	ID          string           `json:"id"`
	RepoID      ContentHash      `json:"repo_id"`
	DocHash     ContentHash      `json:"doc_hash"`
	Manifest    DocChunkManifest `json:"manifest"`
	State       string           `json:"state"`
	Reason      string           `json:"reason,omitempty"`
	Attempts    int              `json:"attempts"`
	Version     int64            `json:"version"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	NextAttempt time.Time        `json:"next_attempt"`
	LeaseUntil  time.Time        `json:"lease_until"`
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
	if !SupportedChunkFormat(j.Manifest.Format) || j.Manifest.Format == "" || len(j.Manifest.Chunks) == 0 || len(j.Manifest.Chunks) > MaxDocJobChunks {
		return fmt.Errorf("%w: invalid document manifest", ErrValidation)
	}
	if len(j.Manifest.Envelope) == 0 || !json.Valid(j.Manifest.Envelope) {
		return ErrValidation
	}
	b, err := json.Marshal(j.Manifest)
	if err != nil || len(b) > MaxDocJobManifestBytes {
		return fmt.Errorf("%w: document manifest too large", ErrValidation)
	}
	for _, h := range j.Manifest.Chunks {
		if err := ValidateContentHash(h); err != nil {
			return err
		}
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
	return j.ID == claim.ID && j.RepoID == claim.RepoID && j.Version == claim.Version && j.State == "running" && j.LeaseUntil.After(now)
}
