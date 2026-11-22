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
