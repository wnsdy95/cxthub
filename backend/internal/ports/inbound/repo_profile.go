package inbound

// RepoProfilePatch preserves absent fields and applies configuration and about
// metadata together in one application transaction.
type RepoProfilePatch struct {
	Description    *string  `json:"description"`
	Website        *string  `json:"website"`
	Topics         []string `json:"topics"`
	DefaultBranch  *string  `json:"default_branch"`
	ProtectDefault *bool    `json:"protect_default"`
}
