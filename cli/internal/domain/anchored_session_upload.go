package domain

import (
	"path/filepath"
	"time"
	"unicode/utf8"
)

// AnchoredSessionRuleVersion names the rule set BuildSegmentReport applies.
// The server keeps it with every report so a report computed by older rules
// can be told apart and replaced.
const AnchoredSessionRuleVersion = 1

// AnchoredRange is a contiguous range of a session's own records. Ordinals
// count the session's own records from zero in the order the tool wrote them.
type AnchoredRange struct {
	First        string `json:"first"`
	Last         string `json:"last"`
	FirstOrdinal int    `json:"first_ordinal"`
	LastOrdinal  int    `json:"last_ordinal"`
	Commands     int    `json:"commands"`
	StartedAt    string `json:"started_at,omitempty"`
	EndedAt      string `json:"ended_at,omitempty"`
	// FirstEvent and Events place the range in the session's content
	// document. Both are zero when the report names no content.
	FirstEvent int `json:"first_event,omitempty"`
	Events     int `json:"events,omitempty"`
}

// AnchoredContent names the document that holds the conversation of the
// session's own records, each stored once, in the order of the ranges.
type AnchoredContent struct {
	Doc      ContentHash      `json:"doc"`
	Identity DocumentIdentity `json:"identity,omitempty"`
	Events   int              `json:"events"`
}

// AnchoredCommit is a commit a segment ends at, with the three values a
// cherry-pick or rebase keeps: the server finds a copy of the commit by them.
type AnchoredCommit struct {
	SHA         string          `json:"sha"`
	Rule        SegmentLinkRule `json:"rule"`
	CreatedAt   string          `json:"created_at,omitempty"`
	AuthoredAt  string          `json:"authored_at,omitempty"`
	AuthorEmail string          `json:"author_email,omitempty"`
	Subject     string          `json:"subject,omitempty"`
}

type AnchoredSegment struct {
	AnchoredRange
	ClosesTurn bool             `json:"closes_turn"`
	Commits    []AnchoredCommit `json:"commits"`
}

// AnchoredPlace names a worktree by its folder name only. The full path stays
// on this computer.
type AnchoredPlace struct {
	Worktree string           `json:"worktree"`
	Branch   string           `json:"branch,omitempty"`
	Removed  bool             `json:"removed,omitempty"`
	Rule     SessionPlaceRule `json:"rule"`
}

type AnchoredUncommitted struct {
	AnchoredRange
	// Turns says where each turn of the part begins among the session's own
	// records and in its content document.
	Turns    []AnchoredTurnMark `json:"turns,omitempty"`
	Places   []AnchoredPlace    `json:"places"`
	Unplaced bool               `json:"unplaced"`
}

