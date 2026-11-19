package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// An anchored session is one coding-agent session as its uploader's CLI read it
// from the records the tools and Git left behind: where the session is cut at
// the commits it made, which rule linked each commit, where the uncommitted
// part was worked on, and which session spawned it.
//
// The CLI computes the cut points, because only it can read the session
// records and the local repository. The server stores them as reported and
// replaces a session's report as a whole when the CLI reads again. Whatever
// depends on the hosting provider is derived on the server from this report;
// the report itself never claims it.

const (
	// AnchoredSessionRuleVersion is the version of the linking and cutting
	// rules this server understands.
	AnchoredSessionRuleVersion = 1

	AnchoredLinkPrintedNumber = "printed-number"
	AnchoredLinkFolderAndTime = "same-folder-and-time"

	AnchoredScopeFolder    = "working-folder"
	AnchoredScopeOriginURL = "origin-url"

	AnchoredSpawnToolCall  = "tool-call"
	AnchoredSpawnStartTime = "start-time"

	AnchoredPlaceRecorded      = "recorded-by-tool"
	AnchoredPlaceHeadLog       = "head-log"
	AnchoredPlaceCurrentBranch = "current-branch"
	AnchoredPlaceRemovedFolder = "folder-no-longer-a-worktree"

	// AnchoredSessionBodyLimit bounds one uploaded report.
	AnchoredSessionBodyLimit = 4 << 20

	anchoredSegmentLimit   = 20000
	anchoredCommitLimit    = 256
	anchoredPlaceLimit     = 256
	anchoredAgentCallLimit = 50000
	anchoredCopiedLimit    = 256
	anchoredNameLimit      = 256
	anchoredEventLimit     = 100_000_000
	anchoredTurnLimit      = 20000
)

var (
	anchoredSessionID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	anchoredCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)
)

// AnchoredRange names a part of one session by the tool's own first and last
// record identifiers and by their positions among the session's own records.
type AnchoredRange struct {
	First        string     `json:"first"`
	Last         string     `json:"last"`
	FirstOrdinal int        `json:"first_ordinal"`
	LastOrdinal  int        `json:"last_ordinal"`
	Commands     int        `json:"commands"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	// FirstEvent and Events place the range in the session's content
	// document. Both are zero when the report names no content.
	FirstEvent int `json:"first_event,omitempty"`
	Events     int `json:"events,omitempty"`
}

// AnchoredContent names the document that holds the conversation of the
// session's own records, each stored once, in the order of the ranges. The
// document is an ordinary repository document, so its bytes are verified,
// counted and read the way every document is.
type AnchoredContent struct {
	Doc      ContentHash      `json:"doc"`
	Identity DocumentIdentity `json:"identity,omitempty"`
	Events   int              `json:"events"`
}

// AnchoredCommit is a commit a segment ends at. The author time, author and
// subject are what a rebase or cherry-pick keeps, so the server can find the
// commit's copies without the uploader's repository.
type AnchoredCommit struct {
	SHA         string     `json:"sha"`
	Rule        string     `json:"rule"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	AuthoredAt  *time.Time `json:"authored_at,omitempty"`
	AuthorEmail string     `json:"author_email,omitempty"`
	Subject     string     `json:"subject,omitempty"`
}

// AnchoredSegment is the part of a session that ends at one or more commits.
type AnchoredSegment struct {
	AnchoredRange
	ClosesTurn bool             `json:"closes_turn"`
	Commits    []AnchoredCommit `json:"commits"`
	// Hidden is set only in what another account reads: the segment keeps its
	// place, so segment numbers mean the same to everyone, and says nothing
	// else. A report never carries it.
	Hidden bool `json:"hidden,omitempty"`
}

