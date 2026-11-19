package domain

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Remote git is what the server itself read from a repository's Git hosting
// provider: commits, branch heads and pull requests. Nothing in it comes from
// a client. It is a cache of one origin: when the repository's origin changes
// the cache starts over.

const (
	RemotePullOpen   = "open"
	RemotePullClosed = "closed"
	RemotePullMerged = "merged"

	// RemoteCommitSubjectLimit bounds a stored subject, in bytes.
	RemoteCommitSubjectLimit = 1024
	// RemotePullCommitLimit is where the provider's commit list of a pull
	// request stops.
	RemotePullCommitLimit = 250
	// RemoteGitUnreadableLimit bounds the commits remembered as refused.
	RemoteGitUnreadableLimit = 256
	// RemoteGitPageDefault and RemoteGitPageLimit bound a page of the commit
	// list a member reads.
	RemoteGitPageDefault = 200
	RemoteGitPageLimit   = 500

	// RemoteGitSyncEvery is the time from the start of a pass that left
	// nothing to read to the next pass.
	RemoteGitSyncEvery = 5 * time.Minute
	// RemoteGitSyncContinue is that time after a pass that ran out of budget.
	RemoteGitSyncContinue = time.Minute
	// RemoteGitSyncRetry is that time after a pass that failed.
	RemoteGitSyncRetry = 2 * time.Minute
	// RemoteGitRequestSpacing is the least time from the start of a pass to a
	// pass somebody asked for.
	RemoteGitRequestSpacing = 30 * time.Second

	remoteGitTextLimit   = 1024
	remoteGitOriginLimit = 2048
	remoteGitParentLimit = 256
)

var remoteCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

// ValidRemoteCommitSHA reports whether sha is a full commit number.
func ValidRemoteCommitSHA(sha string) bool { return remoteCommitSHA.MatchString(sha) }

func remoteGitInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrValidation}, args...)...)
}

