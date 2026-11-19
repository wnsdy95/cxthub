package domain

import (
	"sort"
	"strings"
	"time"
	"unicode"
)

// Session segments cut a recorded AI session at the commits it made.
//
// Every input is a record another tool left behind: the session files and
// databases of the coding agents, and Git's own worktrees, HEAD logs and commit
// objects. Nothing here depends on cxt, or one of its hooks, having run when the
// work happened. Reading the same records again gives the same report.

// SegmentLinkRule names the rule that linked a session to a commit.
type SegmentLinkRule string

const (
	// SegmentLinkPrintedNumber: a command's output printed the commit number
	// while the commit was being created.
	SegmentLinkPrintedNumber SegmentLinkRule = "printed-number"
	// SegmentLinkFolderAndTime is the fallback for a commit no session printed:
	// the HEAD log of a worktree records the commit, and a command of the
	// session was running in that worktree at that time.
	SegmentLinkFolderAndTime SegmentLinkRule = "same-folder-and-time"
)

// SessionScopeRule names why a session belongs to the repository being read.
type SessionScopeRule string

const (
	SessionScopeFolder    SessionScopeRule = "working-folder"
	SessionScopeOriginURL SessionScopeRule = "origin-url"
)

// SessionPlaceRule names how the place of an uncommitted part was read.
type SessionPlaceRule string

const (
	SessionPlaceRecorded      SessionPlaceRule = "recorded-by-tool"
	SessionPlaceHeadLog       SessionPlaceRule = "head-log"
	SessionPlaceCurrentBranch SessionPlaceRule = "current-branch"
	SessionPlaceRemovedFolder SessionPlaceRule = "folder-no-longer-a-worktree"
)

// SpawnPointRule names how the point where a session was spawned was found.
type SpawnPointRule string

const (
	SpawnPointToolCall  SpawnPointRule = "tool-call"
	SpawnPointStartTime SpawnPointRule = "start-time"
)

// LocalCommitState is what the local repository alone shows about a commit.
// Pull-request commit lists are not visible locally, so this is an observation,
// not the final answer to where a commit lives now.
type LocalCommitState string

const (
	LocalCommitOnBranch    LocalCommitState = "on-a-branch"
	LocalCommitThroughCopy LocalCommitState = "through-its-copy"
	LocalCommitNotOnBranch LocalCommitState = "not-on-a-branch"
	LocalCommitNoObject    LocalCommitState = "object-not-in-git"
)

const (
	// A commit matches a command when it was created no earlier than two
	// seconds before the command started and no later than three seconds after
	// it ended.
	segmentSlackBeforeMs = 2000
	segmentSlackAfterMs  = 3000
	// SegmentReportVersion identifies the report layout, not a storage format.
	SegmentReportVersion = 1
)

// RecordedCommand is one shell command a tool recorded with its output.
type RecordedCommand struct {
	StartedMs int64
	EndedMs   int64
	// Folder is the working folder the tool recorded for the command.
	Folder string
	// Tokens are the commit-number candidates the output printed.
	Tokens []string
}

// RecordedItem is one message or item of a session, in the tool's own order.
type RecordedItem struct {
	// ID is the identifier the tool gave the item. A copied item keeps it.
	ID string
	// Turn is the tool's own turn unit; empty when the tool recorded none.
	Turn string
	AtMs int64
	// Folder and Branch are set only when the tool recorded them on the item.
	Folder   string
	Branch   string
	Detached bool
	// Commands are the shell commands this item completes.
	Commands []RecordedCommand
}

// RecordedSession is one session as a tool recorded it. A session spawned by
// another session is a session of its own.
type RecordedSession struct {
	Provider ProviderKind
	ID       string
	Sub      bool
	// Parent is the session the tool says spawned this one.
	Parent string
	// SpawnItem is the parent's item that made the recorded spawning call.
	SpawnItem   string
	StartFolder string
	// StartedMs is the creation time the tool recorded, when it recorded one.
	StartedMs int64
	Scope     SessionScopeRule
	// FileCopies counts identical copies of this session's record found
	// elsewhere and read only once.
	FileCopies int
	// Source is where the tool keeps this session's records, for a reader
	// that returns to them for their content: the session file of Claude
	// Code. It never leaves this computer.
	Source string
	Items  []RecordedItem
}

// RecordedAgentCall is one agent-to-agent call a tool recorded. Sender and
// receivers are taken as recorded and never inferred.
type RecordedAgentCall struct {
	Provider ProviderKind
	Tool     string
	From     string
	To       []string
	Status   string
	AtMs     int64
	Item     string
}

// HeadMove is a branch change recorded in a worktree's HEAD log. An empty
// branch means HEAD was detached.
type HeadMove struct {
	AtSec  int64
	Branch string
}

// MadeCommit is a commit a worktree's HEAD log says was created there.
type MadeCommit struct {
	AtSec int64
	SHA   string
}