// AnchoredPlace is a worktree and branch an uncommitted part passed through.
// Only the worktree's name is reported, never its path.
type AnchoredPlace struct {
	Worktree string `json:"worktree"`
	Branch   string `json:"branch,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
	Rule     string `json:"rule"`
}

// AnchoredUncommitted is the part of a session after its last commit.
type AnchoredUncommitted struct {
	AnchoredRange
	// Turns says where each turn of the part begins, in order. The part is
	// not cut at them: they let the server tell which turns had begun when a
	// branch the part passed through was pushed. A report may leave them out.
	Turns    []AnchoredTurnMark `json:"turns,omitempty"`
	Places   []AnchoredPlace    `json:"places"`
	Unplaced bool               `json:"unplaced"`
	// Partial is set only in what another account reads: the part is open up
	// to here and goes on. A report never carries it.
	Partial bool `json:"partial,omitempty"`
}

// AnchoredTurnMark is where one turn begins: the position of its first record
// among the session's own records, where that is in the content document, and
// the record's time.
type AnchoredTurnMark struct {
	FirstOrdinal int        `json:"first_ordinal"`
	FirstEvent   int        `json:"first_event,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
}

// AnchoredSpawn says where in the parent session this session was spawned:
// the parent's record and its position among the parent's own records. The
// position, not a segment number, is reported, so the parent's segment can be
// found again whenever the parent is cut differently.
type AnchoredSpawn struct {
	AtRecord  string `json:"at_record"`
	AtOrdinal int    `json:"at_ordinal"`
	Rule      string `json:"rule"`
}

// AnchoredAgentCall is one agent-to-agent call the session's tool recorded.
type AnchoredAgentCall struct {
	Tool   string     `json:"tool"`
	From   string     `json:"from"`
	To     []string   `json:"to"`
	Status string     `json:"status,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	Record string     `json:"record,omitempty"`
}

// AnchoredSessionBody is the report a CLI uploads for one session.
type AnchoredSessionBody struct {
	RuleVersion   int                  `json:"rule_version"`
	Sub           bool                 `json:"sub"`
	Parent        string               `json:"parent,omitempty"`
	Spawn         *AnchoredSpawn       `json:"spawn,omitempty"`
	Scope         string               `json:"scope"`
	Machine       string               `json:"machine,omitempty"`
	StartedAt     *time.Time           `json:"started_at,omitempty"`
	EndedAt       *time.Time           `json:"ended_at,omitempty"`
	Records       int                  `json:"records"`
	Commands      int                  `json:"commands"`
	CopiedRecords int                  `json:"copied_records"`
	CopiedFrom    []string             `json:"copied_from,omitempty"`
	FileCopies    int                  `json:"file_copies,omitempty"`
	Segments      []AnchoredSegment    `json:"segments"`
	Uncommitted   *AnchoredUncommitted `json:"uncommitted,omitempty"`
	AgentCalls    []AnchoredAgentCall  `json:"agent_calls,omitempty"`
	Content       *AnchoredContent     `json:"content,omitempty"`
}

// AnchoredSession is a stored report with what the server attached to it.
type AnchoredSession struct {
	RepoID    ContentHash         `json:"repo_id"`
	Provider  ProviderKind        `json:"provider"`
	SessionID string              `json:"session_id"`
	Uploader  string              `json:"uploader"`
	Revision  int64               `json:"revision"`
	BodyHash  ContentHash         `json:"body_hash"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	Body      AnchoredSessionBody `json:"body"`
}

