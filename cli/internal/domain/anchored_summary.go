package domain

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// A summary is what cxt writes down of a session when the session made a
// commit: what was done, what is worth keeping, what is left to do. The
// server keeps one for a session at each commit a segment of it ends at. It
// is made the way a capture's memory is made, from the conversation up to
// that commit and the tool's own memory.

// The limits the server holds a summary to.
const (
	anchoredSummaryTextLimit  = 64 << 10
	anchoredSummaryItemLimit  = 4 << 10
	anchoredSummaryItemsLimit = 256
)

// AnchoredSummaryBody is the text of a summary, as the server stores it.
type AnchoredSummaryBody struct {
	Summary   string   `json:"summary"`
	KeyFacts  []string `json:"key_facts"`
	OpenTasks []string `json:"open_tasks"`
	// TasksAuthoritative says that the open tasks are the tool's own list,
	// taken from the summary the tool wrote of its conversation, and not
	// what cxt picked out of the conversation. Only such a list is given to
	// another session as what is left to do.
	TasksAuthoritative bool `json:"tasks_authoritative,omitempty"`
}

// cutSummaryText cuts text to limit bytes at the start of a character and
// drops zero bytes, which the server takes for something that is not text.
func cutSummaryText(text string, limit int) string {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, ""), "\x00", "")
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func cutSummaryItems(items []string) []string {
	kept := []string{}
	for _, item := range items {
		if item = cutSummaryText(strings.TrimSpace(item), anchoredSummaryItemLimit); item == "" {
			continue
		}
		if len(kept) == anchoredSummaryItemsLimit {
			break
		}
		kept = append(kept, item)
	}
	return kept
}

// AnchoredSummaryOf returns what a digest says, within the limits the server
// holds a summary to. It answers false when the digest says nothing.
func AnchoredSummaryOf(digest MemoryDigest) (AnchoredSummaryBody, bool) {
	body := AnchoredSummaryBody{Summary: cutSummaryText(digest.Summary, anchoredSummaryTextLimit),
		KeyFacts: cutSummaryItems(digest.KeyFacts), OpenTasks: cutSummaryItems(digest.OpenTasks)}
	body.TasksAuthoritative = digest.TasksAuthoritative && len(body.OpenTasks) > 0
	return body, strings.TrimSpace(body.Summary) != "" || len(body.KeyFacts) > 0 || len(body.OpenTasks) > 0
}

// AnchoredSummaryPoint is a place a session is summarized at: the commits a
// segment ends at, and how many events of the session's content, from its
// start, make the conversation up to there.
type AnchoredSummaryPoint struct {
	Commits []string
	End     int
}

// AnchoredSummaryPoints lists where the report's session is summarized: once
// for each segment that ends at a commit and has conversation before its end.
// The report must have its content attached.
func AnchoredSummaryPoints(body AnchoredSessionBody) []AnchoredSummaryPoint {
	points := []AnchoredSummaryPoint{}
	if body.Content == nil {
		return points
	}
	for _, segment := range body.Segments {
		end := segment.FirstEvent + segment.Events
		if len(segment.Commits) == 0 || end < 1 || end > body.Content.Events {
			continue
		}
		point := AnchoredSummaryPoint{End: end}
		for _, commit := range segment.Commits {
			point.Commits = append(point.Commits, commit.SHA)
		}
		points = append(points, point)
	}
	return points
}

// AnchoredSummaryRemembered returns, sorted, the commits among done that the
// points still name: what a later run need not summarize again.
func AnchoredSummaryRemembered(points []AnchoredSummaryPoint, done map[string]struct{}) []string {
	kept := []string{}
	for _, point := range points {
		for _, commit := range point.Commits {
			if _, ok := done[commit]; ok {
				kept = append(kept, commit)
			}
		}
	}
	sort.Strings(kept)
	return kept
}

// AnchoredSummary is a session's summary at one commit as the server answers
// a read: where it belongs and what it says.
type AnchoredSummary struct {
	Provider     ProviderKind        `json:"provider"`
	SessionID    string              `json:"session_id"`
	Commit       string              `json:"commit"`
	Body         AnchoredSummaryBody `json:"body"`
	SegmentNo    int                 `json:"segment_no,omitempty"`
	UploaderName string              `json:"uploader_name,omitempty"`
}

// AnchoredMemory is what the server found along a branch, the summary of the
// newest commit first. Branch is the branch it walked: the one asked for, or
// the default branch when the remote holds no branch of that name.
type AnchoredMemory struct {
	Branch    string            `json:"branch,omitempty"`
	Head      string            `json:"head,omitempty"`
	Summaries []AnchoredSummary `json:"summaries"`
}

const (
	// AnchoredMemoryLocalCommits is the most commits of this folder one read
	// along a branch may name.
	AnchoredMemoryLocalCommits = 50
	// BranchSummaryRead is how many summaries are asked for when a session is
	// given one: enough to step over those the session wrote itself.
	BranchSummaryRead = 5
	// BranchSummaryMaxBytes bounds the text a session is given.
	BranchSummaryMaxBytes = 16 << 10
)

// BranchSummaryFor picks what a session is given from what was found along
// its branch: the newest summary another session wrote. What the session
// wrote itself is already in its own conversation.
func BranchSummaryFor(memory AnchoredMemory, provider ProviderKind, sessionID string) (AnchoredSummary, bool) {
	for _, summary := range memory.Summaries {
		if summary.Provider == provider && summary.SessionID == sessionID {
			continue
		}
		return summary, true
	}
	return AnchoredSummary{}, false
}
