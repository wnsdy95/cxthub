package domain

import (
	"fmt"
	"sort"
	"time"
)

// Publication is which parts of a session the team may read. A session is on
// the server from the start, readable by its uploader only; a part opens to
// the team when what it explains is pushed:
//
//   - a segment, when one of its commits, or a copy of it, is on the remote;
//   - the uncommitted part, up to the last turn that had begun when a branch
//     it passed through was pushed;
//   - a spawned session as a whole, when the point of its parent where it was
//     spawned is open.
//
// What was opened stays open. It is recorded as ranges of the session's own
// records, never as segment numbers, so cutting the session again, or a
// forced push that removes the commit, takes nothing back.

const (
	AnchoredPublishedByCommit     = "commit"
	AnchoredPublishedByBranchPush = "branch-push"
	AnchoredPublishedBySpawn      = "spawned"

	anchoredPublishedRangeLimit = 100000
)

// AnchoredPublicationReason says what opened a range: the remote commit a
// segment ends at (the commit itself or its copy), the pushed branch an
// uncommitted part passed through, or the parent session that spawned it.
type AnchoredPublicationReason struct {
	Kind   string `json:"kind"`
	Commit string `json:"commit,omitempty"`
	Branch string `json:"branch,omitempty"`
	Parent string `json:"parent,omitempty"`
}

// AnchoredPublishedRange is a run of a session's own records that is open to
// the team, with its place in the content document.
type AnchoredPublishedRange struct {
	FirstOrdinal int                       `json:"first_ordinal"`
	LastOrdinal  int                       `json:"last_ordinal"`
	FirstEvent   int                       `json:"first_event,omitempty"`
	Events       int                       `json:"events,omitempty"`
	At           time.Time                 `json:"at"`
	By           AnchoredPublicationReason `json:"by"`
}

// AnchoredPublicationFacts is what the server knows from the hosting provider
// and from its own records when it decides what is open.
type AnchoredPublicationFacts struct {
	// OnRemote answers the commit on the remote that stands for a commit a
	// session made: the commit itself, or a copy of it.
	OnRemote func(commit AnchoredCommit) (string, bool)
	// PushedAt lists the times the server saw a branch pushed.
	PushedAt func(branch string) []time.Time
	// ParentOpen reports whether the record at that position of the parent
	// session is open.
	ParentOpen func(parent string, ordinal int) bool
}

// PublishableRanges applies the rule to a report as it is cut now. The ranges
// carry no time; AddPublishedRanges stamps what is new.
func (b AnchoredSessionBody) PublishableRanges(facts AnchoredPublicationFacts) []AnchoredPublishedRange {
	var ranges []AnchoredPublishedRange
	events := 0
	if b.Content != nil {
		events = b.Content.Events
	}
	if b.Spawn != nil && b.Parent != "" && b.Records > 0 && facts.ParentOpen != nil && facts.ParentOpen(b.Parent, b.Spawn.AtOrdinal) {
		return []AnchoredPublishedRange{{FirstOrdinal: 0, LastOrdinal: b.Records - 1, FirstEvent: 0, Events: events,
			By: AnchoredPublicationReason{Kind: AnchoredPublishedBySpawn, Parent: b.Parent}}}
	}
	for _, segment := range b.Segments {
		for _, commit := range segment.Commits {
			if facts.OnRemote == nil {
				break
			}
			if remote, ok := facts.OnRemote(commit); ok {
				ranges = append(ranges, AnchoredPublishedRange{FirstOrdinal: segment.FirstOrdinal, LastOrdinal: segment.LastOrdinal,
					FirstEvent: segment.FirstEvent, Events: segment.Events, By: AnchoredPublicationReason{Kind: AnchoredPublishedByCommit, Commit: remote}})
				break
			}
		}
	}
	if part := b.Uncommitted; part != nil && facts.PushedAt != nil {
		// The latest push of any branch the part passed through decides.
		var pushed time.Time
		branch := ""
		for _, place := range part.Places {
			if place.Branch == "" {
				continue
			}
			for _, at := range facts.PushedAt(place.Branch) {
				if at.After(pushed) {
					pushed, branch = at, place.Branch
				}
			}
		}
		if branch != "" {
			by := AnchoredPublicationReason{Kind: AnchoredPublishedByBranchPush, Branch: branch}
			if open, ok := part.openUntil(pushed); ok {
				open.By = by
				ranges = append(ranges, open)
			}
		}
	}
	return ranges
}

