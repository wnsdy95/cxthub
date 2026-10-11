package domain

import (
	"context"
	"encoding/hex"
	"strings"
	"time"
)

type PRHistoryCheckpoint struct {
	RepoID               ContentHash      `json:"repo_id"`
	GitOrigin            string           `json:"git_origin"`
	InstallationID       int64            `json:"installation_id"`
	ProviderRepositoryID int64            `json:"provider_repository_id"`
	PR                   PullRequestMerge `json:"pr"`
	Version              int64            `json:"version"`
	Cursor               string           `json:"cursor,omitempty"`
	EvidenceCursor       string           `json:"evidence_cursor,omitempty"`
	MatchCursor          string           `json:"match_cursor,omitempty"`
	PullRequestID        int64            `json:"pull_request_id,omitempty"`
	MergedAt             time.Time        `json:"merged_at,omitempty,omitzero"`
	Complete             bool             `json:"complete"`
	HistoryCheckedAt     time.Time        `json:"history_checked_at,omitempty,omitzero"`
	NextAttempt          time.Time        `json:"next_attempt"`
}

func (checkpoint PRHistoryCheckpoint) Validate() error {
	if ValidateContentHash(checkpoint.RepoID) != nil || checkpoint.GitOrigin == "" || checkpoint.InstallationID <= 0 || checkpoint.ProviderRepositoryID <= 0 || checkpoint.PR.Validate() != nil || checkpoint.Version < 1 || checkpoint.NextAttempt.IsZero() || len(checkpoint.Cursor) > 4096 || len(checkpoint.EvidenceCursor) > 128 {
		return ErrValidation
	}
	if checkpoint.PullRequestID < 0 || (checkpoint.PullRequestID == 0) != checkpoint.MergedAt.IsZero() {
		return ErrValidation
	}
	if checkpoint.Complete && (checkpoint.PullRequestID == 0 || checkpoint.HistoryCheckedAt.IsZero() || checkpoint.Cursor != "") {
		return ErrValidation
	}
	if checkpoint.MatchCursor != "" {
		parts := strings.Split(checkpoint.MatchCursor, ":")
		if len(parts) != 2 || len(parts[0]) != 64 || len(parts[1]) != 32 {
			return ErrValidation
		}
		if _, err := hex.DecodeString(parts[0] + parts[1]); err != nil {
			return ErrValidation
		}
	}
	return nil
}

type PRParticipation struct {
	ID                   string      `json:"id"`
	RepoID               ContentHash `json:"repo_id"`
	GitOrigin            string      `json:"git_origin"`
	ProviderRepositoryID int64       `json:"provider_repository_id"`
	PullRequestID        int64       `json:"pull_request_id"`
	Number               int         `json:"number"`
	MergeSHA             string      `json:"merge_sha"`
	PublicationID        string      `json:"publication_id"`
	Capture              ContentHash `json:"capture"`
	BranchID             string      `json:"branch_id"`
	OriginalHead         string      `json:"original_head"`
	ObservationID        string      `json:"observation_id"`
}

func (participation PRParticipation) WithID() PRParticipation {
	participation.ID = ""
	participation.ID = gitEvidenceKey(participation)
	return participation
}

func (participation PRParticipation) Validate() error {
	if participation.ID != participation.WithID().ID || ValidateContentHash(participation.RepoID) != nil || participation.GitOrigin == "" || participation.ProviderRepositoryID <= 0 || participation.PullRequestID <= 0 || participation.Number <= 0 || ValidateGitOID(participation.MergeSHA) != nil || ValidateGitOID(participation.OriginalHead) != nil || len(participation.MergeSHA) != len(participation.OriginalHead) || ValidateContentHash(participation.Capture) != nil || participation.BranchID == "" || len(participation.BranchID) > 128 || len(participation.PublicationID) != 32 || len(participation.ObservationID) != 64 || participation.PublicationID != strings.ToLower(participation.PublicationID) {
		return ErrValidation
	}
	if _, err := hex.DecodeString(participation.PublicationID); err != nil {
		return ErrValidation
	}
	if _, err := hex.DecodeString(participation.ObservationID); err != nil {
		return ErrValidation
	}
	return nil
}