// AnchoredTurnMark is where one turn of the uncommitted part begins. The
// server uses the marks to tell which turns had begun when a branch was
// pushed.
type AnchoredTurnMark struct {
	FirstOrdinal int    `json:"first_ordinal"`
	FirstEvent   int    `json:"first_event,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
}

// AnchoredSpawn says where in the parent a session was spawned: the parent's
// record and its position among the parent's own records. The position lets
// the server find the parent's segment whenever the parent is cut again.
type AnchoredSpawn struct {
	AtRecord  string         `json:"at_record"`
	AtOrdinal int            `json:"at_ordinal"`
	Rule      SpawnPointRule `json:"rule"`
}

type AnchoredAgentCall struct {
	Tool   string   `json:"tool"`
	From   string   `json:"from"`
	To     []string `json:"to"`
	Status string   `json:"status,omitempty"`
	At     string   `json:"at,omitempty"`
	Record string   `json:"record,omitempty"`
}

// AnchoredSessionBody is what the server stores for one session. It replaces
// the session's earlier report as a whole.
type AnchoredSessionBody struct {
	RuleVersion   int                  `json:"rule_version"`
	Sub           bool                 `json:"sub"`
	Parent        string               `json:"parent,omitempty"`
	Spawn         *AnchoredSpawn       `json:"spawn,omitempty"`
	Scope         SessionScopeRule     `json:"scope"`
	Machine       string               `json:"machine,omitempty"`
	StartedAt     string               `json:"started_at,omitempty"`
	EndedAt       string               `json:"ended_at,omitempty"`
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

// AttachContent names the content document in the report and places every
// range in it. first and count give, for each range in order (the segments,
// then the uncommitted part turn by turn), where it starts and how many
// events it holds.
func (b *AnchoredSessionBody) AttachContent(ref DocumentRef, events int, first, count []int) bool {
	segments, turns := len(b.Segments), 0
	if b.Uncommitted != nil {
		turns = len(b.Uncommitted.Turns)
		if turns == 0 {
			return false
		}
	}
	if len(first) != segments+turns || len(count) != segments+turns {
		return false
	}
	for index := range b.Segments {
		b.Segments[index].FirstEvent, b.Segments[index].Events = first[index], count[index]
	}
	if b.Uncommitted != nil {
		b.Uncommitted.FirstEvent, b.Uncommitted.Events = first[segments], 0
		for turn := range b.Uncommitted.Turns {
			b.Uncommitted.Turns[turn].FirstEvent = first[segments+turn]
			b.Uncommitted.Events += count[segments+turn]
		}
	}
	b.Content = &AnchoredContent{Doc: ref.Hash, Identity: ref.Identity, Events: events}
	return true
}

// AnchoredSessionUpload is one session's report and the key it is stored under.
type AnchoredSessionUpload struct {
	Provider  ProviderKind
	SessionID string
	Body      AnchoredSessionBody
}

// AnchoredSessionReceipt is the server's answer to one upload. Changed is
// false when the server already held the same report.
type AnchoredSessionReceipt struct {
	Revision int64       `json:"revision"`
	BodyHash ContentHash `json:"body_hash"`
	Changed  bool        `json:"changed"`
}

// AnchoredUploadPlan is everything one run uploads, and what it leaves out.
type AnchoredUploadPlan struct {
	Uploads []AnchoredSessionUpload
	// CallsWithoutSender counts agent calls whose sender is not a session of
	// this repository. A call is uploaded with the session that recorded it,
	// so these are not uploaded.
	CallsWithoutSender int
}

func anchoredCommitTime(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// BuildAnchoredUploadPlan turns a segment report into one upload per session.
// It adds nothing to the report: ranges get the ordinals their record counts
// imply, commits get the facts Git recorded for them, and worktrees are
// reduced to their folder names.
func BuildAnchoredUploadPlan(report SegmentReport, commits []RecordedCommit, machine string) AnchoredUploadPlan {
	facts := make(map[string]RecordedCommit, len(commits))
	for _, commit := range commits {
		facts[commit.SHA] = commit
	}
	type key struct {
		provider ProviderKind
		id       string
	}
	spawns := make(map[key]SessionSpawn, len(report.Spawns))
	for _, spawn := range report.Spawns {
		spawns[key{spawn.Provider, spawn.Child}] = spawn
	}
	calls := map[key][]AnchoredAgentCall{}
	inReport := make(map[key]struct{}, len(report.Sessions))
	for _, session := range report.Sessions {
		inReport[key{session.Provider, session.ID}] = struct{}{}
	}
	plan := AnchoredUploadPlan{Uploads: make([]AnchoredSessionUpload, 0, len(report.Sessions))}
	for _, call := range report.AgentCalls {
		sender := key{call.Provider, call.From}
		if _, ok := inReport[sender]; !ok {
			plan.CallsWithoutSender++
			continue
		}
		calls[sender] = append(calls[sender], AnchoredAgentCall{Tool: call.Tool, From: call.From, To: append([]string{}, call.To...), Status: call.Status, At: call.At, Record: call.Record})
	}
	for _, session := range report.Sessions {
		body := AnchoredSessionBody{
			RuleVersion: AnchoredSessionRuleVersion, Sub: session.Sub, Parent: session.Parent, Scope: session.Scope, Machine: machine,
			StartedAt: session.StartedAt, EndedAt: session.EndedAt, Records: session.Records, Commands: session.Commands,
			CopiedRecords: session.CopiedRecords, CopiedFrom: append([]string(nil), session.CopiedFrom...), FileCopies: session.FileCopies,
			Segments: make([]AnchoredSegment, 0, len(session.Segments)), AgentCalls: calls[key{session.Provider, session.ID}],
		}
		if spawn, ok := spawns[key{session.Provider, session.ID}]; ok && spawn.AtRecord != "" && spawn.AtOrdinal != nil && spawn.Rule != "" {
			body.Spawn = &AnchoredSpawn{AtRecord: spawn.AtRecord, AtOrdinal: *spawn.AtOrdinal, Rule: spawn.Rule}
		}
		next := 0
		span := func(r SegmentRange) AnchoredRange {
			out := AnchoredRange{First: r.First, Last: r.Last, FirstOrdinal: next, LastOrdinal: next + r.Records - 1, Commands: r.Commands, StartedAt: r.StartedAt, EndedAt: r.EndedAt}
			next += r.Records
			return out
		}
		for _, segment := range session.Segments {
			out := AnchoredSegment{AnchoredRange: span(segment.SegmentRange), ClosesTurn: segment.ClosesTurn, Commits: make([]AnchoredCommit, 0, len(segment.Commits))}
			for _, link := range segment.Commits {
				commit := AnchoredCommit{SHA: link.SHA, Rule: link.Rule}
				if fact, ok := facts[link.SHA]; ok {
					commit.CreatedAt, commit.AuthoredAt = anchoredCommitTime(fact.CreatedSec), anchoredCommitTime(fact.AuthoredSec)
					commit.AuthorEmail, commit.Subject = fact.AuthorEmail, AnchoredCommitSubject(fact.Subject)
				}
				out.Commits = append(out.Commits, commit)
			}
			body.Segments = append(body.Segments, out)
		}
		if part := session.Uncommitted; part != nil {
			out := AnchoredUncommitted{AnchoredRange: span(part.SegmentRange), Places: make([]AnchoredPlace, 0, len(part.Places)), Unplaced: part.Unplaced}
			for _, turn := range part.Turns {
				out.Turns = append(out.Turns, AnchoredTurnMark{FirstOrdinal: turn.Ordinal, StartedAt: turn.StartedAt})
			}
			seen := map[AnchoredPlace]struct{}{}
			for _, place := range part.Places {
				named := AnchoredPlace{Worktree: filepath.Base(place.Worktree), Branch: place.Branch, Removed: place.Removed, Rule: place.Rule}
				// Two worktrees with one folder name are one name on the server.
				if _, dup := seen[named]; dup {
					continue
				}
				seen[named] = struct{}{}
				out.Places = append(out.Places, named)
			}
			body.Uncommitted = &out
		}
		plan.Uploads = append(plan.Uploads, AnchoredSessionUpload{Provider: session.Provider, SessionID: session.ID, Body: body})
	}
	return plan
}

// ValidAnchoredSessionID reports whether a tool's session identifier can be
// used as a key on the server. One that cannot is reported, never rewritten.
func ValidAnchoredSessionID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// SegmentPushState is what happened to one session's report.
type SegmentPushState string

const (
	SegmentPushStored    SegmentPushState = "stored"
	SegmentPushUnchanged SegmentPushState = "unchanged"
	SegmentPushRefused   SegmentPushState = "refused"
	// SegmentPushRemoved means the session was removed from the server and
	// not allowed again: nothing of it was read back or sent.
	SegmentPushRemoved SegmentPushState = "removed"
)

// SegmentContentState is what happened to the conversation of one session.
type SegmentContentState string

const (
	// SegmentContentStored: the content document is on the server and the
	// report names it.
	SegmentContentStored SegmentContentState = "stored"
	// SegmentContentNone: the session's own records hold no conversation.
	SegmentContentNone SegmentContentState = "none"
	// SegmentContentNotUploaded: the report was uploaded without content.
	SegmentContentNotUploaded SegmentContentState = "not-uploaded"
)

type SegmentPushSession struct {
	Provider ProviderKind     `json:"provider"`
	ID       string           `json:"id"`
	State    SegmentPushState `json:"state"`
	Revision int64            `json:"revision,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	// Content is empty when this run uploads no content at all.
	Content       SegmentContentState `json:"content,omitempty"`
	ContentReason string              `json:"content_reason,omitempty"`
	// Summaries counts the summaries this run stored for the session's
	// commits. SummaryReason says why one was not stored.
	Summaries     int    `json:"summaries,omitempty"`
	SummaryReason string `json:"summary_reason,omitempty"`
}