// openUntil returns the beginning of the part that had begun by the given
// time: every turn that started at or before it. A part without turn marks is
// one unit and is open only when all of it lies at or before that time.
func (part AnchoredUncommitted) openUntil(pushed time.Time) (AnchoredPublishedRange, bool) {
	if len(part.Turns) == 0 {
		if part.EndedAt == nil || part.EndedAt.After(pushed) {
			return AnchoredPublishedRange{}, false
		}
		return AnchoredPublishedRange{FirstOrdinal: part.FirstOrdinal, LastOrdinal: part.LastOrdinal, FirstEvent: part.FirstEvent, Events: part.Events}, true
	}
	begun := 0
	for begun < len(part.Turns) && part.Turns[begun].StartedAt != nil && !part.Turns[begun].StartedAt.After(pushed) {
		begun++
	}
	switch {
	case begun == 0:
		return AnchoredPublishedRange{}, false
	case begun == len(part.Turns):
		return AnchoredPublishedRange{FirstOrdinal: part.FirstOrdinal, LastOrdinal: part.LastOrdinal, FirstEvent: part.FirstEvent, Events: part.Events}, true
	}
	next := part.Turns[begun]
	return AnchoredPublishedRange{FirstOrdinal: part.FirstOrdinal, LastOrdinal: next.FirstOrdinal - 1, FirstEvent: part.FirstEvent, Events: next.FirstEvent - part.FirstEvent}, true
}

func (r AnchoredPublishedRange) validate() error {
	if r.FirstOrdinal < 0 || r.LastOrdinal < r.FirstOrdinal || r.FirstEvent < 0 || r.Events < 0 || r.At.IsZero() ||
		!anchoredOneOf(r.By.Kind, AnchoredPublishedByCommit, AnchoredPublishedByBranchPush, AnchoredPublishedBySpawn) {
		return fmt.Errorf("%w: published range", ErrValidation)
	}
	return nil
}

// ValidateAnchoredPublishedRanges checks a stored list.
func ValidateAnchoredPublishedRanges(ranges []AnchoredPublishedRange) error {
	if len(ranges) > anchoredPublishedRangeLimit {
		return fmt.Errorf("%w: too many published ranges", ErrValidation)
	}
	for _, published := range ranges {
		if err := published.validate(); err != nil {
			return err
		}
	}
	return nil
}

type anchoredSpan struct{ first, end int } // [first, end)