// RecordedWorktree is one worktree of the repository and its HEAD log.
type RecordedWorktree struct {
	Root string
	// Aliases are other spellings of Root, such as its symlink-free form.
	Aliases []string
	Main    bool
	// Current is the branch checked out now; empty when detached.
	Current string
	// Before is the branch recorded as left by the first move in the HEAD log.
	Before string
	Moves  []HeadMove
	Made   []MadeCommit
}

// RecordedCommit is a commit object Git still has.
type RecordedCommit struct {
	SHA         string
	CreatedSec  int64
	AuthoredSec int64
	AuthorEmail string
	Subject     string
	// OnBranch reports reachability from a local or remote-tracking branch.
	OnBranch bool
}

// GitRecords is what Git recorded about the repository.
type GitRecords struct {
	Root      string
	Origin    string
	Worktrees []RecordedWorktree
	Commits   []RecordedCommit
}

// UnreadSessionRecord reports a record that exists but could not be read. Such
// records are reported, never skipped silently: the record is still there and
// can be read again once the reader understands it.
type UnreadSessionRecord struct {
	Provider ProviderKind `json:"provider,omitempty"`
	Source   string       `json:"source"`
	Reason   string       `json:"reason"`
}

// SessionRecordBatch is what one tool's records gave for a repository.
type SessionRecordBatch struct {
	Sessions []RecordedSession
	Calls    []RecordedAgentCall
	Unread   []UnreadSessionRecord
}

// SegmentInputs is everything a report is computed from.
type SegmentInputs struct {
	Git      GitRecords
	Sessions []RecordedSession
	Calls    []RecordedAgentCall
	Unread   []UnreadSessionRecord
}

