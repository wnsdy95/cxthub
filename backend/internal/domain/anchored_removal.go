package domain

import (
	"fmt"
	"strings"
	"time"
)

// AnchoredSessionRemoval says that a session was removed from the server.
// While it stands no report is taken for the session, so that a CLI that
// still holds the tool's records does not bring back what was removed.
//
// Uploader is the account the session belonged to and RemovedBy the account
// that removed it: the uploader, or someone who manages the repository.
type AnchoredSessionRemoval struct {
	RepoID    ContentHash  `json:"repo_id"`
	Provider  ProviderKind `json:"provider"`
	SessionID string       `json:"session_id"`
	Uploader  string       `json:"uploader"`
	RemovedBy string       `json:"removed_by"`
	RemovedAt time.Time    `json:"removed_at"`
}

// Validate checks what a stored removal must hold.
func (r AnchoredSessionRemoval) Validate() error {
	if !ValidAnchoredSessionKey(r.Provider, r.SessionID) {
		return fmt.Errorf("%w: removal: session key", ErrValidation)
	}
	for _, account := range []string{r.Uploader, r.RemovedBy} {
		if account == "" || strings.TrimSpace(account) != account || len(account) > 512 {
			return fmt.Errorf("%w: removal: account", ErrValidation)
		}
	}
	if r.RemovedAt.IsZero() {
		return fmt.Errorf("%w: removal: time", ErrValidation)
	}
	return nil
}

// AnchoredSessionRemovalView is a removal as its session's uploader reads it.
// No account is named: ByYou says whether the reader removed it or someone
// who manages the repository did.
type AnchoredSessionRemovalView struct {
	Provider  ProviderKind `json:"provider"`
	SessionID string       `json:"session_id"`
	RemovedAt time.Time    `json:"removed_at"`
	ByYou     bool         `json:"by_you"`
}

// View is the removal as reader reads it.
func (r AnchoredSessionRemoval) View(reader string) AnchoredSessionRemovalView {
	return AnchoredSessionRemovalView{Provider: r.Provider, SessionID: r.SessionID, RemovedAt: r.RemovedAt.UTC(), ByYou: r.RemovedBy == reader}
}

// AnchoredSessionRemovalReceipt lists what one removal removed: the session
// asked for first, then the sessions it spawned.
type AnchoredSessionRemovalReceipt struct {
	Removed []AnchoredSessionRemovalView `json:"removed"`
}