// mergedSpans returns the union of the spans as disjoint spans in order.
func mergedSpans(spans []anchoredSpan) []anchoredSpan {
	sort.Slice(spans, func(left, right int) bool { return spans[left].first < spans[right].first })
	var merged []anchoredSpan
	for _, span := range spans {
		if span.end <= span.first {
			continue
		}
		if last := len(merged) - 1; last >= 0 && span.first <= merged[last].end {
			if span.end > merged[last].end {
				merged[last].end = span.end
			}
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

func spansCover(spans []anchoredSpan, want anchoredSpan) bool {
	if want.end <= want.first {
		return true
	}
	for _, span := range spans {
		if span.first <= want.first && want.end <= span.end {
			return true
		}
	}
	return false
}

// PublishedOrdinals returns the open records as disjoint spans [first, end).
func PublishedOrdinals(ranges []AnchoredPublishedRange) [][2]int {
	spans := make([]anchoredSpan, 0, len(ranges))
	for _, published := range ranges {
		spans = append(spans, anchoredSpan{published.FirstOrdinal, published.LastOrdinal + 1})
	}
	out := [][2]int{}
	for _, span := range mergedSpans(spans) {
		out = append(out, [2]int{span.first, span.end})
	}
	return out
}

// PublishedEvents returns the open events of the content document as
// disjoint spans [first, end).
func PublishedEvents(ranges []AnchoredPublishedRange) [][2]int {
	spans := make([]anchoredSpan, 0, len(ranges))
	for _, published := range ranges {
		spans = append(spans, anchoredSpan{published.FirstEvent, published.FirstEvent + published.Events})
	}
	out := [][2]int{}
	for _, span := range mergedSpans(spans) {
		out = append(out, [2]int{span.first, span.end})
	}
	return out
}

// AddPublishedRanges adds what is newly open to what was open before and
// reports whether anything was added. A range the earlier ones already cover,
// in records and in events, adds nothing; nothing is ever removed.
func AddPublishedRanges(open, publishable []AnchoredPublishedRange, at time.Time) ([]AnchoredPublishedRange, bool) {
	merged := append([]AnchoredPublishedRange{}, open...)
	changed := false
	for _, candidate := range publishable {
		ordinals, events := make([]anchoredSpan, 0, len(merged)), make([]anchoredSpan, 0, len(merged))
		for _, published := range merged {
			ordinals = append(ordinals, anchoredSpan{published.FirstOrdinal, published.LastOrdinal + 1})
			events = append(events, anchoredSpan{published.FirstEvent, published.FirstEvent + published.Events})
		}
		if spansCover(mergedSpans(ordinals), anchoredSpan{candidate.FirstOrdinal, candidate.LastOrdinal + 1}) &&
			spansCover(mergedSpans(events), anchoredSpan{candidate.FirstEvent, candidate.FirstEvent + candidate.Events}) {
			continue
		}
		candidate.At = at.UTC()
		merged = append(merged, candidate)
		changed = true
	}
	return merged, changed
}

// OrdinalOpen reports whether the record at that position is open.
func OrdinalOpen(ranges []AnchoredPublishedRange, ordinal int) bool {
	for _, published := range ranges {
		if ordinal >= published.FirstOrdinal && ordinal <= published.LastOrdinal {
			return true
		}
	}
	return false
}

// AnchoredPublication is what is open of one session.
type AnchoredPublication struct {
	Provider  ProviderKind             `json:"provider"`
	SessionID string                   `json:"session_id"`
	Ranges    []AnchoredPublishedRange `json:"ranges"`
}

// Validate checks a stored publication.
func (p AnchoredPublication) Validate() error {
	if !ValidAnchoredSessionKey(p.Provider, p.SessionID) {
		return fmt.Errorf("%w: publication key", ErrValidation)
	}
	return ValidateAnchoredPublishedRanges(p.Ranges)
}

// AnchoredContentAccess is what one reader may read of a session's
// conversation: the documents that hold it, in order, and the events open to
// that reader, as disjoint spans [first, end) in order.
type AnchoredContentAccess struct {
	Parts  []AnchoredContentPart
	Events int
	Open   [][2]int
}

// Part returns the document that holds the event at index, and the events
// [first, end) of the conversation that document holds: the event is at
// index-first in it.
func (a AnchoredContentAccess) Part(index int) (doc ContentHash, first, end int, found bool) {
	for _, part := range a.Parts {
		if end = first + part.Events; index >= first && index < end {
			return part.Doc, first, end, true
		}
		first = end
	}
	return "", 0, 0, false
}

// Span returns the open span that holds the event at index.
func (a AnchoredContentAccess) Span(index int) ([2]int, bool) {
	for _, span := range a.Open {
		if index >= span[0] && index < span[1] {
			return span, true
		}
	}
	return [2]int{}, false
}

// From returns the first open event at or after index: index itself when it
// is open, otherwise the beginning of the next open span.
func (a AnchoredContentAccess) From(index int) (int, [2]int, bool) {
	for _, span := range a.Open {
		if index < span[0] {
			return span[0], span, true
		}
		if index < span[1] {
			return index, span, true
		}
	}
	return 0, [2]int{}, false
}

// AnchoredFragmentPage is a piece of a session's conversation as one reader
// may read it, cut to a size a tool takes at once: whole events where they
// fit, and a part of one event where it alone is larger. NextIndex is the
// event to go on with and NextOffset the byte within it; NextIndex is -1 at
// the end of what is open to that reader. Neither ever points into what is
// not open: reading goes on at the next open part.
type AnchoredFragmentPage struct {
	Fragments  []DocEventFragment `json:"fragments"`
	Total      int                `json:"total_events"`
	Open       [][2]int           `json:"open_events"`
	NextIndex  int                `json:"next_index"`
	NextOffset int                `json:"next_offset"`
}

// AnchoredEventPage is a page of a session's conversation as one reader may
// read it. Next is the next event to ask for, or -1 at the end of what is
// open to that reader from here; it never points into what is not.
type AnchoredEventPage struct {
	Events []DocViewEvent `json:"events"`
	Offset int            `json:"offset"`
	Next   int            `json:"next"`
}
