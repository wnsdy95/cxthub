package domain

// EffectiveMemorySelection never infers a worker's code position from shared
// main. MemoryHash, when supplied, pins an immutable attachment of SnapshotID.
type EffectiveMemorySelection struct {
	SnapshotID ContentHash `json:"snapshot_id"`
	CodeCommit string      `json:"code_commit"`
	MemoryHash ContentHash `json:"memory_hash,omitempty"`
}

func (s EffectiveMemorySelection) Validate() error {
	if ValidateContentHash(s.SnapshotID) != nil || ValidateGitOID(s.CodeCommit) != nil || ValidateOptionalContentHash(s.MemoryHash) != nil {
		return ErrValidation
	}
	return nil
}

type MemoryClaimAssessment struct {
	State  string `json:"state"` // applied, inactive, retained, review
	Reason string `json:"reason"`
}

// AssessMemoryClaim consumes verified file evidence and a separately proven
// integration state. It does not establish that an author's prose is true.
// "absent" means integration was excluded with complete evidence; "unknown"
// must never be interpreted as absence. Prose-only legacy text is not a claim.
func AssessMemoryClaim(claim MemoryClaim, paths []CodePathState, integration string) MemoryClaimAssessment {
	if claim.Kind == "decision" || claim.Kind == "rationale" {
		return MemoryClaimAssessment{"retained", "historical_knowledge"}
	}
	if claim.Kind != "code" || claim.Code == nil || len(paths) == 0 || len(paths) != len(claim.Code.Paths) {
		return MemoryClaimAssessment{"review", "scope_evidence_pending"}
	}
	after, before := true, true
	seen := map[string]bool{}
	declared := map[string]bool{}
	for _, p := range claim.Code.Paths {
		declared[p] = true
	}
	for _, p := range paths {
		switch p.State {
		case "applied", "before", "changed", "equivalent", "not_in_history":
		default:
			return MemoryClaimAssessment{"review", "scope_evidence_pending"}
		}
		if !declared[p.Path] || seen[p.Path] {
			return MemoryClaimAssessment{"review", "scope_evidence_pending"}
		}
		seen[p.Path] = true
		after = after && p.Selected == p.After
		before = before && p.Selected == p.Before
	}
	if integration == "verified" {
		if after {
			return MemoryClaimAssessment{"applied", "declared_scope_matches"}
		}
		if before {
			return MemoryClaimAssessment{"inactive", "declared_scope_before"}
		}
		return MemoryClaimAssessment{"review", "scope_changed"}
	}
	if integration == "absent" && before && !after {
		return MemoryClaimAssessment{"inactive", "source_not_selected"}
	}
	if after {
		return MemoryClaimAssessment{"review", "equivalent_without_integration"}
	}
	return MemoryClaimAssessment{"review", "integration_unproven"}
}

// EffectiveMemoryItem is a statement or a bounded piece of untyped history.
// Only code items can have applied/inactive scope evidence. Legacy text stays
// unverified and cannot silently become an instruction or current code fact.
type EffectiveMemoryItem struct {
	TextHash       ContentHash      `json:"text_hash,omitempty"`
	Part           int              `json:"part,omitempty"`
	ID             ContentHash      `json:"id"`
	SourceSnapshot ContentHash      `json:"source_snapshot"`
	Kind           string           `json:"kind"`
	Text           string           `json:"text"`
	Code           *MemoryCodeScope `json:"code,omitempty"`
	MemoryClaimAssessment
	PublicationIDs     []string        `json:"publication_ids,omitempty"`
	IntegrationReceipt string          `json:"integration_receipt,omitempty"`
	Paths              []CodePathState `json:"paths,omitempty"`
}

type EffectiveMemoryRequest struct {
	Selection EffectiveMemorySelection
	// Claims excludes untyped archival text without changing claim assessment.
	Content string // empty/all, claims
	Limit   int
	Cursor  string
}

type EffectiveMemoryPage struct {
	Content     string                   `json:"content,omitempty"`
	Selection   EffectiveMemorySelection `json:"selection"`
	Revision    RepositoryRevision       `json:"revision"`
	StateHash   ContentHash              `json:"state_hash"`
	LineageHash ContentHash              `json:"lineage_hash"`
	Items       []EffectiveMemoryItem    `json:"items"`
	Total       int                      `json:"total"`
	NextCursor  string                   `json:"next_cursor"`
}
