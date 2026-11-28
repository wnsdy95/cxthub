package domain

import (
	"fmt"
	"strings"
	"time"
)

// Searching the sessions tab. One query is looked for in everything the tab
// shows: pull request titles, branch names, commit subjects, the
// conversations of sessions and manual notes. Each hit says which of these it
// is. A reader is answered only from what that reader may read: a part of
// another account's session that is not open is never a hit.

const (
	AnchoredHitPull    = "pull"
	AnchoredHitBranch  = "branch"
	AnchoredHitCommit  = "commit"
	AnchoredHitSegment = "segment"
	AnchoredHitNote    = "note"

	// AnchoredSearchKindLimit is the most hits of one kind in an answer.
	AnchoredSearchKindLimit = 20
	// AnchoredSearchSessionHits is the most hits one session contributes.
	AnchoredSearchSessionHits = 3
	// AnchoredSearchSessions is the most conversations one query reads.
	AnchoredSearchSessions = 200

	anchoredSearchQueryMin = 2
	anchoredSearchQueryMax = 256
	anchoredSnippetRunes   = 160
)

// AnchoredSearchHit is one thing the query was found in.
type AnchoredSearchHit struct {
	Kind string `json:"kind"`
	// Title is the pull request's title, the branch name, the commit subject,
	// or who spoke the matching part of a conversation.
	Title string `json:"title"`
	// Snippet is the text around the match, for a conversation or a note.
	Snippet string `json:"snippet,omitempty"`

	Pull   int    `json:"pull,omitempty"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`

	// A conversation hit names the session and where the matching event is
	// in it: a segment, or the part after the last commit.
	Provider  ProviderKind `json:"provider,omitempty"`
	SessionID string       `json:"session_id,omitempty"`
	SegmentNo int          `json:"segment_no,omitempty"`
	Rest      bool         `json:"rest,omitempty"`
	Event     *int         `json:"event,omitempty"`

	// A note hit names the note and what it was written about.
	NoteID string              `json:"note_id,omitempty"`
	Target *AnchoredNoteTarget `json:"target,omitempty"`

	At *time.Time `json:"at,omitempty"`
}

// AnchoredSearchResult is the answer to one query. Truncated is true when a
// kind reached its limit or not every conversation was read: a narrower
// query finds what was left out.
type AnchoredSearchResult struct {
	Hits      []AnchoredSearchHit `json:"hits"`
	Truncated bool                `json:"truncated"`
}

// NormalizeAnchoredSearchQuery returns the query as it is compared: without
// surrounding space and in lower case.
func NormalizeAnchoredSearchQuery(raw string) (string, error) {
	query := strings.ToLower(strings.TrimSpace(raw))
	if runes := len([]rune(query)); runes < anchoredSearchQueryMin || runes > anchoredSearchQueryMax {
		return "", fmt.Errorf("%w: a search needs %d to %d characters", ErrValidation, anchoredSearchQueryMin, anchoredSearchQueryMax)
	}
	return query, nil
}

// AnchoredSnippet returns the text around the first place the query, already
// normalized, is found in text, on one line. It answers false when the query
// is not in the text.
func AnchoredSnippet(text, query string) (string, bool) {
	runes, lowered := []rune(text), []rune(strings.ToLower(text))
	// Lower case keeps the number of runes for the text this is used on; when
	// it does not, the match is reported without a place.
	at := strings.Index(string(lowered), query)
	if at < 0 {
		return "", false
	}
	start := len([]rune(string(lowered)[:at]))
	if len(lowered) != len(runes) {
		start = 0
	}
	from := start - anchoredSnippetRunes/4
	if from < 0 {
		from = 0
	}
	to := from + anchoredSnippetRunes
	if to > len(runes) {
		to = len(runes)
	}
	snippet := strings.Join(strings.Fields(string(runes[from:to])), " ")
	if from > 0 {
		snippet = "…" + snippet
	}
	if to < len(runes) {
		snippet += "…"
	}
	return snippet, true
}

// EventPart says which part of the session an event of its conversation is
// in: the number of a segment, or the part after the last commit. In the
// view another account is given, a part that is not open holds no event, so
// an event of it is in no part.
func (b AnchoredSessionBody) EventPart(event int) (segmentNo int, rest bool, ok bool) {
	for index, segment := range b.Segments {
		if !segment.Hidden && event >= segment.FirstEvent && event < segment.FirstEvent+segment.Events {
			return index + 1, false, true
		}
	}
	if part := b.Uncommitted; part != nil && event >= part.FirstEvent && event < part.FirstEvent+part.Events {
		return 0, true, true
	}
	return 0, false, false
}