func VerifyPRParticipation(checkpoint PRHistoryCheckpoint, observation GitRefObservation, publication HistoryEvent, history []HistoryEvent) (PRParticipation, error) {
	verifier, err := NewPRParticipationVerifier(checkpoint, history)
	if err != nil {
		return PRParticipation{}, err
	}
	return verifier.Verify(observation, publication)
}

type PRParticipationVerifier struct {
	checkpoint   PRHistoryCheckpoint
	publications map[string]HistoryEvent
	branches     map[string]map[string]bool
}

type PRParticipationHistoryIndex struct {
	repo        ContentHash
	history     []HistoryEvent
	candidates  map[string][]int
	attachments map[string][]int
}

type publicationObservationKey struct {
	repo, branch, name, local, worktree, git string
	target                                   ContentHash
}

func publicationKey(event HistoryEvent) publicationObservationKey {
	return publicationObservationKey{event.RepoID, event.BranchID, event.Branch, event.LocalBranch, event.WorktreeID, event.GitAfter, event.Target}
}

func NewPRParticipationVerifier(checkpoint PRHistoryCheckpoint, history []HistoryEvent) (*PRParticipationVerifier, error) {
	if checkpoint.Validate() != nil || checkpoint.PullRequestID == 0 {
		return nil, ErrIntegrity
	}
	index, err := NewPRParticipationHistoryIndex(context.Background(), checkpoint.RepoID, history)
	if err != nil {
		return nil, err
	}
	return index.Verifier(context.Background(), checkpoint)
}

func NewPRParticipationHistoryIndex(ctx context.Context, repo ContentHash, history []HistoryEvent) (*PRParticipationHistoryIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(repo) != nil {
		return nil, ErrValidation
	}
	index := &PRParticipationHistoryIndex{repo: repo, history: make([]HistoryEvent, 0, len(history)), candidates: map[string][]int{}, attachments: map[string][]int{}}
	for _, event := range history {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.RepoID != string(repo) || ValidateHistoryEvent(event) != nil {
			return nil, ErrIntegrity
		}
		if event.Kind == "pr-merge" {
			continue
		}
		if event.Creation != nil {
			creation := *event.Creation
			creation.Command = append([]string(nil), creation.Command...)
			event.Creation = &creation
		}
		position := len(index.history)
		index.history = append(index.history, event)
		if event.Branch != "" {
			index.candidates[event.Branch] = append(index.candidates[event.Branch], position)
		}
		if event.LocalBranch != "" && event.LocalBranch != event.Branch && event.WorktreeID != "" {
			index.candidates[event.LocalBranch] = append(index.candidates[event.LocalBranch], position)
		}
		if event.Kind == "attach" {
			index.attachments[event.BranchID] = append(index.attachments[event.BranchID], position)
		}
	}
	return index, nil
}