// SegmentPushResult lists every session of the report with what happened to
// it, and what the report itself could not cover.
type SegmentPushResult struct {
	Version int `json:"version"`
	// Automatic is true for a run nobody asked for. Such a run leaves out
	// BeforeAutomatic sessions: all their records lie before automatic
	// reading began in this repository.
	Automatic       bool `json:"automatic,omitempty"`
	BeforeAutomatic int  `json:"before_automatic,omitempty"`
	Stored          int  `json:"stored"`
	Unchanged       int  `json:"unchanged"`
	Refused         int  `json:"refused"`
	// Removed counts sessions that were removed from the server and are
	// therefore not sent. It is no failure.
	Removed int `json:"removed,omitempty"`
	// ContentNotUploaded counts sessions whose report went up without their
	// conversation; each one says why.
	ContentNotUploaded int `json:"content_not_uploaded"`
	// Summaries counts the summaries this run stored: one for a session at
	// each commit it made. It is absent when the run writes no summaries.
	Summaries int                  `json:"summaries,omitempty"`
	Sessions  []SegmentPushSession `json:"sessions"`
	// Unread records exist but were not read, so no report covers them.
	Unread []UnreadSessionRecord `json:"unread"`
	// AgentCallsWithoutSender were recorded by a session outside this
	// repository and are uploaded with no session.
	AgentCallsWithoutSender int `json:"agent_calls_without_sender"`
	// Branches is absent when this run does not handle branch IDs.
	Branches *SegmentPushBranches `json:"branches,omitempty"`
	// Worktrees is absent when this run does not report the worktree list.
	Worktrees *SegmentPushWorktrees `json:"worktrees,omitempty"`
	// LocalRecords is absent when this run keeps no copy of what it sends.
	LocalRecords *SegmentPushLocalRecords `json:"local_records,omitempty"`
	// NotFound names sessions the run was asked for that the tools' records
	// on this computer do not hold. It is set only by a run for named
	// sessions.
	NotFound []SessionUploadKey `json:"not_found,omitempty"`
}