func remoteGitText(value string, required bool) bool {
	if value == "" {
		return !required
	}
	return len(value) <= remoteGitTextLimit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// ValidRemoteBranchName reports whether name can be stored as the name of a
// branch on the remote. The provider decides what a branch name is; the
// server only refuses what it cannot keep.
func ValidRemoteBranchName(name string) bool { return remoteGitText(name, true) }

// remoteGitSecond reports whether t is a set time in UTC without a fraction of
// a second, which is what Git records for a commit.
func remoteGitSecond(t time.Time) bool {
	_, offset := t.Zone()
	return !t.IsZero() && offset == 0 && t.Nanosecond() == 0
}

// RemoteCommit is a commit as the provider lists it.
type RemoteCommit struct {
	SHA         string    `json:"sha"`
	Parents     []string  `json:"parents"`
	AuthorName  string    `json:"author_name"`
	AuthorEmail string    `json:"author_email"`
	AuthoredAt  time.Time `json:"authored_at"`
	CommittedAt time.Time `json:"committed_at"`
	Subject     string    `json:"subject"`
}

// Validate checks a commit before it is stored.
func (c RemoteCommit) Validate() error {
	if !ValidRemoteCommitSHA(c.SHA) {
		return remoteGitInvalid("commit number")
	}
	if len(c.Parents) > remoteGitParentLimit {
		return remoteGitInvalid("commit %s names too many parents", c.SHA[:12])
	}
	for _, parent := range c.Parents {
		if !ValidRemoteCommitSHA(parent) {
			return remoteGitInvalid("commit %s names a parent without a full number", c.SHA[:12])
		}
	}
	if !remoteGitText(c.AuthorName, false) || !remoteGitText(c.AuthorEmail, false) {
		return remoteGitInvalid("commit %s author", c.SHA[:12])
	}
	if !remoteGitSecond(c.AuthoredAt) || !remoteGitSecond(c.CommittedAt) {
		return remoteGitInvalid("commit %s has both times, in UTC to the second", c.SHA[:12])
	}
	if len(c.Subject) > RemoteCommitSubjectLimit || !remoteGitText(c.Subject, false) {
		return remoteGitInvalid("commit %s subject", c.SHA[:12])
	}
	return nil
}

// RemoteCommitKey is what a rebase or a cherry-pick keeps of a commit: two
// commits with one key are copies of each other.
type RemoteCommitKey struct {
	AuthoredAt  time.Time `json:"authored_at"`
	AuthorEmail string    `json:"author_email"`
	Subject     string    `json:"subject"`
}

// Key returns what a copy of the commit would keep.
func (c RemoteCommit) Key() RemoteCommitKey {
	return RemoteCommitKey{AuthoredAt: c.AuthoredAt.UTC(), AuthorEmail: c.AuthorEmail, Subject: c.Subject}
}

// Validate checks a key before it is looked up.
func (k RemoteCommitKey) Validate() error {
	if !remoteGitSecond(k.AuthoredAt) || !remoteGitText(k.AuthorEmail, false) ||
		len(k.Subject) > RemoteCommitSubjectLimit || !remoteGitText(k.Subject, false) {
		return remoteGitInvalid("commit key")
	}
	return nil
}

// RemoteCommitSubject returns the subject of a commit message as Git prints
// it for %s: the lines before the first blank line, after any leading blank
// lines, each without its trailing whitespace, joined with one space. The
// result is cut to RemoteCommitSubjectLimit bytes on a character boundary.
func RemoteCommitSubject(message string) string {
	const blank = " \t\r\n"
	var subject strings.Builder
	started := false
	for message != "" {
		line := message
		if end := strings.IndexByte(message, '\n'); end >= 0 {
			line, message = message[:end], message[end+1:]
		} else {
			message = ""
		}
		line = strings.TrimRight(line, blank)
		if line == "" {
			if started {
				break
			}
			continue
		}
		if started {
			subject.WriteByte(' ')
		}
		subject.WriteString(line)
		started = true
	}
	out := subject.String()
	if len(out) > RemoteCommitSubjectLimit {
		cut := RemoteCommitSubjectLimit
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}

// RemoteCommitCursor is the place a page of the commit list ended: the last
// commit a page held. The zero cursor is the top of the list.
type RemoteCommitCursor struct {
	CommittedAt time.Time
	SHA         string
}

// IsZero reports whether the cursor is the top of the list.
func (c RemoteCommitCursor) IsZero() bool { return c.SHA == "" && c.CommittedAt.IsZero() }

// RemoteCommitCursorAt returns the cursor of a page that ended at commit.
func RemoteCommitCursorAt(commit RemoteCommit) RemoteCommitCursor {
	return RemoteCommitCursor{CommittedAt: commit.CommittedAt.UTC(), SHA: commit.SHA}
}

// String returns the cursor in the form clients carry it. The form is not
// part of any contract: clients send it back as they got it.
func (c RemoteCommitCursor) String() string {
	if c.IsZero() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.CommittedAt.Unix(), 10) + "." + c.SHA))
}