// AnchoredSessionSummary lists a stored report without its ranges: what a
// list of sessions, commits, worktrees and branches is drawn from. The ranges
// and the agent calls stay in the report itself.
type AnchoredSessionSummary struct {
	Provider    ProviderKind                `json:"provider"`
	SessionID   string                      `json:"session_id"`
	Parent      string                      `json:"parent,omitempty"`
	Sub         bool                        `json:"sub"`
	Revision    int64                       `json:"revision"`
	BodyHash    ContentHash                 `json:"body_hash"`
	Records     int                         `json:"records"`
	Commands    int                         `json:"commands"`
	Segments    int                         `json:"segments"`
	Machine     string                      `json:"machine,omitempty"`
	StartedAt   *time.Time                  `json:"started_at,omitempty"`
	EndedAt     *time.Time                  `json:"ended_at,omitempty"`
	Content     bool                        `json:"content"`
	Commits     []AnchoredSummaryCommit     `json:"commits"`
	Uncommitted *AnchoredSummaryUncommitted `json:"uncommitted,omitempty"`
	SpawnedIn   *AnchoredSummarySpawn       `json:"spawned_in,omitempty"`
	// Uploader is the account the session belongs to. Shared is true when
	// that is not the caller: the summary then holds only what is open.
	Uploader  string    `json:"uploader"`
	Shared    bool      `json:"shared,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AnchoredSummarySpawn places a spawned session in its parent as the parent is
// cut now: in one of its segments, or in the part after its last commit.
type AnchoredSummarySpawn struct {
	SegmentNo   int    `json:"segment_no,omitempty"`
	Uncommitted bool   `json:"uncommitted,omitempty"`
	Rule        string `json:"rule"`
}

// AnchoredSummaryCommit is a commit one of the session's segments ends at.
type AnchoredSummaryCommit struct {
	SHA        string     `json:"sha"`
	SegmentNo  int        `json:"segment_no"`
	Rule       string     `json:"rule"`
	Subject    string     `json:"subject,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	AuthoredAt *time.Time `json:"authored_at,omitempty"`
	// OnRemote and Copies say where the commit is now, from the server's own
	// read of the hosting provider: on the remote itself, or as copies with
	// the same author time, author and subject. OnRemote is absent until the
	// provider was read: nothing is then known to be there, or missing.
	OnRemote *bool    `json:"on_remote,omitempty"`
	Copies   []string `json:"copies,omitempty"`
}

