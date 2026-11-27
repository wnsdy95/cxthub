package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// A summary is what a CLI wrote down of a session when the session made a
// commit: what was done, the facts worth keeping, and what is left to do. It
// belongs to one session and one commit a segment of that session ends at.
// A summary is made to be shared, so a session that made no commit has none.
// It is read under the rule of its segment: by the uploader always, by anyone
// else once the commit is pushed.

const (
	// AnchoredSummaryBodyLimit is the most bytes a summary's request holds.
	AnchoredSummaryBodyLimit = 512 << 10

	anchoredSummaryTextLimit  = 64 << 10
	anchoredSummaryItemLimit  = 4 << 10
	anchoredSummaryItemsLimit = 256
)

// AnchoredSummaryBody is the text of a summary.
type AnchoredSummaryBody struct {
	Summary   string   `json:"summary"`
	KeyFacts  []string `json:"key_facts"`
	OpenTasks []string `json:"open_tasks"`
	// TasksAuthoritative says that the open tasks are the tool's own list,
	// taken from the summary the tool wrote of its conversation, and not
	// what a CLI picked out of the conversation. Only such a list is given
	// to another session as what is left to do.
	TasksAuthoritative bool `json:"tasks_authoritative,omitempty"`
}

// Canonical returns the body as it is stored and hashed: lists are never null.
func (b AnchoredSummaryBody) Canonical() AnchoredSummaryBody {
	b.KeyFacts, b.OpenTasks = append([]string{}, b.KeyFacts...), append([]string{}, b.OpenTasks...)
	return b
}

func anchoredSummaryText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// Validate reports whether the body can be stored: text within its limits,
// and something said.
func (b AnchoredSummaryBody) Validate() error {
	if !anchoredSummaryText(b.Summary, anchoredSummaryTextLimit) {
		return fmt.Errorf("%w: anchored summary: the summary is too long or not text", ErrValidation)
	}
	if len(b.KeyFacts) > anchoredSummaryItemsLimit || len(b.OpenTasks) > anchoredSummaryItemsLimit {
		return fmt.Errorf("%w: anchored summary: too many items", ErrValidation)
	}
	said := strings.TrimSpace(b.Summary) != ""
	for _, list := range [][]string{b.KeyFacts, b.OpenTasks} {
		for _, item := range list {
			if strings.TrimSpace(item) == "" || !anchoredSummaryText(item, anchoredSummaryItemLimit) {
				return fmt.Errorf("%w: anchored summary: an item is empty, too long or not text", ErrValidation)
			}
			said = true
		}
	}
	if !said {
		return fmt.Errorf("%w: anchored summary: nothing is said", ErrValidation)
	}
	return nil
}

// Hash identifies the body's content.
func (b AnchoredSummaryBody) Hash() (ContentHash, error) {
	encoded, err := json.Marshal(b.Canonical())
	if err != nil {
		return "", err
	}
	return HashContent(encoded), nil
}

// AnchoredSummary is the summary of one session at one commit.
type AnchoredSummary struct {
	RepoID    ContentHash         `json:"repo_id"`
	Provider  ProviderKind        `json:"provider"`
	SessionID string              `json:"session_id"`
	Commit    string              `json:"commit"`
	BodyHash  ContentHash         `json:"body_hash"`
	Body      AnchoredSummaryBody `json:"body"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`

	// SegmentNo, Uploader and UploaderName are filled when summaries are
	// read: the segment of the session that ends at the commit, the account
	// the session belongs to, and its public handle for a reader who is not
	// that account. They are never stored.
	SegmentNo    int    `json:"segment_no,omitempty"`
	Uploader     string `json:"uploader,omitempty"`
	UploaderName string `json:"uploader_name,omitempty"`
}

// Validate reports whether the summary can be stored.
func (s AnchoredSummary) Validate() error {
	if err := ValidateContentHash(s.RepoID); err != nil {
		return err
	}
	if !ValidAnchoredSessionKey(s.Provider, s.SessionID) || !ValidAnchoredCommitSHA(s.Commit) {
		return fmt.Errorf("%w: anchored summary: invalid session or commit", ErrValidation)
	}
	if err := s.Body.Validate(); err != nil {
		return err
	}
	hash, err := s.Body.Hash()
	if err != nil {
		return err
	}
	if hash != s.BodyHash || s.CreatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) || s.SegmentNo != 0 || s.Uploader != "" || s.UploaderName != "" {
		return fmt.Errorf("%w: anchored summary: the stored form does not agree with its content", ErrValidation)
	}
	return nil
}

// AnchoredMemoryLimit is the most summaries one read along a branch answers.
const AnchoredMemoryLimit = 20

// AnchoredMemoryLocalCommits is the most commits of the caller's own folder
// one read along a branch may name.
const AnchoredMemoryLocalCommits = 50

// AnchoredMemory is what the summaries say along a branch, the summary of
// the newest commit first. It is found by walking the commits the server
// read from the hosting provider back from the branch's newest commit, so it
// goes on into the commits the branch started from: the newest summary of
// the branch itself comes first, and where the branch has none, that of the
// branch it was made from.
type AnchoredMemory struct {
	// Branch is the branch that was walked: the one asked for, or the
	// default branch when the remote holds no branch of that name. Head is
	// its newest commit. Both are empty when the remote was not read yet.
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head,omitempty"`
	// Walked counts the commits of the remote the walk went through.
	Walked    int               `json:"walked"`
	Summaries []AnchoredSummary `json:"summaries"`
}

// AnchoredSummaryReceipt answers a stored summary. Changed is false when the
// same text was stored before.
type AnchoredSummaryReceipt struct {
	BodyHash ContentHash `json:"body_hash"`
	Changed  bool        `json:"changed"`
}

// SegmentOfCommit returns the number of the segment that ends at the commit,
// or zero when none does or that segment is not open in this view.
func (b AnchoredSessionBody) SegmentOfCommit(sha string) int {
	for index, segment := range b.Segments {
		if segment.Hidden {
			continue
		}
		for _, commit := range segment.Commits {
			if commit.SHA == sha {
				return index + 1
			}
		}
	}
	return 0
}
