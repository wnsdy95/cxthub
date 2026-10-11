package domain

import (
	"encoding/hex"
	"strings"
	"time"
)

type GitAssociationStatusRequest struct {
	Kind   string
	Cursor string
	State  string
	Limit  int
}

func (request GitAssociationStatusRequest) Validate() error {
	if request.Limit < 1 || request.Limit > 100 || (request.State != "pending" && request.State != "all") {
		return ErrValidation
	}
	want := 64
	switch request.Kind {
	case "publication":
		want = 32
	case "integration", "memory":
	default:
		return ErrValidation
	}
	if request.Cursor != "" {
		if len(request.Cursor) != want || strings.ToLower(request.Cursor) != request.Cursor {
			return ErrValidation
		}
		if _, err := hex.DecodeString(request.Cursor); err != nil {
			return ErrValidation
		}
	}
	return nil
}

type GitAssociationStatus struct {
	ID                  string      `json:"id"`
	Kind                string      `json:"kind"`
	State               string      `json:"state"`
	Reason              string      `json:"reason,omitempty"`
	SourceBranch        string      `json:"source_branch"`
	SourceBranchID      string      `json:"source_branch_id"`
	DestinationBranch   string      `json:"destination_branch,omitempty"`
	DestinationBranchID string      `json:"destination_branch_id,omitempty"`
	Capture             ContentHash `json:"capture"`
	OriginalHead        string      `json:"original_head"`
	PublicationID       string      `json:"publication_id"`
	AdmissionID         string      `json:"admission_id,omitempty"`
	ContributionID      string      `json:"contribution_id,omitempty"`
	Attempts            int         `json:"attempts"`
	NextAttempt         *time.Time  `json:"next_attempt,omitempty"`
}

type GitAssociationDiscoveryStatus struct {
	State   string `json:"state"`
	Version int64  `json:"version,string"`
}

type GitAssociationHeadStatus struct {
	State  string `json:"state"`
	Page   int    `json:"page"`
	Reason string `json:"reason,omitempty"`
}

type GitAssociationStatusPage struct {
	RepoID               ContentHash                   `json:"repo_id"`
	Kind                 string                        `json:"kind"`
	Filter               string                        `json:"filter"`
	Items                []GitAssociationStatus        `json:"items"`
	NextCursor           string                        `json:"next_cursor,omitempty"`
	Discovery            GitAssociationDiscoveryStatus `json:"discovery"`
	Reconciliation       GitAssociationHeadStatus      `json:"reconciliation"`
	CurrentApplicability string                        `json:"current_applicability"`
}
