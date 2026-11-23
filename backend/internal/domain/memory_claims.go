package domain

const MemoryClaimsVersion uint32 = 1

// MemoryClaim is an author's explicitly scoped statement. File evidence can
// assess its declared scope; it cannot prove the prose's semantic truth.
type MemoryClaim struct {
	Kind string           `json:"kind"` // code, decision, rationale
	Text string           `json:"text"`
	Code *MemoryCodeScope `json:"code,omitempty"`
}

type MemoryCodeScope struct {
	Commit string   `json:"commit"`
	Parent string   `json:"parent,omitempty"`
	Paths  []string `json:"paths"`
}