// ParseRemoteCommitCursor reads a cursor a client sent back. An empty cursor
// is the top of the list.
func ParseRemoteCommitCursor(raw string) (RemoteCommitCursor, error) {
	if raw == "" {
		return RemoteCommitCursor{}, nil
	}
	if len(raw) > 256 {
		return RemoteCommitCursor{}, remoteGitInvalid("commit list cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return RemoteCommitCursor{}, remoteGitInvalid("commit list cursor")
	}
	seconds, sha, found := strings.Cut(string(decoded), ".")
	at, err := strconv.ParseInt(seconds, 10, 64)
	// The year bound keeps the time inside what both stores can hold.
	if !found || err != nil || !ValidRemoteCommitSHA(sha) || at < -62135596800 || at > 253402300799 {
		return RemoteCommitCursor{}, remoteGitInvalid("commit list cursor")
	}
	cursor := RemoteCommitCursor{CommittedAt: time.Unix(at, 0).UTC(), SHA: sha}
	if cursor.String() != raw {
		return RemoteCommitCursor{}, remoteGitInvalid("commit list cursor")
	}
	return cursor, nil
}

// RemoteBranch is a branch name on the remote and the commit it points at.
type RemoteBranch struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// Validate checks a branch before it is stored.
func (b RemoteBranch) Validate() error {
	if !ValidRemoteBranchName(b.Name) || !ValidRemoteCommitSHA(b.SHA) {
		return remoteGitInvalid("remote branch")
	}
	return nil
}

// RemotePull is a pull request as the provider lists it, with its commits.
type RemotePull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"`
	HeadRef string `json:"head_ref"`
	HeadSHA string `json:"head_sha"`
	BaseRef string `json:"base_ref"`
	// MergeSHA is the commit the pull request became, only when it is merged.
	MergeSHA string `json:"merge_sha,omitempty"`
	// Fork: the head lives in another repository.
	Fork bool `json:"fork"`
	// Commits are the commit numbers as the provider lists them, oldest first.
	Commits []string `json:"commits"`
	// Truncated: the provider's list stops at RemotePullCommitLimit commits.
	Truncated bool      `json:"truncated"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks a pull request before it is stored.
func (p RemotePull) Validate() error {
	if p.Number < 1 {
		return remoteGitInvalid("pull request number")
	}
	if p.State != RemotePullOpen && p.State != RemotePullClosed && p.State != RemotePullMerged {
		return remoteGitInvalid("pull request %d state", p.Number)
	}
	if !remoteGitText(p.Title, false) || !ValidRemoteBranchName(p.HeadRef) || !ValidRemoteBranchName(p.BaseRef) || !ValidRemoteCommitSHA(p.HeadSHA) {
		return remoteGitInvalid("pull request %d title, head or base", p.Number)
	}
	if p.MergeSHA != "" && (p.State != RemotePullMerged || !ValidRemoteCommitSHA(p.MergeSHA)) {
		return remoteGitInvalid("pull request %d names a merge commit only when it is merged", p.Number)
	}
	if len(p.Commits) > RemotePullCommitLimit || (p.Truncated && len(p.Commits) != RemotePullCommitLimit) {
		return remoteGitInvalid("pull request %d commit list", p.Number)
	}
	for _, sha := range p.Commits {
		if !ValidRemoteCommitSHA(sha) {
			return remoteGitInvalid("pull request %d commit list", p.Number)
		}
	}
	if !remoteGitSecond(p.UpdatedAt) {
		return remoteGitInvalid("pull request %d has its update time, in UTC to the second", p.Number)
	}
	return nil
}

// RemoteGitState is the reading state of one repository's cache.
type RemoteGitState struct {
	RepoID ContentHash `json:"repo_id"`
	// GitOrigin is the origin this cache was read from.
	GitOrigin     string `json:"git_origin"`
	DefaultBranch string `json:"default_branch,omitempty"`
	// SyncedAt is the start of the last pass that finished without error.
	SyncedAt *time.Time `json:"synced_at,omitempty"`
	// RequestedAt is the last time a fresh read was asked for.
	RequestedAt *time.Time `json:"requested_at,omitempty"`
	// AttemptedAt is the start of the last pass.
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	// NextAt is the earliest time of the next pass.
	NextAt time.Time `json:"next_at"`
	// PullPage is the next page of the first full pull-request read. It means
	// something only while PullsComplete is false.
	PullPage      int  `json:"pull_page,omitempty"`
	PullsComplete bool `json:"pulls_complete"`
	// Remaining: the last pass ran out of budget.
	Remaining bool `json:"remaining"`
	// Unreadable are commits the provider refused to list. The frontier
	// skips them, so that they are not asked for again on every pass.
	Unreadable []string `json:"unreadable,omitempty"`
}

// NewRemoteGitState returns the state of a cache nothing was read into yet.
// Its first pass is due at once.
func NewRemoteGitState(repo ContentHash, origin string, now time.Time) RemoteGitState {
	return RemoteGitState{RepoID: repo, GitOrigin: origin, NextAt: now.UTC(), PullPage: 1}
}

// Validate checks a state before it is stored.
func (s RemoteGitState) Validate() error {
	if ValidateContentHash(s.RepoID) != nil {
		return remoteGitInvalid("remote git repository")
	}
	if s.GitOrigin == "" || len(s.GitOrigin) > remoteGitOriginLimit || (s.DefaultBranch != "" && !ValidRemoteBranchName(s.DefaultBranch)) {
		return remoteGitInvalid("remote git origin or default branch")
	}
	if s.NextAt.IsZero() || s.PullPage < 0 || len(s.Unreadable) > RemoteGitUnreadableLimit {
		return remoteGitInvalid("remote git reading state")
	}
	for _, at := range []*time.Time{s.SyncedAt, s.RequestedAt, s.AttemptedAt} {
		if at != nil && at.IsZero() {
			return remoteGitInvalid("remote git reading state")
		}
	}
	for _, sha := range s.Unreadable {
		if !ValidRemoteCommitSHA(sha) {
			return remoteGitInvalid("remote git reading state")
		}
	}
	return nil
}

// Due is the one scheduling rule: a pass is due when the time of the next
// pass has come and no claim is still held. claimedUntil is the end of the
// last claim; the zero time when there is none.
func (s RemoteGitState) Due(now, claimedUntil time.Time) bool {
	return !now.Before(s.NextAt) && !now.Before(claimedUntil)
}

// ClaimRemoteGitPass applies the rule to a worker asking to read origin at
// now. stored is the state the store holds, nil when the repository was never
// read. It returns the state the pass starts with and whether the pass may
// start. startOver is true when what the store holds was read from another
// origin and has to be removed first.
func ClaimRemoteGitPass(stored *RemoteGitState, claimedUntil time.Time, repo ContentHash, origin string, now time.Time) (state RemoteGitState, startOver, ok bool) {
	switch {
	case stored == nil:
		return NewRemoteGitState(repo, origin, now).Started(now), false, true
	case now.Before(claimedUntil):
		return RemoteGitState{}, false, false
	case stored.GitOrigin != origin:
		return NewRemoteGitState(repo, origin, now).Started(now), true, true
	case !stored.Due(now, claimedUntil):
		return RemoteGitState{}, false, false
	}
	return stored.Started(now), false, true
}

// Started returns the state of a cache whose pass starts at now.
func (s RemoteGitState) Started(now time.Time) RemoteGitState {
	now = now.UTC()
	s.AttemptedAt = &now
	return s
}

// SamePass reports whether other is the state of the pass this state belongs
// to: the same origin, started at the same time.
func (s RemoteGitState) SamePass(other RemoteGitState) bool {
	return s.GitOrigin == other.GitOrigin && s.AttemptedAt != nil && other.AttemptedAt != nil && s.AttemptedAt.Equal(*other.AttemptedAt)
}

// Ended returns the state after the pass that started at start. A pass that
// failed keeps what it stored and does not count as a read. remaining says
// that a pass without error ran out of budget with work left.
//
// A read asked for while the pass ran may have come after the pass read what
// was asked about, so it still pulls the next pass closer.
func (s RemoteGitState) Ended(start time.Time, failed, remaining bool) RemoteGitState {
	start = start.UTC()
	switch {
	case failed:
		s.NextAt = start.Add(RemoteGitSyncRetry)
	case remaining:
		s.SyncedAt, s.Remaining, s.NextAt = &start, true, start.Add(RemoteGitSyncContinue)
	default:
		s.SyncedAt, s.Remaining, s.NextAt = &start, false, start.Add(RemoteGitSyncEvery)
	}
	if s.RequestedAt != nil && s.RequestedAt.After(start) {
		s.NextAt = remoteGitRequested(s.NextAt, &start, *s.RequestedAt)
	}
	return s
}

// Request returns the state after a fresh read was asked for at now. The next
// pass is pulled closer, and never closer to the start of the last pass than
// RemoteGitRequestSpacing.
func (s RemoteGitState) Request(now time.Time) RemoteGitState {
	now = now.UTC()
	s.RequestedAt = &now
	s.NextAt = remoteGitRequested(s.NextAt, s.AttemptedAt, now)
	return s
}

func remoteGitRequested(next time.Time, attempted *time.Time, now time.Time) time.Time {
	earliest := now
	if attempted != nil && attempted.Add(RemoteGitRequestSpacing).After(now) {
		earliest = attempted.Add(RemoteGitRequestSpacing)
	}
	if earliest.Before(next) {
		return earliest
	}
	return next
}

// WithUnreadable returns the state remembering that the provider refused to
// list sha. The newest RemoteGitUnreadableLimit refusals are kept.
func (s RemoteGitState) WithUnreadable(sha string) RemoteGitState {
	kept := make([]string, 0, len(s.Unreadable)+1)
	for _, known := range s.Unreadable {
		if known != sha {
			kept = append(kept, known)
		}
	}
	kept = append(kept, sha)
	if len(kept) > RemoteGitUnreadableLimit {
		kept = kept[len(kept)-RemoteGitUnreadableLimit:]
	}
	s.Unreadable = kept
	return s
}

// RemoteGitPage is one read of the cache: the default branch, the branches
// and the pull requests on the first page, and a page of the commit list,
// newest first. Next is where the following page starts, nil on the last.
type RemoteGitPage struct {
	DefaultBranch string
	SyncedAt      *time.Time
	Branches      []RemoteBranch
	Pulls         []RemotePull
	Commits       []RemoteCommit
	Next          *RemoteCommitCursor
}