// SegmentPushLocalRecords says what a run kept in the repository's .cxt
// folder: for each session the report and the conversation as they were sent
// and a log of the uploads. Keeping them never stops the run: a failure is
// said here and the next run tries again.
type SegmentPushLocalRecords struct {
	// Kept counts the sessions whose record is up to date after this run.
	Kept int `json:"kept"`
	// NotKept says why a record was not written.
	NotKept string `json:"not_kept,omitempty"`
}

// SegmentPushBranches says what a run did about branch IDs. They are handled
// before the sessions and never stop them: a failure is said here and the
// next run tries again.
type SegmentPushBranches struct {
	// WithID counts the repository's branches that have an ID after this run;
	// Assigned of them got theirs in this run.
	WithID   int `json:"with_id"`
	Assigned int `json:"assigned"`
	// OnOrigin counts those Git's records show on origin; Registered of them
	// the server now knows.
	OnOrigin   int `json:"on_origin"`
	Registered int `json:"registered"`
	// NotAssigned and NotRegistered say why a step did not happen.
	NotAssigned   string `json:"not_assigned,omitempty"`
	NotRegistered string `json:"not_registered,omitempty"`
}

// AnchoredCommitSubjectLimit bounds a commit subject in a report, in bytes.
// The server keeps the subjects it reads from the hosting provider to the
// same length, cut the same way, so that a copy of a commit is still found by
// its author time, author and subject.
const AnchoredCommitSubjectLimit = 1024

// AnchoredCommitSubject cuts a subject to the limit at the start of a
// character.
func AnchoredCommitSubject(subject string) string {
	if len(subject) <= AnchoredCommitSubjectLimit {
		return subject
	}
	cut := AnchoredCommitSubjectLimit
	for cut > 0 && !utf8.RuneStart(subject[cut]) {
		cut--
	}
	return subject[:cut]
}

// AnchoredSessionRemoval says that a session was removed from the server.
// While it stands the server takes no report for the session. ByYou is false
// when someone who manages the repository removed it.
type AnchoredSessionRemoval struct {
	Provider  ProviderKind `json:"provider"`
	SessionID string       `json:"session_id"`
	RemovedAt string       `json:"removed_at"`
	ByYou     bool         `json:"by_you"`
}
