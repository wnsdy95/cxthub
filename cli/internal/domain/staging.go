package domain

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const StagingVersion = 1

// Version 2 records one final memory selection for the selected target. Index
// and stash formats remain version 1; interrupted version 1 commits retain
// their original observations when replayed.
const StagingCommitVersion = 2

var ErrStagingVersion = errors.New("unsupported staging version")
var ErrEmptyIndex = errors.New("no frozen sessions staged; run cxt add first")

// StagedSession freezes one source generation. Re-adding a growing generation
// replaces its entry; replacement/rewritten transcripts create another entry.
// No field is a live provider selector or a provider file path.
type StagedSession struct {
	Key           ContentHash  `json:"key"`
	Provider      ProviderKind `json:"provider"`
	SessionID     string       `json:"session_id"`
	SourceID      ContentHash  `json:"source_id"`
	Generation    ContentHash  `json:"generation"`
	DocHash       ContentHash  `json:"doc_hash"`
	Events        int          `json:"events"`
	StartEvent    int          `json:"start_event"`
	CapturedBytes int64        `json:"captured_bytes"`
	CodeCommit    string       `json:"code_commit"`
	Branch        string       `json:"branch"`
	BranchID      string       `json:"branch_id"`
	Base          ContentHash  `json:"base,omitempty"`
	CapturedAt    time.Time    `json:"captured_at"`
}

func StagedSessionKey(provider ProviderKind, session string, source, generation ContentHash) ContentHash {
	raw, _ := json.Marshal([]string{string(provider), session, string(source), string(generation)})
	return HashContent(raw)
}

// StagingIndex is a worktree-local index. Sequence prevents ABA, including an
// empty index recreated after a commit. Revision authenticates the full manifest.
type StagingIndex struct {
	Version    int             `json:"version"`
	RepoID     string          `json:"repo_id"`
	WorktreeID string          `json:"worktree_id"`
	Sequence   uint64          `json:"sequence"`
	Revision   ContentHash     `json:"revision"`
	Entries    []StagedSession `json:"entries"`
}

func (i StagingIndex) WithRevision() StagingIndex {
	i.Entries = append([]StagedSession{}, i.Entries...)
	sort.Slice(i.Entries, func(a, b int) bool { return i.Entries[a].Key < i.Entries[b].Key })
	i.Revision = ""
	raw, _ := json.Marshal(i)
	i.Revision = HashContent(raw)
	return i
}

func ValidateStagingIndex(i StagingIndex) error {
	if i.Version != StagingVersion {
		return ErrStagingVersion
	}
	if ValidateContentHash(ContentHash(i.RepoID)) != nil || len(i.WorktreeID) != 32 {
		return ErrHashMismatch
	}
	if _, err := hex.DecodeString(i.WorktreeID); err != nil {
		return ErrHashMismatch
	}
	if i.WithRevision().Revision != i.Revision {
		return ErrHashMismatch
	}
	seen := map[ContentHash]bool{}
	for _, e := range i.Entries {
		if err := ValidateStagedSession(e); err != nil {
			return err
		}
		if seen[e.Key] {
			return fmt.Errorf("duplicate staged source: %w", ErrHashMismatch)
		}
		seen[e.Key] = true
	}
	return nil
}

func ValidateStagedSession(e StagedSession) error {
	if e.Provider != ProviderClaude && e.Provider != ProviderCodex {
		return ErrUnsupportedProvider
	}
	if e.SessionID == "" || len(e.SessionID) > 256 || e.Events < 0 || e.StartEvent < 0 || e.StartEvent > e.Events || e.CapturedBytes < 0 || e.CapturedAt.IsZero() || e.BranchID == "" {
		return ErrHashMismatch
	}
	for _, h := range []ContentHash{e.Key, e.SourceID, e.Generation, e.DocHash} {
		if err := ValidateContentHash(h); err != nil {
			return err
		}
	}
	if err := ValidateOptionalContentHash(e.Base); err != nil {
		return err
	}
	if e.Key != StagedSessionKey(e.Provider, e.SessionID, e.SourceID, e.Generation) {
		return ErrHashMismatch
	}
	if err := ValidateStagingCode(e.CodeCommit); err != nil {
		return err
	}
	if e.Branch != "" {
		return ValidateBranchName(e.Branch)
	}
	return nil
}

func ValidateStagingCode(sha string) error {
	if (len(sha) != 40 && len(sha) != 64) || sha != strings.ToLower(sha) || strings.Trim(sha, "0") == "" {
		return fmt.Errorf("staging requires an exact current Git commit")
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return fmt.Errorf("invalid staged Git commit")
	}
	return nil
}

// ConsumeStagedEntries removes only entries unchanged since this operation was
// prepared. A concurrent re-add or a different source generation survives.
func ConsumeStagedEntries(current StagingIndex, frozen []StagedSession) StagingIndex {
	consumed := map[ContentHash]StagedSession{}
	for _, e := range frozen {
		consumed[e.Key] = e
	}
	out := current
	out.Entries = []StagedSession{}
	for _, e := range current.Entries {
		if prior, ok := consumed[e.Key]; !ok || prior != e {
			out.Entries = append(out.Entries, e)
		}
	}
	if len(out.Entries) != len(current.Entries) {
		out.Sequence++
	}
	return out.WithRevision()
}

// StagingCommit is both the durable local operation and pending upload receipt.
// Publications use the existing immutable history/outbox transport. LocalFinalized
// is not a claim that a server accepted the operation.
type StagingCommit struct {
	Version          int             `json:"version"`
	ID               string          `json:"id"`
	Index            StagingIndex    `json:"index"`
	ExpectedPosition WorkingPosition `json:"expected_position"`
	ExpectedRef      Ref             `json:"expected_ref"`
	Position         WorkingPosition `json:"position"`
	Ref              Ref             `json:"ref"`
	Advance          *HistoryEvent   `json:"advance,omitempty"`
	Publications     []HistoryEvent  `json:"publications"`
	CreatedAt        time.Time       `json:"created_at"`
	LocalFinalized   bool            `json:"local_finalized"`
}

// StagingObservations are source/code facts derived from the durable commit
// manifest, not publication acknowledgements. Version 2 uses the final selection
// as the selected target's witness: a contribution's empty memory attachment must
// not also become a conflicting memory selection. Raw publications are retained.
// IDs stay deterministic across crash recovery and disjoint from publish IDs.
func StagingObservations(op StagingCommit) []HistoryEvent {
	var final HistoryEvent
	if op.Position.Selection != nil {
		final = *op.Position.Selection
		final.Kind = "position"
	}
	publications := make([]HistoryEvent, 0, len(op.Publications)+1)
	for _, event := range op.Publications {
		if op.Version == StagingCommitVersion && publicationProof(event, final) {
			continue
		}
		publications = append(publications, event)
	}
	if op.Position.Selection != nil {
		publications = append(publications, *op.Position.Selection)
	}
	observations := make([]HistoryEvent, 0, len(publications))
	for _, event := range publications {
		event.ID = string(HashContent([]byte("staging-observation/v1/" + event.ID)))[7:39]
		event.Kind = "position"
		observations = append(observations, event)
	}
	return observations
}

// StagingStash preserves the complete frozen index and its selected base. Pop
// merges unchanged entries and never overwrites another staged generation.
type StagingStash struct {
	Version   int             `json:"version"`
	ID        string          `json:"id"`
	Index     StagingIndex    `json:"index"`
	Position  WorkingPosition `json:"position"`
	CreatedAt time.Time       `json:"created_at"`
	Applied   bool            `json:"applied"`
}
