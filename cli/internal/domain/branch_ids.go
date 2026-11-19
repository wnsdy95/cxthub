package domain

// A branch is identified by its name and an ID of its own. The ID only tells
// two uses of one name apart; it proves nothing about when or how the branch
// was made. Git keeps it in the branch's own configuration, so Git moves it
// with a rename, drops it with the branch, and a branch made again under the
// same name starts without one.
const (
	// BranchIDConfigKey is the per-branch setting that holds the ID:
	// branch.<name>.cxtBranchId.
	BranchIDConfigKey = "cxtBranchId"
	// BranchIDIssuedForConfigKey holds the name the ID was issued for:
	// branch.<name>.cxtBranchIdIssuedFor. Copying a branch copies its
	// configuration; the name tells the copy from the original.
	BranchIDIssuedForConfigKey = "cxtBranchIdIssuedFor"
)

// LocalBranch is a branch of the repository and what its configuration holds.
type LocalBranch struct {
	Name      string
	ID        string
	IssuedFor string
	// Remote is the branch's name on origin, when Git's own records show it
	// there: the upstream the branch is configured to track on origin, or
	// else a remote-tracking branch of origin with the same name.
	Remote string
}

// BranchIDAssignment is a new ID to store for a branch, issued for its name.
type BranchIDAssignment struct {
	Name string
	ID   string
}

// ValidBranchID reports whether id is an ID this CLI issues: 32 lowercase
// hexadecimal characters.
func ValidBranchID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// PlanBranchIDs decides which branches get a new ID.
//
// A branch without an ID gets one. When two or more branches hold the same
// ID, every holder whose name is not the name the ID was issued for gets a
// new one: it is a copy.
func PlanBranchIDs(branches []LocalBranch, newID func() string) []BranchIDAssignment {
	holders := make(map[string]int, len(branches))
	for _, branch := range branches {
		if ValidBranchID(branch.ID) {
			holders[branch.ID]++
		}
	}
	var out []BranchIDAssignment
	for _, branch := range branches {
		if branch.Name == "" {
			continue
		}
		if !ValidBranchID(branch.ID) || (holders[branch.ID] > 1 && branch.IssuedFor != branch.Name) {
			out = append(out, BranchIDAssignment{Name: branch.Name, ID: newID()})
		}
	}
	return out
}

// BranchRegistration reports one branch as present on the remote, with the ID
// this CLI keeps for it.
type BranchRegistration struct {
	Name    string `json:"name"`
	LocalID string `json:"local_id"`
}

// BranchRegistrationResult is the server's answer for one reported branch:
// the ID of the lifetime the name has, and what the report did.
type BranchRegistrationResult struct {
	Name     string `json:"name"`
	LocalID  string `json:"local_id"`
	LedgerID string `json:"ledger_id"`
	Outcome  string `json:"outcome"`
}

// BranchRegistrations lists the branches Git's records show on origin, each
// with its ID. A branch without an ID or without a name on origin is left out.
func BranchRegistrations(branches []LocalBranch) []BranchRegistration {
	out := []BranchRegistration{}
	seen := map[BranchRegistration]struct{}{}
	for _, branch := range branches {
		if branch.Remote == "" || !ValidBranchID(branch.ID) {
			continue
		}
		registration := BranchRegistration{Name: branch.Remote, LocalID: branch.ID}
		if _, dup := seen[registration]; dup {
			continue
		}
		seen[registration] = struct{}{}
		out = append(out, registration)
	}
	return out
}
