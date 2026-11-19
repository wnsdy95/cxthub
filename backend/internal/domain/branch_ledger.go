package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The branch ledger keeps one permanent record for each lifetime of a branch
// name on the remote: when the name came to exist there, when it was deleted,
// and which IDs were joined to it. A branch ID only tells two uses of one name
// apart; it proves nothing about when or how the branch was made.
//
// One rule decides every change: a name that is alive on the remote has
// exactly one lifetime, and registering under that name joins it.
//
// The ledger is separate from context branch identity: it never reads or
// moves a ref, and none of its fields is a ref's branch identity.

const (
	// BranchEvidenceRegistered: a member's CLI reported the branch as present
	// on the remote, with the ID it keeps for the branch.
	BranchEvidenceRegistered = "registered"
	// BranchEvidenceHostingPush: the hosting provider delivered a push to the
	// name. A push that leaves the name without a commit is its deletion.
	BranchEvidenceHostingPush = "hosting-push"

	branchLedgerNameLimit    = 255
	branchLedgerLinkedLimit  = 4096
	branchLedgerReopenLimit  = 4096
	branchLedgerAccountLimit = 256
)

var branchLedgerID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidBranchLedgerID reports whether id has the form of a branch ID: 32
// lowercase hexadecimal characters, whether a CLI or the server made it.
func ValidBranchLedgerID(id string) bool { return branchLedgerID.MatchString(id) }

// BranchEvidence says what a ledger change was based on.
type BranchEvidence struct {
	Kind string `json:"kind"`
	// Account is the signed-in account behind a registration.
	Account string `json:"account,omitempty"`
	// Source is the stored ref observation behind a hosting event.
	Source string `json:"source,omitempty"`
}