func (index *PRParticipationHistoryIndex) Verifier(ctx context.Context, checkpoint PRHistoryCheckpoint) (*PRParticipationVerifier, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if index == nil || checkpoint.Validate() != nil || checkpoint.PullRequestID == 0 || checkpoint.RepoID != index.repo {
		return nil, ErrIntegrity
	}
	proofs := make(map[publicationObservationKey][]HistoryEvent)
	attachments := make(map[string][]HistoryEvent)
	attachedBranches := make(map[string]bool)
	candidates := index.candidates[checkpoint.PR.HeadBranch]
	for _, position := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		event := index.history[position]
		if event.Kind != "publish" && !event.CreatedAt.After(checkpoint.MergedAt) {
			key := publicationKey(event)
			proofs[key] = append(proofs[key], event)
		}
		if event.Branch == checkpoint.PR.HeadBranch || attachedBranches[event.BranchID] {
			continue
		}
		attachedBranches[event.BranchID] = true
		for _, position := range index.attachments[event.BranchID] {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			attached := index.history[position]
			if !attached.CreatedAt.After(checkpoint.MergedAt) {
				attachments[event.BranchID] = append(attachments[event.BranchID], attached)
			}
		}
	}
	verifier := &PRParticipationVerifier{checkpoint: checkpoint, publications: make(map[string]HistoryEvent), branches: make(map[string]map[string]bool)}
	for _, observations := range proofs {
		for _, observation := range observations {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if observation.Target == "" || ValidateGitOID(observation.GitAfter) != nil {
				continue
			}
			candidate := observation
			candidate.Kind, candidate.Source = "publish", candidate.Target
			if candidate.Branch != checkpoint.PR.HeadBranch {
				matching := append([]HistoryEvent{observation}, attachments[observation.BranchID]...)
				if !MatchesPRSourcePublication(candidate, checkpoint.PR.HeadBranch, matching) {
					continue
				}
			}
			if verifier.branches[observation.GitAfter] == nil {
				verifier.branches[observation.GitAfter] = make(map[string]bool)
			}
			verifier.branches[observation.GitAfter][observation.BranchID] = true
		}
	}
	for _, position := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		publication := index.history[position]
		if publication.Kind != "publish" {
			continue
		}
		observations := proofs[publicationKey(publication)]
		if len(observations) == 0 {
			continue
		}
		if publication.Branch != checkpoint.PR.HeadBranch {
			matching := append([]HistoryEvent{}, observations...)
			matching = append(matching, attachments[publication.BranchID]...)
			if !MatchesPRSourcePublication(publication, checkpoint.PR.HeadBranch, matching) {
				continue
			}
		}
		verifier.publications[publication.ID] = publication
	}
	return verifier, nil
}

func (verifier *PRParticipationVerifier) Verify(observation GitRefObservation, publication HistoryEvent) (PRParticipation, error) {
	var empty PRParticipation
	checkpoint := verifier.checkpoint
	if observation.Validate() != nil || ValidateHistoryEvent(publication) != nil {
		return empty, ErrIntegrity
	}
	provider := observation.PullRequest
	if observation.RepoID != checkpoint.RepoID || observation.GitOrigin != checkpoint.GitOrigin || provider == nil || provider.RepositoryID != checkpoint.ProviderRepositoryID || provider.ID != checkpoint.PullRequestID || provider.Number != checkpoint.PR.Number || provider.BaseRef != checkpoint.PR.BaseBranch || observation.Ref != "refs/heads/"+checkpoint.PR.HeadBranch {
		return empty, ErrPRSourcePending
	}
	rewrite := observation.Source == "github-pr-reconciliation" && provider.Action == "force-pushed" && !provider.OccurredAt.After(checkpoint.MergedAt)
	closed := observation.Source == "github-pull-request" && provider.Action == "closed" && provider.MergeSHA == checkpoint.PR.MergeSHA && observation.After == checkpoint.PR.HeadSHA
	if !rewrite && !closed {
		return empty, ErrPRSourcePending
	}
	accepted, found := verifier.publications[publication.ID]
	if !found || accepted != publication || publication.Target == "" || (publication.GitAfter != observation.Before && publication.GitAfter != observation.After) {
		return empty, ErrPRSourcePending
	}
	if len(verifier.branches[publication.GitAfter]) != 1 {
		return empty, ErrConflict
	}
	participation := PRParticipation{RepoID: checkpoint.RepoID, GitOrigin: checkpoint.GitOrigin, ProviderRepositoryID: checkpoint.ProviderRepositoryID, PullRequestID: checkpoint.PullRequestID, Number: checkpoint.PR.Number, MergeSHA: checkpoint.PR.MergeSHA, PublicationID: publication.ID, Capture: publication.Target, BranchID: publication.BranchID, OriginalHead: publication.GitAfter, ObservationID: observation.ID}.WithID()
	return participation, participation.Validate()
}
