package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const AgentContextVersion = 1
const MaxAgentContextTokens = 800000
const UTF8ByteBoundCounter = "utf8_bytes_conservative_bound"

// DefaultMemoryContextTokens is a bounded engineering default, not a measured
// model-quality claim. It may be configured independently from history budgets.
const DefaultMemoryContextTokens = 8000

var ErrContextBudgetExceeded = errors.New("context_budget_exceeded")
var ErrProviderCapabilityUnknown = errors.New("provider_capability_unknown")
var ErrAgentContextUnavailable = errors.New("agent_context_unavailable")

// InputPolicy describes supplied material, independently of replay fidelity.
type InputPolicy struct {
	Version      int    `json:"version"`
	Mode         string `json:"mode"`
	BudgetTokens int    `json:"budget_tokens"`
	Source       string `json:"source"`
}

func MemoryInputPolicy() InputPolicy {
	return InputPolicy{Version: AgentContextVersion, Mode: "memory", BudgetTokens: DefaultMemoryContextTokens, Source: "product_default"}
}

// ParseHistoryBudget uses decimal k, never binary bytes. Empty and full have
// identical meaning. Duplicate flags are rejected by the command parser.
func ParseHistoryBudget(value string) (int, error) {
	if value == "" || value == "full" {
		return MaxAgentContextTokens, nil
	}
	multiplier := 1
	if strings.HasSuffix(value, "k") {
		multiplier = 1000
		value = strings.TrimSuffix(value, "k")
	}
	if value == "" {
		return 0, fmt.Errorf("%w: empty history budget", ErrContextBudgetExceeded)
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: use a positive token count, 200k, or full", ErrContextBudgetExceeded)
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 || n > MaxAgentContextTokens/multiplier {
		return 0, fmt.Errorf("%w: budget must be 1..800000 tokens", ErrContextBudgetExceeded)
	}
	return n * multiplier, nil
}

func (p InputPolicy) Validate() error {
	if p.Version != AgentContextVersion || (p.Mode != "memory" && p.Mode != "history") || p.Source == "" {
		return fmt.Errorf("%w: unsupported input policy", ErrAgentContextUnavailable)
	}
	if p.BudgetTokens <= 0 || p.BudgetTokens > MaxAgentContextTokens {
		return ErrContextBudgetExceeded
	}
	return nil
}

// PersonalWorkScope prevents another contributor's open tasks becoming the
// current user's instructions. All three values must match an explicit source.
type PersonalWorkScope struct {
	ActorID    string `json:"actor_id"`
	SessionID  string `json:"session_id"`
	WorktreeID string `json:"worktree_id"`
}

func (s PersonalWorkScope) Complete() bool {
	return s.ActorID != "" && s.SessionID != "" && s.WorktreeID != ""
}

type AgentSourcePointer struct {
	SnapshotID  ContentHash      `json:"snapshot_id"`
	DocHash     ContentHash      `json:"doc_hash,omitempty"`
	DocIdentity DocumentIdentity `json:"doc_identity,omitempty"`
	MemoryHash  ContentHash      `json:"memory_hash,omitempty"`
	StartEvent  int              `json:"start_event,omitempty"`
	EndEvent    int              `json:"end_event,omitempty"`
	Tool        string           `json:"tool"`
}

type ExactUserConstraint struct {
	Text   string             `json:"text"`
	Source AgentSourcePointer `json:"source"`
}

type PersonalWorkState struct {
	Scope            PersonalWorkScope     `json:"scope"`
	Goal             string                `json:"goal,omitempty"`
	Acceptance       []string              `json:"acceptance,omitempty"`
	Completed        []string              `json:"completed,omitempty"`
	Remaining        []string              `json:"remaining,omitempty"`
	LastVerification string                `json:"last_verification,omitempty"`
	NextStep         string                `json:"next_step,omitempty"`
	Constraints      []ExactUserConstraint `json:"exact_user_constraints,omitempty"`
	Sources          []AgentSourcePointer  `json:"sources"`
}

// AgentMemoryPin selects an exact historical attachment. A non-nil empty pin
// means that no memory existed at the selected position; it must not query a
// newer attachment. SnapshotID is the attachment's owner, which can be an
// ancestor of the selected conversation snapshot.
type AgentMemoryPin struct {
	SnapshotID ContentHash `json:"snapshot_id,omitempty"`
	MemoryHash ContentHash `json:"memory_hash,omitempty"`
}

func (p *AgentMemoryPin) Validate() error {
	if p == nil || (p.SnapshotID == "" && p.MemoryHash == "") {
		return nil
	}
	if ValidateContentHash(p.SnapshotID) != nil || ValidateContentHash(p.MemoryHash) != nil {
		return ErrHashMismatch
	}
	return nil
}

type AgentContextSelection struct {
	RepositoryID     string      `json:"repository_id"`
	Branch           string      `json:"branch,omitempty"`
	SnapshotID       ContentHash `json:"snapshot_id"`
	CodeCommit       string      `json:"code_commit"`
	ContextStateHash ContentHash `json:"context_state_hash"`
	MemoryStateHash  ContentHash `json:"memory_state_hash"`
	// Both proofs bind the complete server projection, independently of the
	// repository-wide pagination/revision generation. Never use a partial pair.
	ContextDeliveryHash ContentHash           `json:"context_delivery_hash,omitempty"`
	MemoryDeliveryHash  ContentHash           `json:"memory_delivery_hash,omitempty"`
	MemoryPin           *AgentMemoryPin       `json:"memory_pin,omitempty"`
	WorktreeStateHash   ContentHash           `json:"worktree_state_hash,omitempty"`
	EvidenceRevision    uint64                `json:"evidence_revision,string"`
	GraphRevision       uint64                `json:"graph_revision,string"`
	SourcePolicy        string                `json:"source_policy,omitempty"`
	WorkingPosition     *AgentWorkingPosition `json:"working_position,omitempty"`
}

const AgentSourceLatestMain = "latest_server_main"

// AgentWorkingPosition fences the code being edited without using it to select
// project knowledge. Source CodeCommit above describes server main instead.
type AgentWorkingPosition struct {
	Branch     string `json:"branch,omitempty"`
	CodeCommit string `json:"code_commit"`
}

func (s AgentContextSelection) DeliveryCodeCommit() string {
	if s.WorkingPosition != nil {
		return s.WorkingPosition.CodeCommit
	}
	return s.CodeCommit
}

func (s AgentContextSelection) DeliveryBranch() string {
	if s.WorkingPosition != nil {
		return s.WorkingPosition.Branch
	}
	return s.Branch
}

func (s AgentContextSelection) ValidateSource() error {
	if (s.ContextDeliveryHash == "") != (s.MemoryDeliveryHash == "") || ValidateOptionalContentHash(s.ContextDeliveryHash) != nil || ValidateOptionalContentHash(s.MemoryDeliveryHash) != nil {
		return ErrHashMismatch
	}
	if s.SourcePolicy == "" && s.WorkingPosition == nil {
		return nil
	}
	if s.SourcePolicy != AgentSourceLatestMain || s.Branch != "main" || s.MemoryPin != nil || s.WorkingPosition == nil || !ValidGitOID(s.WorkingPosition.CodeCommit) || ValidateContentHash(s.WorktreeStateHash) != nil {
		return ErrAgentContextUnavailable
	}
	return nil
}

type AgentHistorySegment struct {
	Source    AgentSourcePointer `json:"source"`
	SessionID string             `json:"session_id"`
	// Events are historical evidence inside a quoted package, not instructions
	// or provider-native replay. Opaque provider state is never decoded here.
	Events []Event `json:"events"`
}

type AgentCoverageGap struct {
	Reason string              `json:"reason"`
	Source *AgentSourcePointer `json:"source,omitempty"`
}

type AgentContextContent struct {
	Notice        string                `json:"notice"`
	Selection     AgentContextSelection `json:"selection"`
	ProjectMemory []EffectiveMemoryItem `json:"project_memory"`
	PersonalWork  *PersonalWorkState    `json:"personal_work,omitempty"`
	Sources       []AgentSourcePointer  `json:"sources"`
	History       []AgentHistorySegment `json:"historical_evidence,omitempty"`
	Gaps          []AgentCoverageGap    `json:"coverage_gaps"`
}

type AgentTokenUsage struct {
	Tokens    int    `json:"tokens"`
	Exact     bool   `json:"exact"`
	Tokenizer string `json:"tokenizer"`
	// Scope distinguishes local text accounting from an entire native request.
	// Optional fields preserve existing serialized packages and their hashes.
	Scope  string `json:"scope,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// AgentHostCapability must come from a verified adapter, not a larger arbitrary
// model_context_window setting. Prepared is not provider-accepted.
type AgentHostCapability struct {
	Provider        ProviderKind `json:"provider"`
	Model           string       `json:"model"`
	HostVersion     string       `json:"host_version"`
	Evidence        string       `json:"evidence"`
	Verified        bool         `json:"verified"`
	ContextWindow   int          `json:"context_window"`
	HostInputTokens int          `json:"host_input_tokens"`
	HostInputKnown  bool         `json:"host_input_known"`
	// InitialPromptTokens is reserved by the app under the declared text policy.
	// HostInputTokens and FramingTokens must exclude it to avoid double counting.
	InitialPromptTokens int `json:"initial_prompt_tokens,omitempty"`
	// Required output/reasoning/work space, separate from fixed host input.
	ReservedTokens    int `json:"reserved_tokens"`
	FramingTokens     int `json:"framing_tokens"`
	AutoCompactTokens int `json:"auto_compact_tokens,omitempty"`
	// Legacy strict accounting requires Known even when compaction is disabled.
	// Measured reserve accounting can retain an explicitly unknown threshold.
	AutoCompactKnown bool   `json:"auto_compact_known"`
	Tokenizer        string `json:"tokenizer"`
	// Measured input accounting still requires verified runtime/window evidence.
	// RuntimeScope is an adapter-owned fingerprint of provider/account routing,
	// configuration and instruction/tool scope, never a user window override.
	InputAccountingPolicy string                `json:"input_accounting_policy,omitempty"`
	RuntimeScope          ContentHash           `json:"runtime_scope,omitempty"`
	Calibration           AgentInputCalibration `json:"-"`
	// Native estimate policy only. The baseline precedes reference append and
	// is not known exact host input. A measurement tag distinguishes zero from
	// absent evidence. FramingAllowanceTokens covers the reference projection.
	BaselineInputEstimateTokens int    `json:"baseline_input_estimate_tokens,omitempty"`
	BaselineInputMeasurement    string `json:"baseline_input_measurement,omitempty"`
	FramingAllowanceTokens      int    `json:"framing_allowance_tokens,omitempty"`
}

func (c AgentHostCapability) Check(provider ProviderKind, model string, usage AgentTokenUsage) error {
	b, err := c.ResolveBudget(provider, model, MaxAgentContextTokens, usage)
	if err != nil {
		return err
	}
	return b.Validate(provider, model, MaxAgentContextTokens, usage)
}

type AgentContextPackage struct {
	Provider     ProviderKind        `json:"provider,omitempty"`
	Version      int                 `json:"version"`
	ID           ContentHash         `json:"id"`
	Policy       InputPolicy         `json:"policy"`
	Content      AgentContextContent `json:"content"`
	Usage        AgentTokenUsage     `json:"usage"`
	Capability   string              `json:"capability"`
	Delivery     string              `json:"delivery"`
	ArtifactOnly bool                `json:"artifact_only,omitempty"`
	// Optional so previously prepared package hashes remain unchanged.
	Budget *AgentContextBudget `json:"budget,omitempty"`
	// In-memory authorization only; receipts never contain the initial prompt.
	initialPromptReservation AgentPromptReservation
}

// Artifact returns an inspectable package/receipt. Its metadata is not part of
// model input and must not be mistaken for provider acceptance.
func (p AgentContextPackage) Artifact() ([]byte, error) {
	if err := p.ValidateIdentity(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(p, "", "  ")
}

// Prompt is the exact text accounted by Usage. Receipt metadata is excluded
// because it is not supplied to the agent. Historical JSON stays quoted data.
func (p AgentContextPackage) Prompt() (string, error) {
	raw, err := json.Marshal(p.Content)
	if err != nil {
		return "", err
	}
	return "[cxt context package v1]\n" + string(raw), nil
}

// Digest binds content to its policy, accounting and delivery restriction. An
// artifact receipt cannot become launchable by flipping ArtifactOnly alone.
func (p AgentContextPackage) Digest() (ContentHash, error) {
	p.ID = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func (p AgentContextPackage) ValidateIdentity() error {
	if p.Version != AgentContextVersion || p.Delivery != "prepared" {
		return ErrAgentContextUnavailable
	}
	expected, err := p.Digest()
	if err != nil {
		return err
	}
	if expected != p.ID {
		return ErrHashMismatch
	}
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	if err := p.Content.Selection.ValidateSource(); err != nil {
		return err
	}
	if p.Budget != nil {
		if p.Policy.Mode != "history" || p.ArtifactOnly || p.Capability != "verified_for_preparation" {
			return ErrAgentContextUnavailable
		}
		if err := p.Budget.Validate(p.Budget.Provider, p.Budget.Model, p.Policy.BudgetTokens, p.Usage); err != nil {
			return err
		}
		kind, err := ValidateAgentTokenAccounting(p.Budget.InputAccountingPolicy, p.Budget.Provider, p.Budget.Tokenizer, p.Usage)
		if err != nil {
			return err
		}
		if kind == AgentTextUTF8Allowance {
			prompt, err := p.Prompt()
			if err != nil {
				return err
			}
			if p.Usage.Tokens != len(prompt) {
				return fmt.Errorf("%w: package allowance differs from its rendered UTF-8 byte length", ErrHashMismatch)
			}
		}
	}
	return nil
}

// IsAgentContextPackageText lets capture/distillation exclude synthetic input
// from new user decisions. Archived source records remain untouched.
func IsAgentContextPackageText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "[cxt context package v1]\n")
}