func (e BranchEvidence) validate() error {
	switch e.Kind {
	case BranchEvidenceRegistered:
		if strings.TrimSpace(e.Account) == "" || len(e.Account) > branchLedgerAccountLimit || e.Source != "" {
			return fmt.Errorf("%w: a registration names its account and nothing else", ErrValidation)
		}
	case BranchEvidenceHostingPush:
		if strings.TrimSpace(e.Source) == "" || len(e.Source) > branchLedgerAccountLimit || e.Account != "" {
			return fmt.Errorf("%w: a hosting event names its observation and nothing else", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: unknown branch evidence", ErrValidation)
	}
	return nil
}

// BranchLinkedID is another ID joined to a lifetime: the ID a member's CLI
// keeps for its local branch of that name.
type BranchLinkedID struct {
	ID       string         `json:"id"`
	LinkedAt time.Time      `json:"linked_at"`
	By       BranchEvidence `json:"by"`
}

// BranchReopening records a closing that turned out not to be the end: the
// lifetime's ID was registered again and the lifetime continued.
type BranchReopening struct {
	ClosedAt   time.Time      `json:"closed_at"`
	ClosedBy   BranchEvidence `json:"closed_by"`
	ReopenedAt time.Time      `json:"reopened_at"`
	ReopenedBy BranchEvidence `json:"reopened_by"`
	// Name is the name the lifetime had before, when it came back under another.
	Name string `json:"name,omitempty"`
}

// BranchLifetime is one lifetime of a branch name on the remote.
type BranchLifetime struct {
	RepoID ContentHash `json:"repo_id"`
	// LedgerID is the lifetime's own ID: the first ID it was opened with.
	LedgerID   string            `json:"ledger_id"`
	Name       string            `json:"name"`
	OpenedAt   time.Time         `json:"opened_at"`
	OpenedBy   BranchEvidence    `json:"opened_by"`
	ClosedAt   *time.Time        `json:"closed_at,omitempty"`
	ClosedBy   *BranchEvidence   `json:"closed_by,omitempty"`
	Linked     []BranchLinkedID  `json:"linked"`
	Reopenings []BranchReopening `json:"reopenings,omitempty"`
}

// Alive reports whether the name of this lifetime is on the remote.
func (l BranchLifetime) Alive() bool { return l.ClosedAt == nil }

// Holds reports whether id is this lifetime's ID or one joined to it.
func (l BranchLifetime) Holds(id string) bool {
	if l.LedgerID == id {
		return true
	}
	for _, linked := range l.Linked {
		if linked.ID == id {
			return true
		}
	}
	return false
}

// IDs returns the lifetime's ID followed by the IDs joined to it.
func (l BranchLifetime) IDs() []string {
	out := make([]string, 0, len(l.Linked)+1)
	out = append(out, l.LedgerID)
	for _, linked := range l.Linked {
		out = append(out, linked.ID)
	}
	return out
}

// SawObservation reports whether a stored ref observation already opened or
// closed this lifetime, so that a delivery processed twice changes it once.
func (l BranchLifetime) SawObservation(source string) bool {
	if source == "" {
		return false
	}
	if l.OpenedBy.Source == source || (l.ClosedBy != nil && l.ClosedBy.Source == source) {
		return true
	}
	for _, reopening := range l.Reopenings {
		if reopening.ClosedBy.Source == source || reopening.ReopenedBy.Source == source {
			return true
		}
	}
	return false
}

// ValidateBranchLedgerName checks a branch name as the remote holds it.
func ValidateBranchLedgerName(name string) error {
	if len(name) > branchLedgerNameLimit {
		return fmt.Errorf("%w: branch name is too long", ErrValidation)
	}
	return ValidateBranchName(name)
}

// Validate checks a stored lifetime.
func (l BranchLifetime) Validate() error {
	if ValidateContentHash(l.RepoID) != nil {
		return fmt.Errorf("%w: branch lifetime repository", ErrValidation)
	}
	if !ValidBranchLedgerID(l.LedgerID) {
		return fmt.Errorf("%w: branch lifetime ID", ErrValidation)
	}
	if err := ValidateBranchLedgerName(l.Name); err != nil {
		return err
	}
	if l.OpenedAt.IsZero() {
		return fmt.Errorf("%w: a branch lifetime has an opening time", ErrValidation)
	}
	if err := l.OpenedBy.validate(); err != nil {
		return err
	}
	if (l.ClosedAt == nil) != (l.ClosedBy == nil) {
		return fmt.Errorf("%w: a closed branch lifetime has a closing time and its evidence", ErrValidation)
	}
	if l.ClosedAt != nil {
		if l.ClosedAt.IsZero() {
			return fmt.Errorf("%w: a closed branch lifetime has a closing time", ErrValidation)
		}
		if err := l.ClosedBy.validate(); err != nil {
			return err
		}
	}
	if len(l.Linked) > branchLedgerLinkedLimit || len(l.Reopenings) > branchLedgerReopenLimit {
		return fmt.Errorf("%w: too many joined IDs or reopenings", ErrValidation)
	}
	seen := map[string]struct{}{l.LedgerID: {}}
	for _, linked := range l.Linked {
		if !ValidBranchLedgerID(linked.ID) || linked.LinkedAt.IsZero() {
			return fmt.Errorf("%w: a joined branch ID has an ID and a time", ErrValidation)
		}
		if _, dup := seen[linked.ID]; dup {
			return fmt.Errorf("%w: a branch ID is joined twice", ErrValidation)
		}
		seen[linked.ID] = struct{}{}
		if err := linked.By.validate(); err != nil {
			return err
		}
	}
	for _, reopening := range l.Reopenings {
		if reopening.ClosedAt.IsZero() || reopening.ReopenedAt.IsZero() {
			return fmt.Errorf("%w: a reopening has both times", ErrValidation)
		}
		if err := reopening.ClosedBy.validate(); err != nil {
			return err
		}
		if err := reopening.ReopenedBy.validate(); err != nil {
			return err
		}
		if reopening.Name != "" {
			if err := ValidateBranchLedgerName(reopening.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// BranchLedgerOutcome names what a registration or an observation did.
type BranchLedgerOutcome string

const (
	// BranchLedgerUnchanged: the ledger already said so.
	BranchLedgerUnchanged BranchLedgerOutcome = "unchanged"
	// BranchLedgerOpened: no lifetime was alive under the name; one opened.
	BranchLedgerOpened BranchLedgerOutcome = "opened"
	// BranchLedgerJoined: the ID was joined to the lifetime alive under the name.
	BranchLedgerJoined BranchLedgerOutcome = "joined"
	// BranchLedgerReopened: the ID belongs to a closed lifetime, which continues.
	BranchLedgerReopened BranchLedgerOutcome = "reopened"
	// BranchLedgerSeparate: the ID belongs to another lifetime, so the name
	// has a lifetime with an ID of its own.
	BranchLedgerSeparate BranchLedgerOutcome = "separate"
	// BranchLedgerClosed: the lifetime alive under the name was closed.
	BranchLedgerClosed BranchLedgerOutcome = "closed"
)

// RegisterBranchID applies the rule to a member's report that branch name is
// on the remote and that its CLI keeps localID for it.
//
// alive is the lifetime alive under name, if any; holder is the lifetime that
// holds localID, alive or closed, if any. The returned lifetime is the one the
// name has afterwards. store is false when the ledger already says so.
func RegisterBranchID(repo ContentHash, name, localID string, alive, holder *BranchLifetime, by BranchEvidence, at time.Time, newID func() string) (lifetime BranchLifetime, outcome BranchLedgerOutcome, store bool) {
	at = at.UTC()
	switch {
	case alive != nil && alive.Holds(localID):
		return *alive, BranchLedgerUnchanged, false
	case alive != nil && holder != nil:
		// The ID already belongs to another lifetime and cannot belong to
		// two. The name keeps the one lifetime it has.
		return *alive, BranchLedgerSeparate, false
	case alive != nil:
		// A name alive on the remote has one lifetime: the ID joins it.
		next := *alive
		next.Linked = append(append([]BranchLinkedID(nil), alive.Linked...), BranchLinkedID{ID: localID, LinkedAt: at, By: by})
		return next, BranchLedgerJoined, true
	case holder != nil && !holder.Alive():
		// Registering the ID of a closed lifetime again continues it.
		next := *holder
		reopening := BranchReopening{ClosedAt: *holder.ClosedAt, ClosedBy: *holder.ClosedBy, ReopenedAt: at, ReopenedBy: by}
		if holder.Name != name {
			reopening.Name = holder.Name
		}
		next.Reopenings = append(append([]BranchReopening(nil), holder.Reopenings...), reopening)
		next.Name, next.ClosedAt, next.ClosedBy = name, nil, nil
		return next, BranchLedgerReopened, true
	case holder != nil:
		// The ID is the identity of a branch alive under another name. This
		// name still needs its one lifetime, with an ID of its own.
		return BranchLifetime{RepoID: repo, LedgerID: newID(), Name: name, OpenedAt: at, OpenedBy: by, Linked: []BranchLinkedID{}}, BranchLedgerSeparate, true
	default:
		// No lifetime is alive under the name: the registering side's ID opens one.
		return BranchLifetime{RepoID: repo, LedgerID: localID, Name: name, OpenedAt: at, OpenedBy: by, Linked: []BranchLinkedID{}}, BranchLedgerOpened, true
	}
}

// ObserveBranchOnRemote applies the rule to the hosting provider showing the
// name with a commit. A name nobody registered gets an ID from the server.
func ObserveBranchOnRemote(repo ContentHash, name string, alive *BranchLifetime, by BranchEvidence, at time.Time, newID func() string) (lifetime BranchLifetime, outcome BranchLedgerOutcome, store bool) {
	if alive != nil {
		return *alive, BranchLedgerUnchanged, false
	}
	return BranchLifetime{RepoID: repo, LedgerID: newID(), Name: name, OpenedAt: at.UTC(), OpenedBy: by, Linked: []BranchLinkedID{}}, BranchLedgerOpened, true
}

// ObserveBranchDeleted applies the rule to the hosting provider showing the
// name gone. The lifetime stays in the ledger, closed.
func ObserveBranchDeleted(alive *BranchLifetime, by BranchEvidence, at time.Time) (lifetime BranchLifetime, outcome BranchLedgerOutcome, store bool) {
	if alive == nil {
		return BranchLifetime{}, BranchLedgerUnchanged, false
	}
	next, closed := *alive, at.UTC()
	next.ClosedAt, next.ClosedBy = &closed, &by
	return next, BranchLedgerClosed, true
}

// BranchRegistration is one branch a member's CLI reports as present on the
// remote, with the ID it keeps for it.
type BranchRegistration struct {
	Name    string `json:"name"`
	LocalID string `json:"local_id"`
}

// BranchRegistrationLimit bounds one report.
const BranchRegistrationLimit = 2000

// Validate checks one reported branch.
func (r BranchRegistration) Validate() error {
	if err := ValidateBranchLedgerName(r.Name); err != nil {
		return err
	}
	if !ValidBranchLedgerID(r.LocalID) {
		return fmt.Errorf("%w: branch ID", ErrValidation)
	}
	return nil
}

// BranchRegistrationResult says which lifetime the name has after a
// registration and what the registration did.
type BranchRegistrationResult struct {
	Name     string              `json:"name"`
	LocalID  string              `json:"local_id"`
	LedgerID string              `json:"ledger_id"`
	Outcome  BranchLedgerOutcome `json:"outcome"`
}