// AnchoredSummaryUncommitted is where the part after the last commit was
// worked on.
type AnchoredSummaryUncommitted struct {
	Commands  int             `json:"commands"`
	StartedAt *time.Time      `json:"started_at,omitempty"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
	Places    []AnchoredPlace `json:"places"`
	Unplaced  bool            `json:"unplaced"`
}

// AnchoredCommitSegment is one session's segment that ends at a commit.
type AnchoredCommitSegment struct {
	Provider  ProviderKind    `json:"provider"`
	SessionID string          `json:"session_id"`
	SegmentNo int             `json:"segment_no"`
	Rule      string          `json:"rule"`
	Segment   AnchoredSegment `json:"segment"`
}

// AnchoredCommitLink is the stored index entry behind AnchoredCommitSegment.
type AnchoredCommitLink struct {
	Provider  ProviderKind
	SessionID string
	CommitSHA string
	SegmentNo int
	Rule      string
}

// AnchoredSessionReceipt answers an upload. Changed is false when the stored
// report already had the same content.
type AnchoredSessionReceipt struct {
	Revision int64       `json:"revision"`
	BodyHash ContentHash `json:"body_hash"`
	Changed  bool        `json:"changed"`
}

// ValidAnchoredSessionKey reports whether a tool and session identifier can
// name a stored report.
func ValidAnchoredSessionKey(provider ProviderKind, sessionID string) bool {
	return (provider == ProviderClaude || provider == ProviderCodex) && anchoredSessionID.MatchString(sessionID)
}

// ValidAnchoredCommitSHA reports whether sha is a full commit number.
func ValidAnchoredCommitSHA(sha string) bool { return anchoredCommitSHA.MatchString(sha) }

func anchoredText(value string, limit int, required bool) bool {
	if value == "" {
		return !required
	}
	if len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func anchoredOneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func anchoredInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: anchored session: %s", ErrValidation, fmt.Sprintf(format, args...))
}

func (r AnchoredRange) validate(what string, first, firstEvent int) error {
	if !anchoredText(r.First, anchoredNameLimit, true) || !anchoredText(r.Last, anchoredNameLimit, true) {
		return anchoredInvalid("%s needs its first and last record", what)
	}
	if r.FirstOrdinal != first || r.LastOrdinal < r.FirstOrdinal {
		return anchoredInvalid("%s must start at record %d and end at or after it", what, first)
	}
	if r.FirstEvent != firstEvent || r.Events < 0 {
		return anchoredInvalid("%s must start at event %d of the content", what, firstEvent)
	}
	if r.Commands < 0 {
		return anchoredInvalid("%s has a negative command count", what)
	}
	if r.StartedAt != nil && r.EndedAt != nil && r.EndedAt.Before(*r.StartedAt) {
		return anchoredInvalid("%s ends before it starts", what)
	}
	return nil
}

// Validate checks that a report is well formed: every record of the session
// is in exactly one segment or in the uncommitted part, in order, and every
// commit is named once.
func (b AnchoredSessionBody) Validate() error {
	if b.RuleVersion != AnchoredSessionRuleVersion {
		return anchoredInvalid("rule version %d is not supported", b.RuleVersion)
	}
	if !anchoredOneOf(b.Scope, AnchoredScopeFolder, AnchoredScopeOriginURL) {
		return anchoredInvalid("unknown scope")
	}
	if b.Parent != "" && !anchoredSessionID.MatchString(b.Parent) {
		return anchoredInvalid("parent is not a session identifier")
	}
	if b.Spawn != nil && (b.Parent == "" || !anchoredText(b.Spawn.AtRecord, anchoredNameLimit, true) || b.Spawn.AtOrdinal < 0 || !anchoredOneOf(b.Spawn.Rule, AnchoredSpawnToolCall, AnchoredSpawnStartTime)) {
		return anchoredInvalid("spawn point needs a parent, a record and a known rule")
	}
	if !anchoredText(b.Machine, anchoredNameLimit, false) {
		return anchoredInvalid("machine label is not plain text")
	}
	if b.Records < 0 || b.Commands < 0 || b.CopiedRecords < 0 || b.FileCopies < 0 {
		return anchoredInvalid("counts cannot be negative")
	}
	if len(b.CopiedFrom) > anchoredCopiedLimit {
		return anchoredInvalid("too many source sessions")
	}
	for _, source := range b.CopiedFrom {
		if !anchoredSessionID.MatchString(source) {
			return anchoredInvalid("a copied-from entry is not a session identifier")
		}
	}
	if len(b.Segments) > anchoredSegmentLimit {
		return anchoredInvalid("too many segments")
	}
	if content := b.Content; content != nil {
		if ValidateContentHash(content.Doc) != nil || (content.Identity != DocumentIdentityLegacy && content.Identity != DocumentIdentityRootV1) ||
			content.Events < 1 || content.Events > anchoredEventLimit {
			return anchoredInvalid("content needs a document, a known identity and its event count")
		}
	}
	next, nextEvent := 0, 0
	seen := map[string]struct{}{}
	for index, segment := range b.Segments {
		if segment.Hidden {
			return anchoredInvalid("segment %d is marked hidden; a report holds every segment", index+1)
		}
		if err := segment.validate(fmt.Sprintf("segment %d", index+1), next, nextEvent); err != nil {
			return err
		}
		nextEvent += segment.Events
		if len(segment.Commits) == 0 || len(segment.Commits) > anchoredCommitLimit {
			return anchoredInvalid("segment %d must end at 1 to %d commits", index+1, anchoredCommitLimit)
		}
		for _, commit := range segment.Commits {
			if !ValidAnchoredCommitSHA(commit.SHA) || !anchoredOneOf(commit.Rule, AnchoredLinkPrintedNumber, AnchoredLinkFolderAndTime) {
				return anchoredInvalid("segment %d names a commit without a full number or a known rule", index+1)
			}
			if _, dup := seen[commit.SHA]; dup {
				return anchoredInvalid("commit %s is named twice", commit.SHA[:12])
			}
			seen[commit.SHA] = struct{}{}
			if !anchoredText(commit.AuthorEmail, anchoredNameLimit, false) || len(commit.Subject) > 1024 {
				return anchoredInvalid("commit %s has an oversized author or subject", commit.SHA[:12])
			}
		}
		next = segment.LastOrdinal + 1
	}
	if part := b.Uncommitted; part != nil {
		if err := part.validate("the uncommitted part", next, nextEvent); err != nil {
			return err
		}
		nextEvent += part.Events
		if len(part.Places) > anchoredPlaceLimit {
			return anchoredInvalid("too many places")
		}
		if part.Unplaced != (len(part.Places) == 0) {
			return anchoredInvalid("the uncommitted part is unplaced exactly when it has no place")
		}
		if part.Partial {
			return anchoredInvalid("the uncommitted part is marked partial; a report holds all of it")
		}
		if len(part.Turns) > anchoredTurnLimit {
			return anchoredInvalid("too many turns")
		}
		// Turns begin at the part's first record and go forward through its
		// records and, with content, through its events.
		for index, turn := range part.Turns {
			ordinal, event := part.FirstOrdinal, part.FirstEvent
			if index > 0 {
				ordinal, event = part.Turns[index-1].FirstOrdinal+1, part.Turns[index-1].FirstEvent
			}
			if (index == 0 && (turn.FirstOrdinal != ordinal || turn.FirstEvent != event)) || turn.FirstOrdinal < ordinal || turn.FirstOrdinal > part.LastOrdinal ||
				turn.FirstEvent < event || turn.FirstEvent > part.FirstEvent+part.Events {
				return anchoredInvalid("turn %d of the uncommitted part is out of place", index+1)
			}
		}
		for _, place := range part.Places {
			if !anchoredText(place.Worktree, anchoredNameLimit, true) || strings.ContainsAny(place.Worktree, `/\`) || !anchoredText(place.Branch, anchoredNameLimit, false) ||
				!anchoredOneOf(place.Rule, AnchoredPlaceRecorded, AnchoredPlaceHeadLog, AnchoredPlaceCurrentBranch, AnchoredPlaceRemovedFolder) {
				return anchoredInvalid("a place needs a worktree name, not a path, and a known rule")
			}
		}
		next = part.LastOrdinal + 1
	}
	if next != b.Records {
		return anchoredInvalid("segments and the uncommitted part cover %d records, the session has %d", next, b.Records)
	}
	// Without content no range claims an event; with it the ranges cover the
	// document exactly, in order.
	if events := 0; b.Content != nil {
		events = b.Content.Events
		if nextEvent != events {
			return anchoredInvalid("the ranges cover %d events, the content has %d", nextEvent, events)
		}
	} else if nextEvent != 0 {
		return anchoredInvalid("a range names events, the report names no content")
	}
	if len(b.AgentCalls) > anchoredAgentCallLimit {
		return anchoredInvalid("too many agent calls")
	}
	for _, call := range b.AgentCalls {
		if !anchoredText(call.Tool, anchoredNameLimit, true) || !anchoredText(call.From, anchoredNameLimit, true) || len(call.To) > anchoredCommitLimit ||
			!anchoredText(call.Status, anchoredNameLimit, false) || !anchoredText(call.Record, anchoredNameLimit, false) {
			return anchoredInvalid("an agent call needs a tool and a sender")
		}
		for _, receiver := range call.To {
			if !anchoredText(receiver, anchoredNameLimit, true) {
				return anchoredInvalid("an agent call names an empty receiver")
			}
		}
	}
	return nil
}

// Canonical returns the report in the one form it is stored and hashed in:
// times in UTC, and no nil slices where a list is always present.
func (b AnchoredSessionBody) Canonical() AnchoredSessionBody {
	utc := func(at *time.Time) *time.Time {
		if at == nil {
			return nil
		}
		value := at.UTC()
		return &value
	}
	out := b
	out.StartedAt, out.EndedAt = utc(b.StartedAt), utc(b.EndedAt)
	out.CopiedFrom = append([]string(nil), b.CopiedFrom...)
	out.Segments = make([]AnchoredSegment, len(b.Segments))
	for index, segment := range b.Segments {
		segment.StartedAt, segment.EndedAt = utc(segment.StartedAt), utc(segment.EndedAt)
		commits := make([]AnchoredCommit, len(segment.Commits))
		for position, commit := range segment.Commits {
			commit.CreatedAt, commit.AuthoredAt = utc(commit.CreatedAt), utc(commit.AuthoredAt)
			commits[position] = commit
		}
		segment.Commits = commits
		out.Segments[index] = segment
	}
	if b.Spawn != nil {
		spawn := *b.Spawn
		out.Spawn = &spawn
	}
	if b.Content != nil {
		content := *b.Content
		out.Content = &content
	}
	if b.Uncommitted != nil {
		part := *b.Uncommitted
		part.StartedAt, part.EndedAt = utc(part.StartedAt), utc(part.EndedAt)
		part.Places = append([]AnchoredPlace{}, part.Places...)
		part.Turns = nil
		for _, turn := range b.Uncommitted.Turns {
			turn.StartedAt = utc(turn.StartedAt)
			part.Turns = append(part.Turns, turn)
		}
		out.Uncommitted = &part
	}
	out.AgentCalls = nil
	for _, call := range b.AgentCalls {
		call.At = utc(call.At)
		call.To = append([]string{}, call.To...)
		out.AgentCalls = append(out.AgentCalls, call)
	}
	return out
}

// Hash identifies the content of a report.
func (b AnchoredSessionBody) Hash() (ContentHash, error) {
	encoded, err := json.Marshal(b.Canonical())
	if err != nil {
		return "", err
	}
	return HashContent(encoded), nil
}

// CommitLinks lists the commit index entries a report implies.
func (b AnchoredSessionBody) CommitLinks(provider ProviderKind, sessionID string) []AnchoredCommitLink {
	var links []AnchoredCommitLink
	for index, segment := range b.Segments {
		for _, commit := range segment.Commits {
			links = append(links, AnchoredCommitLink{Provider: provider, SessionID: sessionID, CommitSHA: commit.SHA, SegmentNo: index + 1, Rule: commit.Rule})
		}
	}
	return links
}

// Summary lists a stored report without its ranges.
func (s AnchoredSession) Summary() AnchoredSessionSummary {
	body := s.Body
	summary := AnchoredSessionSummary{Provider: s.Provider, SessionID: s.SessionID, Parent: body.Parent, Sub: body.Sub, Revision: s.Revision,
		BodyHash: s.BodyHash, Records: body.Records, Commands: body.Commands, Segments: len(body.Segments), Machine: body.Machine,
		StartedAt: body.StartedAt, EndedAt: body.EndedAt, Content: body.Content != nil, Commits: []AnchoredSummaryCommit{}, Uploader: s.Uploader, UpdatedAt: s.UpdatedAt}
	for index, segment := range body.Segments {
		for _, commit := range segment.Commits {
			summary.Commits = append(summary.Commits, AnchoredSummaryCommit{SHA: commit.SHA, SegmentNo: index + 1, Rule: commit.Rule,
				Subject: commit.Subject, CreatedAt: commit.CreatedAt, AuthoredAt: commit.AuthoredAt})
		}
	}
	if part := body.Uncommitted; part != nil {
		summary.Uncommitted = &AnchoredSummaryUncommitted{Commands: part.Commands, StartedAt: part.StartedAt, EndedAt: part.EndedAt,
			Places: append([]AnchoredPlace{}, part.Places...), Unplaced: part.Unplaced}
	}
	return summary
}

// SpawnedIn places a spawn point in this session as it is cut now. It answers
// false when the point is in none of the session's ranges.
func (b AnchoredSessionBody) SpawnedIn(spawn AnchoredSpawn) (AnchoredSummarySpawn, bool) {
	for index, segment := range b.Segments {
		if spawn.AtOrdinal >= segment.FirstOrdinal && spawn.AtOrdinal <= segment.LastOrdinal {
			return AnchoredSummarySpawn{SegmentNo: index + 1, Rule: spawn.Rule}, true
		}
	}
	if part := b.Uncommitted; part != nil && spawn.AtOrdinal >= part.FirstOrdinal && spawn.AtOrdinal <= part.LastOrdinal {
		return AnchoredSummarySpawn{Uncommitted: true, Rule: spawn.Rule}, true
	}
	return AnchoredSummarySpawn{}, false
}

// OpenView returns the session as another account reads it: only what is
// open. A segment that is not open keeps its place and says nothing else, so
// a segment number means the same to everyone. The uncommitted part is cut to
// its open beginning. Worktree names, the machine label, the agent calls, the
// turn marks and the content document's hash are the uploader's alone; the
// conversation is read through the session, never by that hash. It answers
// false when nothing of the session is open.
func (s AnchoredSession) OpenView(open []AnchoredPublishedRange) (AnchoredSession, bool) {
	if len(open) == 0 {
		return AnchoredSession{}, false
	}
	covered := func(first, last int) bool {
		for _, span := range PublishedOrdinals(open) {
			if span[0] <= first && last < span[1] {
				return true
			}
		}
		return false
	}
	body := s.Body
	view := AnchoredSessionBody{RuleVersion: body.RuleVersion, Sub: body.Sub, Parent: body.Parent, Scope: body.Scope, StartedAt: body.StartedAt,
		Segments: make([]AnchoredSegment, len(body.Segments))}
	if body.Spawn != nil {
		spawn := *body.Spawn
		view.Spawn = &spawn
	}
	shown := false
	for index, segment := range body.Segments {
		if !covered(segment.FirstOrdinal, segment.LastOrdinal) {
			view.Segments[index] = AnchoredSegment{Commits: []AnchoredCommit{}, Hidden: true}
			continue
		}
		segment.Commits = append([]AnchoredCommit{}, segment.Commits...)
		for position := range segment.Commits {
			segment.Commits[position].AuthorEmail = ""
		}
		view.Segments[index] = segment
		view.Records += segment.LastOrdinal - segment.FirstOrdinal + 1
		view.Commands += segment.Commands
		shown = true
	}
	if part := body.Uncommitted; part != nil {
		// The part opens from its beginning; find how far.
		end := part.FirstOrdinal
		for _, span := range PublishedOrdinals(open) {
			if span[0] <= part.FirstOrdinal && part.FirstOrdinal < span[1] {
				end = span[1]
			}
		}
		if end > part.LastOrdinal+1 {
			end = part.LastOrdinal + 1
		}
		if end > part.FirstOrdinal {
			cut := AnchoredUncommitted{AnchoredRange: AnchoredRange{FirstOrdinal: part.FirstOrdinal, LastOrdinal: end - 1, StartedAt: part.StartedAt, FirstEvent: part.FirstEvent},
				Places: []AnchoredPlace{}, Partial: end <= part.LastOrdinal}
			if cut.Partial {
				for _, span := range PublishedEvents(open) {
					if span[0] <= part.FirstEvent && part.FirstEvent <= span[1] && span[1]-part.FirstEvent > cut.Events {
						cut.Events = span[1] - part.FirstEvent
					}
				}
				if cut.Events > part.Events {
					cut.Events = part.Events
				}
			} else {
				cut.First, cut.Last, cut.Commands, cut.EndedAt, cut.Events = part.First, part.Last, part.Commands, part.EndedAt, part.Events
			}
			seen := map[string]struct{}{}
			for _, place := range part.Places {
				if _, dup := seen[place.Branch]; dup || place.Branch == "" {
					continue
				}
				seen[place.Branch] = struct{}{}
				cut.Places = append(cut.Places, AnchoredPlace{Branch: place.Branch, Rule: place.Rule})
			}
			view.Uncommitted = &cut
			view.Records += end - part.FirstOrdinal
			shown = true
		}
	}
	if !shown {
		return AnchoredSession{}, false
	}
	if body.Content != nil {
		view.Content = &AnchoredContent{Events: body.Content.Events}
	}
	out := s
	out.Body = view
	return out, true
}

// OpenSummary lists a session as another account sees it in a list.
func (s AnchoredSession) OpenSummary(open []AnchoredPublishedRange) (AnchoredSessionSummary, bool) {
	view, ok := s.OpenView(open)
	if !ok {
		return AnchoredSessionSummary{}, false
	}
	summary := view.Summary()
	summary.Shared, summary.Segments, summary.Commits = true, 0, []AnchoredSummaryCommit{}
	for index, segment := range view.Body.Segments {
		if segment.Hidden {
			continue
		}
		summary.Segments++
		for _, commit := range segment.Commits {
			summary.Commits = append(summary.Commits, AnchoredSummaryCommit{SHA: commit.SHA, SegmentNo: index + 1, Rule: commit.Rule,
				Subject: commit.Subject, CreatedAt: commit.CreatedAt, AuthoredAt: commit.AuthoredAt})
		}
	}
	return summary, true
}