// SegmentRange is a part of one session. It names the first and last record by
// the tool's identifiers, so it does not commit to any storage layout.
type SegmentRange struct {
	First     string `json:"first"`
	Last      string `json:"last"`
	Records   int    `json:"records"`
	Commands  int    `json:"commands"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// SegmentCommitLink links a segment to a commit and names the rule used.
type SegmentCommitLink struct {
	SHA  string          `json:"sha"`
	Rule SegmentLinkRule `json:"rule"`
}

// SessionSegment is the part of a session that ends at one or more commits.
type SessionSegment struct {
	SegmentRange
	Commits []SegmentCommitLink `json:"commits"`
	// ClosesTurn is true when the segment runs to the end of the turn because
	// it holds the turn's last commit.
	ClosesTurn bool `json:"closes_turn"`
}

// SessionPlace is a worktree and branch an uncommitted part passed through.
type SessionPlace struct {
	Worktree string `json:"worktree"`
	// Branch is empty when the work was done without a branch.
	Branch  string           `json:"branch,omitempty"`
	Removed bool             `json:"removed,omitempty"`
	Rule    SessionPlaceRule `json:"rule"`
}

// UncommittedPart is the part of a session after its last commit, or all of a
// session that made none. It is not cut further.
type UncommittedPart struct {
	SegmentRange
	Places []SessionPlace `json:"places"`
	// Unplaced is true when no place could be read. None is guessed.
	Unplaced bool `json:"unplaced"`
}

// SegmentedSession is one session with its segments.
type SegmentedSession struct {
	Provider  ProviderKind     `json:"provider"`
	ID        string           `json:"id"`
	Sub       bool             `json:"sub"`
	Parent    string           `json:"parent,omitempty"`
	Scope     SessionScopeRule `json:"scope"`
	StartedAt string           `json:"started_at,omitempty"`
	EndedAt   string           `json:"ended_at,omitempty"`
	Records   int              `json:"records"`
	Commands  int              `json:"commands"`
	// CopiedRecords are records this session holds that belong to an earlier
	// session; they are counted there and nowhere else.
	CopiedRecords int              `json:"copied_records"`
	CopiedFrom    []string         `json:"copied_from,omitempty"`
	FileCopies    int              `json:"file_copies,omitempty"`
	Segments      []SessionSegment `json:"segments"`
	Uncommitted   *UncommittedPart `json:"uncommitted,omitempty"`
}

// SegmentCommitSession names a session linked to a commit.
type SegmentCommitSession struct {
	Provider ProviderKind    `json:"provider"`
	ID       string          `json:"id"`
	Rule     SegmentLinkRule `json:"rule"`
}

// LocalCommitPosition is the local-only view of where a commit is now.
type LocalCommitPosition struct {
	State  LocalCommitState `json:"state"`
	Copies []string         `json:"copies,omitempty"`
}

// SegmentCommit is a commit with the sessions linked to it. A commit a HEAD
// log records with no session linked is listed with no sessions, so the gap
// stays visible.
type SegmentCommit struct {
	SHA       string                 `json:"sha"`
	CreatedAt string                 `json:"created_at,omitempty"`
	MadeIn    []string               `json:"made_in,omitempty"`
	Sessions  []SegmentCommitSession `json:"sessions"`
	Local     LocalCommitPosition    `json:"local"`
}

// SessionSpawn says which session spawned which, and where in the parent.
type SessionSpawn struct {
	Provider      ProviderKind `json:"provider"`
	Parent        string       `json:"parent"`
	Child         string       `json:"child"`
	ParentInScope bool         `json:"parent_in_scope"`
	AtRecord      string       `json:"at_record,omitempty"`
	// ParentSegment is the 1-based segment of the parent holding the point.
	ParentSegment     int            `json:"parent_segment,omitempty"`
	ParentUncommitted bool           `json:"parent_uncommitted,omitempty"`
	Rule              SpawnPointRule `json:"rule,omitempty"`
}

// SessionAgentCall is one recorded agent-to-agent call.
type SessionAgentCall struct {
	Provider ProviderKind `json:"provider"`
	Tool     string       `json:"tool"`
	From     string       `json:"from"`
	To       []string     `json:"to"`
	Status   string       `json:"status,omitempty"`
	At       string       `json:"at,omitempty"`
	Record   string       `json:"record,omitempty"`
}

// SegmentRepository identifies what was read.
type SegmentRepository struct {
	Root      string `json:"root"`
	Origin    string `json:"origin,omitempty"`
	Worktrees int    `json:"worktrees"`
}

// SegmentSessionCounts counts sessions or segments by tool and kind.
type SegmentSessionCounts struct {
	Total     int `json:"total"`
	Claude    int `json:"claude"`
	ClaudeSub int `json:"claude_sub"`
	Codex     int `json:"codex"`
	CodexSub  int `json:"codex_sub"`
}

// SegmentSummary holds the totals of a report.
type SegmentSummary struct {
	Sessions                 SegmentSessionCounts `json:"sessions"`
	SessionsWithCommits      SegmentSessionCounts `json:"sessions_with_commits"`
	SessionsWithoutCommits   SegmentSessionCounts `json:"sessions_without_commits"`
	Segments                 SegmentSessionCounts `json:"segments"`
	SegmentsWithOneCommit    int                  `json:"segments_with_one_commit"`
	SegmentsWithMoreCommits  int                  `json:"segments_with_more_commits"`
	LinkedCommits            int                  `json:"linked_commits"`
	LinkedByPrintedNumber    int                  `json:"linked_by_printed_number"`
	LinkedByFolderAndTime    int                  `json:"linked_by_folder_and_time"`
	CommitsWithOneSession    int                  `json:"commits_with_one_session"`
	CommitsWithMoreSessions  int                  `json:"commits_with_more_sessions"`
	HeadLogCommits           int                  `json:"head_log_commits"`
	HeadLogCommitsNoSession  int                  `json:"head_log_commits_without_session"`
	CopiedRecords            int                  `json:"copied_records"`
	SessionFileCopies        int                  `json:"session_file_copies"`
	Spawns                   int                  `json:"spawns"`
	AgentCalls               int                  `json:"agent_calls"`
	UnreadRecords            int                  `json:"unread_records"`
	UnplacedUncommittedParts int                  `json:"unplaced_uncommitted_parts"`
}

// SegmentReport is the read-only result for one repository.
type SegmentReport struct {
	Version    int                   `json:"version"`
	Repository SegmentRepository     `json:"repository"`
	Summary    SegmentSummary        `json:"summary"`
	Unread     []UnreadSessionRecord `json:"unread"`
	Sessions   []SegmentedSession    `json:"sessions"`
	Commits    []SegmentCommit       `json:"commits"`
	Spawns     []SessionSpawn        `json:"spawns"`
	AgentCalls []SessionAgentCall    `json:"agent_calls"`
}

// CommitNumberTokens returns the words of text that can be a commit number: 7
// to 40 lowercase hexadecimal digits standing alone as a word. Each token is
// returned once, in order of first appearance.
func CommitNumberTokens(text string) []string {
	var tokens []string
	var seen map[string]struct{}
	start, hex := -1, true
	flush := func(end int) {
		if start >= 0 && hex && end-start >= 7 && end-start <= 40 {
			token := text[start:end]
			if _, dup := seen[token]; !dup {
				if seen == nil {
					seen = map[string]struct{}{}
				}
				seen[token] = struct{}{}
				tokens = append(tokens, token)
			}
		}
		start, hex = -1, true
	}
	for index, r := range text {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = index
			}
			if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
				hex = false
			}
			continue
		}
		flush(index)
	}
	flush(len(text))
	return tokens
}

// BranchAt reads the branch a worktree was on at a time from its HEAD log.
// Without a recorded move it falls back to the branch checked out now.
func (w RecordedWorktree) BranchAt(atSec int64) (string, SessionPlaceRule) {
	if len(w.Moves) == 0 {
		return w.Current, SessionPlaceCurrentBranch
	}
	index := sort.Search(len(w.Moves), func(i int) bool { return w.Moves[i].AtSec > atSec })
	if index == 0 {
		return w.Before, SessionPlaceHeadLog
	}
	return w.Moves[index-1].Branch, SessionPlaceHeadLog
}

type segmentLink struct {
	position int
	rule     SegmentLinkRule
}

type segmentSessionState struct {
	record RecordedSession
	// own lists the positions in record.Items this session owns.
	own        []int
	copied     int
	copiedFrom map[int]struct{}
	startedMs  int64
	links      map[string]segmentLink
	segments   []segmentSpan
	// rest is the first owned position after the last segment.
	rest int
}

type segmentSpan struct {
	first, last int
	commits     []string
	closesTurn  bool
}

// SessionRecordRanges names the records behind each range of one session: its
// segments in order, then its uncommitted part. A copied record belongs to
// another session and is in none of them.
type SessionRecordRanges struct {
	Provider ProviderKind
	ID       string
	Source   string
	Ranges   [][]string
}

// BuildSegmentReport applies the linking and cutting rules to the records.
func BuildSegmentReport(in SegmentInputs) SegmentReport {
	report, _ := BuildSegmentReportWithRecords(in)
	return report
}

// BuildSegmentReportWithRecords returns the same report together with the
// record identifiers behind every range, one entry per session in the order
// of the report's sessions.
func BuildSegmentReportWithRecords(in SegmentInputs) (SegmentReport, []SessionRecordRanges) {
	states := make([]*segmentSessionState, len(in.Sessions))
	for index, session := range in.Sessions {
		states[index] = &segmentSessionState{record: session, copiedFrom: map[int]struct{}{}, links: map[string]segmentLink{}}
	}
	// A stable order first, so ties below never depend on input order.
	sort.SliceStable(states, func(i, j int) bool {
		a, b := states[i].record, states[j].record
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Parent < b.Parent
	})
	assignRecordOwners(states)

	roots := newWorktreeRoots(in.Git.Worktrees)
	commits := newCommitIndex(in.Git.Commits)
	printed := linkPrintedNumbers(states, commits)
	linkByFolderAndTime(states, in.Git.Worktrees, roots, printed)
	for _, state := range states {
		state.cut()
	}
	report := assembleSegmentReport(in, states, roots, commits)
	type sessionKey struct {
		provider   ProviderKind
		id, parent string
	}
	byKey := make(map[sessionKey]*segmentSessionState, len(states))
	for _, state := range states {
		byKey[sessionKey{state.record.Provider, state.record.ID, state.record.Parent}] = state
	}
	records := make([]SessionRecordRanges, len(report.Sessions))
	for index, session := range report.Sessions {
		state := byKey[sessionKey{session.Provider, session.ID, session.Parent}]
		ranges := SessionRecordRanges{Provider: session.Provider, ID: session.ID, Source: state.record.Source, Ranges: [][]string{}}
		ids := func(first, last int) []string {
			out := make([]string, 0, last-first+1)
			for position := first; position <= last; position++ {
				out = append(out, state.record.Items[state.own[position]].ID)
			}
			return out
		}
		for _, span := range state.segments {
			ranges.Ranges = append(ranges.Ranges, ids(span.first, span.last))
		}
		if state.rest < len(state.own) {
			ranges.Ranges = append(ranges.Ranges, ids(state.rest, len(state.own)-1))
		}
		records[index] = ranges
	}
	return report, records
}

type recordHolderKey struct {
	provider ProviderKind
	id       string
}

// assignRecordOwners counts every record once. Tools copy the earlier part of
// a conversation into a new session file when a session is forked or resumed,
// keeping each record's identifier and time. A record held by several
// sessions belongs to the one that started first. Because a copy keeps its
// original time, a session starts at its earliest record no other session
// holds; a session whose records were all copied onward starts at its last.
func assignRecordOwners(states []*segmentSessionState) {
	holders := map[recordHolderKey][]int{}
	for index, state := range states {
		seen := map[string]struct{}{}
		for _, item := range state.record.Items {
			if _, dup := seen[item.ID]; dup || item.ID == "" {
				continue
			}
			seen[item.ID] = struct{}{}
			key := recordHolderKey{state.record.Provider, item.ID}
			holders[key] = append(holders[key], index)
		}
	}
	for _, state := range states {
		var unique, last int64
		hasItems, hasUnique := false, false
		for _, item := range state.record.Items {
			if !hasItems || item.AtMs > last {
				last = item.AtMs
			}
			hasItems = true
			alone := item.ID == "" || len(holders[recordHolderKey{state.record.Provider, item.ID}]) == 1
			if alone && (!hasUnique || item.AtMs < unique) {
				unique, hasUnique = item.AtMs, true
			}
		}
		switch {
		case hasUnique:
			state.startedMs = unique
		case hasItems:
			state.startedMs = last
		default:
			state.startedMs = state.record.StartedMs
		}
	}
	for index, state := range states {
		seen := map[string]struct{}{}
		for position, item := range state.record.Items {
			if item.ID == "" {
				state.own = append(state.own, position)
				continue
			}
			if _, dup := seen[item.ID]; dup {
				continue
			}
			seen[item.ID] = struct{}{}
			owner := index
			for _, other := range holders[recordHolderKey{state.record.Provider, item.ID}] {
				// states are sorted, so the lower index wins a tie on time.
				if states[other].startedMs < states[owner].startedMs || (states[other].startedMs == states[owner].startedMs && other < owner) {
					owner = other
				}
			}
			if owner == index {
				state.own = append(state.own, position)
				continue
			}
			state.copied++
			state.copiedFrom[owner] = struct{}{}
		}
	}
}

type worktreeRoots struct {
	paths []string
	index []int
}

func newWorktreeRoots(worktrees []RecordedWorktree) worktreeRoots {
	var roots worktreeRoots
	for index, worktree := range worktrees {
		for _, path := range append([]string{worktree.Root}, worktree.Aliases...) {
			if path = strings.TrimRight(path, "/"); path != "" {
				roots.paths = append(roots.paths, path)
				roots.index = append(roots.index, index)
			}
		}
	}
	return roots
}

// of returns the worktree holding folder, preferring the deepest root, or -1.
func (r worktreeRoots) of(folder string) int {
	best, length := -1, -1
	for index, root := range r.paths {
		if len(root) > length && (folder == root || strings.HasPrefix(folder, root+"/")) {
			best, length = r.index[index], len(root)
		}
	}
	return best
}

// SessionRecordScope tells a reader which sessions belong to the repository:
// a session whose working folder is inside one of the repository's worktrees.
// The one fallback is a remote address the tool recorded that names the same
// origin, for a session whose folder is no longer a worktree.
type SessionRecordScope struct {
	Folders []string
	Origin  string
}

// ScopeOf returns the scope of the repository Git recorded.
func (g GitRecords) ScopeOf() SessionRecordScope {
	return SessionRecordScope{Folders: newWorktreeRoots(g.Worktrees).paths, Origin: g.Origin}
}

// Holds reports whether folder is inside one of the repository's worktrees.
func (s SessionRecordScope) Holds(folder string) bool {
	for _, root := range s.Folders {
		if folder == root || strings.HasPrefix(folder, root+"/") {
			return true
		}
	}
	return false
}

type commitIndex struct {
	commits  []RecordedCommit
	byPrefix map[string][]int
	bySHA    map[string]int
}

func newCommitIndex(commits []RecordedCommit) commitIndex {
	index := commitIndex{commits: commits, byPrefix: map[string][]int{}, bySHA: map[string]int{}}
	for position, commit := range commits {
		if len(commit.SHA) < 7 {
			continue
		}
		if _, dup := index.bySHA[commit.SHA]; dup {
			continue
		}
		index.bySHA[commit.SHA] = position
		index.byPrefix[commit.SHA[:7]] = append(index.byPrefix[commit.SHA[:7]], position)
	}
	return index
}

func withinCommand(command RecordedCommand, atMs int64) bool {
	return command.StartedMs-segmentSlackBeforeMs <= atMs && atMs <= command.EndedMs+segmentSlackAfterMs
}

// linkPrintedNumbers applies the first rule and returns the commits it linked.
func linkPrintedNumbers(states []*segmentSessionState, commits commitIndex) map[string]struct{} {
	printed := map[string]struct{}{}
	for _, state := range states {
		for position, itemIndex := range state.own {
			for _, command := range state.record.Items[itemIndex].Commands {
				for _, token := range command.Tokens {
					if len(token) < 7 {
						continue
					}
					for _, candidate := range commits.byPrefix[token[:7]] {
						commit := commits.commits[candidate]
						if !strings.HasPrefix(commit.SHA, token) || !withinCommand(command, commit.CreatedSec*1000) {
							continue
						}
						if _, linked := state.links[commit.SHA]; !linked {
							state.links[commit.SHA] = segmentLink{position, SegmentLinkPrintedNumber}
						}
						printed[commit.SHA] = struct{}{}
					}
				}
			}
		}
	}
	return printed
}

// linkByFolderAndTime applies the one fallback, only to commits no session
// printed.
func linkByFolderAndTime(states []*segmentSessionState, worktrees []RecordedWorktree, roots worktreeRoots, printed map[string]struct{}) {
	type running struct {
		state    *segmentSessionState
		position int
		command  RecordedCommand
	}
	byWorktree := map[int][]running{}
	for _, state := range states {
		for position, itemIndex := range state.own {
			for _, command := range state.record.Items[itemIndex].Commands {
				if command.Folder == "" {
					continue
				}
				if worktree := roots.of(command.Folder); worktree >= 0 {
					byWorktree[worktree] = append(byWorktree[worktree], running{state, position, command})
				}
			}
		}
	}
	for index, worktree := range worktrees {
		for _, made := range worktree.Made {
			if _, done := printed[made.SHA]; done {
				continue
			}
			for _, candidate := range byWorktree[index] {
				if !withinCommand(candidate.command, made.AtSec*1000) {
					continue
				}
				if _, linked := candidate.state.links[made.SHA]; !linked {
					candidate.state.links[made.SHA] = segmentLink{candidate.position, SegmentLinkFolderAndTime}
				}
			}
		}
	}
}

// cut closes a segment at every command that made a commit. The last commit of
// a turn keeps the rest of that turn; earlier commits of the turn close right
// after their command. A command that made several commits ends one segment
// linked to all of them.
func (s *segmentSessionState) cut() {
	cuts := map[int][]string{}
	for sha, link := range s.links {
		cuts[link.position] = append(cuts[link.position], sha)
	}
	positions := make([]int, 0, len(cuts))
	for position := range cuts {
		positions = append(positions, position)
	}
	sort.Ints(positions)
	begin := 0
	for index, position := range positions {
		turn := s.record.Items[s.own[position]].Turn
		end := position
		for turn != "" && end+1 < len(s.own) && s.record.Items[s.own[end+1]].Turn == turn {
			end++
		}
		closes := turn != "" && (index+1 == len(positions) || positions[index+1] > end)
		if !closes {
			end = position
		}
		shas := cuts[position]
		sort.Strings(shas)
		s.segments = append(s.segments, segmentSpan{first: begin, last: end, commits: shas, closesTurn: closes})
		begin = end + 1
	}
	s.rest = begin
}

func segmentTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func (s *segmentSessionState) span(first, last int) SegmentRange {
	out := SegmentRange{Records: last - first + 1}
	items := s.record.Items
	out.First, out.Last = items[s.own[first]].ID, items[s.own[last]].ID
	startedMs, endedMs := items[s.own[first]].AtMs, items[s.own[first]].AtMs
	for position := first; position <= last; position++ {
		item := items[s.own[position]]
		out.Commands += len(item.Commands)
		if item.AtMs < startedMs {
			startedMs = item.AtMs
		}
		if item.AtMs > endedMs {
			endedMs = item.AtMs
		}
	}
	out.StartedAt, out.EndedAt = segmentTime(startedMs), segmentTime(endedMs)
	return out
}

// places reads where an uncommitted part was worked on. A tool-recorded branch
// is used as is. Otherwise the worktree's HEAD log gives the branch at that
// time, and a worktree with no recorded move gives the branch it is on now. A
// folder outside every worktree is never turned into a guess.
func (s *segmentSessionState) places(first, last int, worktrees []RecordedWorktree, roots worktreeRoots) []SessionPlace {
	var places []SessionPlace
	seen := map[SessionPlace]struct{}{}
	started := strings.TrimRight(s.record.StartFolder, "/")
	add := func(folder string, atMs int64, branch string, detached bool) {
		if folder == "" {
			return
		}
		index := roots.of(folder)
		var place SessionPlace
		switch {
		case index < 0 && s.record.Scope == SessionScopeOriginURL && started != "" && (folder == started || strings.HasPrefix(folder, started+"/")):
			place = SessionPlace{Worktree: started, Removed: true, Rule: SessionPlaceRemovedFolder}
		case index < 0:
			return
		case branch != "" || detached:
			place = SessionPlace{Worktree: worktrees[index].Root, Branch: branch, Rule: SessionPlaceRecorded}
		default:
			name, rule := worktrees[index].BranchAt(atMs / 1000)
			place = SessionPlace{Worktree: worktrees[index].Root, Branch: name, Rule: rule}
		}
		if _, dup := seen[place]; !dup {
			seen[place] = struct{}{}
			places = append(places, place)
		}
	}
	items := s.record.Items
	for position := first; position <= last; position++ {
		item := items[s.own[position]]
		add(item.Folder, item.AtMs, item.Branch, item.Detached)
	}
	if len(places) == 0 {
		add(s.record.StartFolder, items[s.own[first]].AtMs, "", false)
	}
	return places
}

func (s *segmentSessionState) kind() (claude, sub bool) {
	return s.record.Provider == ProviderClaude, s.record.Sub
}

func (c *SegmentSessionCounts) add(claude, sub bool, n int) {
	c.Total += n
	switch {
	case claude && sub:
		c.ClaudeSub += n
	case claude:
		c.Claude += n
	case sub:
		c.CodexSub += n
	default:
		c.Codex += n
	}
}

func assembleSegmentReport(in SegmentInputs, states []*segmentSessionState, roots worktreeRoots, commits commitIndex) SegmentReport {
	report := SegmentReport{
		Version:    SegmentReportVersion,
		Repository: SegmentRepository{Root: in.Git.Root, Origin: in.Git.Origin, Worktrees: len(in.Git.Worktrees)},
		Unread:     append([]UnreadSessionRecord{}, in.Unread...),
		Sessions:   []SegmentedSession{},
		Commits:    []SegmentCommit{},
		Spawns:     []SessionSpawn{},
		AgentCalls: []SessionAgentCall{},
	}
	sort.SliceStable(report.Unread, func(i, j int) bool {
		a, b := report.Unread[i], report.Unread[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Reason < b.Reason
	})
	summary := &report.Summary
	summary.UnreadRecords = len(report.Unread)

	linked := map[string][]SegmentCommitSession{}
	startedMs := make([]int64, 0, len(states))
	for _, state := range states {
		record := state.record
		claude, sub := state.kind()
		session := SegmentedSession{
			Provider: record.Provider, ID: record.ID, Sub: record.Sub, Parent: record.Parent, Scope: record.Scope,
			Records: len(state.own), CopiedRecords: state.copied, FileCopies: record.FileCopies,
			Segments: []SessionSegment{},
		}
		for owner := range state.copiedFrom {
			session.CopiedFrom = append(session.CopiedFrom, states[owner].record.ID)
		}
		sort.Strings(session.CopiedFrom)
		if len(state.own) > 0 {
			whole := state.span(0, len(state.own)-1)
			session.StartedAt, session.EndedAt, session.Commands = whole.StartedAt, whole.EndedAt, whole.Commands
		} else {
			session.StartedAt = segmentTime(record.StartedMs)
		}
		for _, span := range state.segments {
			segment := SessionSegment{SegmentRange: state.span(span.first, span.last), ClosesTurn: span.closesTurn}
			for _, sha := range span.commits {
				rule := state.links[sha].rule
				segment.Commits = append(segment.Commits, SegmentCommitLink{SHA: sha, Rule: rule})
				linked[sha] = append(linked[sha], SegmentCommitSession{Provider: record.Provider, ID: record.ID, Rule: rule})
			}
			session.Segments = append(session.Segments, segment)
			if len(span.commits) == 1 {
				summary.SegmentsWithOneCommit++
			} else {
				summary.SegmentsWithMoreCommits++
			}
		}
		if state.rest < len(state.own) {
			part := &UncommittedPart{SegmentRange: state.span(state.rest, len(state.own)-1)}
			part.Places = state.places(state.rest, len(state.own)-1, in.Git.Worktrees, roots)
			if part.Places == nil {
				part.Places, part.Unplaced = []SessionPlace{}, true
				summary.UnplacedUncommittedParts++
			}
			session.Uncommitted = part
		}
		summary.Sessions.add(claude, sub, 1)
		summary.Segments.add(claude, sub, len(session.Segments))
		if len(session.Segments) > 0 {
			summary.SessionsWithCommits.add(claude, sub, 1)
		} else {
			summary.SessionsWithoutCommits.add(claude, sub, 1)
		}
		summary.CopiedRecords += state.copied
		summary.SessionFileCopies += record.FileCopies
		report.Sessions = append(report.Sessions, session)
		startedMs = append(startedMs, state.startedMs)
	}
	// states are already ordered by tool and identifier; a stable sort by start
	// keeps that order among sessions that started together.
	order := make([]int, len(report.Sessions))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(i, j int) bool { return startedMs[order[i]] < startedMs[order[j]] })
	sessions := make([]SegmentedSession, len(order))
	for index, from := range order {
		sessions[index] = report.Sessions[from]
	}
	report.Sessions = sessions

	report.Commits = assembleSegmentCommits(in.Git.Worktrees, commits, linked, summary)
	report.Spawns = assembleSessionSpawns(states)
	summary.Spawns = len(report.Spawns)
	report.AgentCalls = assembleAgentCalls(in.Calls, states)
	summary.AgentCalls = len(report.AgentCalls)
	return report
}

func assembleSegmentCommits(worktrees []RecordedWorktree, commits commitIndex, linked map[string][]SegmentCommitSession, summary *SegmentSummary) []SegmentCommit {
	madeIn := map[string][]string{}
	madeAt := map[string]int64{}
	for _, worktree := range worktrees {
		for _, made := range worktree.Made {
			if _, known := madeAt[made.SHA]; !known || made.AtSec < madeAt[made.SHA] {
				madeAt[made.SHA] = made.AtSec
			}
			present := false
			for _, root := range madeIn[made.SHA] {
				present = present || root == worktree.Root
			}
			if !present {
				madeIn[made.SHA] = append(madeIn[made.SHA], worktree.Root)
			}
		}
	}
	all := map[string]struct{}{}
	for sha := range linked {
		all[sha] = struct{}{}
	}
	for sha := range madeIn {
		all[sha] = struct{}{}
	}
	type copyKey struct {
		authoredSec int64
		email       string
		subject     string
	}
	copies := map[copyKey][]string{}
	for _, commit := range commits.commits {
		if commit.OnBranch {
			key := copyKey{commit.AuthoredSec, commit.AuthorEmail, commit.Subject}
			copies[key] = append(copies[key], commit.SHA)
		}
	}
	out := make([]SegmentCommit, 0, len(all))
	for sha := range all {
		entry := SegmentCommit{SHA: sha, Sessions: append([]SegmentCommitSession{}, linked[sha]...), MadeIn: madeIn[sha]}
		sort.Strings(entry.MadeIn)
		sort.SliceStable(entry.Sessions, func(i, j int) bool {
			a, b := entry.Sessions[i], entry.Sessions[j]
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			return a.ID < b.ID
		})
		createdSec := madeAt[sha]
		if position, known := commits.bySHA[sha]; known {
			commit := commits.commits[position]
			createdSec = commit.CreatedSec
			switch others := copies[copyKey{commit.AuthoredSec, commit.AuthorEmail, commit.Subject}]; {
			case commit.OnBranch:
				entry.Local.State = LocalCommitOnBranch
			case len(others) > 0:
				entry.Local.State = LocalCommitThroughCopy
				entry.Local.Copies = append([]string{}, others...)
				sort.Strings(entry.Local.Copies)
			default:
				entry.Local.State = LocalCommitNotOnBranch
			}
		} else {
			entry.Local.State = LocalCommitNoObject
		}
		entry.CreatedAt = segmentTime(createdSec * 1000)
		out = append(out, entry)

		if _, recorded := madeIn[sha]; recorded {
			summary.HeadLogCommits++
			if len(entry.Sessions) == 0 {
				summary.HeadLogCommitsNoSession++
			}
		}
		if len(entry.Sessions) == 0 {
			continue
		}
		summary.LinkedCommits++
		// Every link to one commit comes from the same rule: the fallback is
		// used only when no session printed the commit.
		if entry.Sessions[0].Rule == SegmentLinkPrintedNumber {
			summary.LinkedByPrintedNumber++
		} else {
			summary.LinkedByFolderAndTime++
		}
		if len(entry.Sessions) == 1 {
			summary.CommitsWithOneSession++
		} else {
			summary.CommitsWithMoreSessions++
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].SHA < out[j].SHA
	})
	return out
}

// assembleSessionSpawns reports who spawned whom. The point in the parent is
// the parent's item that made the recorded call; when the tool did not record
// one, it is the parent's last record at the time the spawned session started.
func assembleSessionSpawns(states []*segmentSessionState) []SessionSpawn {
	type sessionKey struct {
		provider ProviderKind
		id       string
	}
	byID := map[sessionKey]*segmentSessionState{}
	for _, state := range states {
		key := sessionKey{state.record.Provider, state.record.ID}
		if byID[key] == nil {
			byID[key] = state
		}
	}
	spawns := []SessionSpawn{}
	for _, child := range states {
		if child.record.Parent == "" {
			continue
		}
		spawn := SessionSpawn{Provider: child.record.Provider, Parent: child.record.Parent, Child: child.record.ID}
		if parent := byID[sessionKey{child.record.Provider, child.record.Parent}]; parent != nil {
			spawn.ParentInScope = true
			position, rule := -1, SpawnPointRule("")
			if child.record.SpawnItem != "" {
				for candidate, itemIndex := range parent.own {
					if parent.record.Items[itemIndex].ID == child.record.SpawnItem {
						position, rule = candidate, SpawnPointToolCall
						break
					}
				}
			}
			if position < 0 {
				startedMs := child.record.StartedMs
				if len(child.record.Items) > 0 {
					startedMs = child.record.Items[0].AtMs
					for _, item := range child.record.Items {
						if item.AtMs < startedMs {
							startedMs = item.AtMs
						}
					}
				}
				for candidate, itemIndex := range parent.own {
					if startedMs > 0 && parent.record.Items[itemIndex].AtMs <= startedMs {
						position, rule = candidate, SpawnPointStartTime
					}
				}
			}
			if position >= 0 {
				spawn.AtRecord, spawn.Rule = parent.record.Items[parent.own[position]].ID, rule
				spawn.ParentUncommitted = position >= parent.rest
				for index, span := range parent.segments {
					if position >= span.first && position <= span.last {
						spawn.ParentSegment = index + 1
					}
				}
			}
		}
		spawns = append(spawns, spawn)
	}
	sort.SliceStable(spawns, func(i, j int) bool {
		a, b := spawns[i], spawns[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Parent != b.Parent {
			return a.Parent < b.Parent
		}
		return a.Child < b.Child
	})
	return spawns
}

func assembleAgentCalls(calls []RecordedAgentCall, states []*segmentSessionState) []SessionAgentCall {
	inScope := map[recordHolderKey]struct{}{}
	for _, state := range states {
		inScope[recordHolderKey{state.record.Provider, state.record.ID}] = struct{}{}
	}
	out := []SessionAgentCall{}
	for _, call := range calls {
		_, involved := inScope[recordHolderKey{call.Provider, call.From}]
		for _, receiver := range call.To {
			if _, ok := inScope[recordHolderKey{call.Provider, receiver}]; ok {
				involved = true
			}
		}
		if !involved {
			continue
		}
		to := append([]string{}, call.To...)
		sort.Strings(to)
		out = append(out, SessionAgentCall{Provider: call.Provider, Tool: call.Tool, From: call.From, To: to, Status: call.Status, At: segmentTime(call.AtMs), Record: call.Item})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.At != b.At {
			return a.At < b.At
		}
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Record != b.Record {
			return a.Record < b.Record
		}
		return a.Tool < b.Tool
	})
	return out
}
