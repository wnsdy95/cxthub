package domain

import (
	"fmt"
	"sort"
	"time"
)

// Restoring sessions from captures. Before session reports, the server kept a
// session as captures: each one the whole conversation at one moment, with
// the commits that moment published. A session that no CLI has reported is
// rebuilt from them, at the granularity of its captures:
//
//   - The newest capture holds the conversation; its events are the
//     session's records, named by their position in it.
//   - An earlier capture places a boundary at its own last event, when its
//     whole conversation is the beginning of the newest one. Otherwise it
//     places none.
//   - A capture that published commits and places a boundary ends a segment
//     linked to those commits; the newest capture always places one.
//   - What follows the last segment is the uncommitted part. Captures do not
//     say where it was worked on, so its place is unknown.
//
// A CLI's report of the same session is exact and replaces the restored one.

const (
	// AnchoredScopeCaptures: the session belongs to the repository because it
	// was captured into it, and the report was rebuilt from those captures.
	AnchoredScopeCaptures = "captures"
	// AnchoredLinkCapturedAtCommit: a capture of the session published the
	// commit.
	AnchoredLinkCapturedAtCommit = "captured-at-commit"
	// AnchoredRestoredUploader stands in for the uploader of a restored
	// report. No account has it, so only what is open of such a report is
	// read, by every member alike.
	AnchoredRestoredUploader = "cxt:captures"
)

// AnchoredCapture is one capture of a session, as the restoration reads it.
type AnchoredCapture struct {
	Snapshot  ContentHash
	Doc       ContentHash
	Identity  DocumentIdentity
	CreatedAt time.Time
	// Commits are the commits the capture published, in the order published.
	Commits []string
	// Events is how many events the capture's conversation holds.
	Events int
	// Prefix: the capture's whole conversation is the beginning of the
	// newest capture's. The newest capture is its own beginning.
	Prefix bool
}

// AnchoredRecordName names a record of a restored report: an event of the
// capture document, by its position.
func AnchoredRecordName(event int) string { return fmt.Sprintf("event-%d", event) }

// RestoreAnchoredSession rebuilds a session's report from its captures. The
// captures may come in any order; the newest one, by time and then
// snapshot, holds the conversation. It answers false when there is nothing
// to rebuild: no capture, or a newest capture without events.
func RestoreAnchoredSession(captures []AnchoredCapture) (AnchoredSessionBody, bool) {
	if len(captures) == 0 {
		return AnchoredSessionBody{}, false
	}
	ordered := append([]AnchoredCapture{}, captures...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if !ordered[left].CreatedAt.Equal(ordered[right].CreatedAt) {
			return ordered[left].CreatedAt.Before(ordered[right].CreatedAt)
		}
		return ordered[left].Snapshot < ordered[right].Snapshot
	})
	newest := ordered[len(ordered)-1]
	if newest.Events < 1 {
		return AnchoredSessionBody{}, false
	}
	first := ordered[0].CreatedAt.UTC()
	body := AnchoredSessionBody{RuleVersion: AnchoredSessionRuleVersion, Scope: AnchoredScopeCaptures, Records: newest.Events,
		StartedAt: &first, Content: &AnchoredContent{Doc: newest.Doc, Identity: newest.Identity, Events: newest.Events}}
	end, seen := 0, map[string]struct{}{}
	at := func(capture AnchoredCapture) *time.Time {
		created := capture.CreatedAt.UTC()
		return &created
	}
	for index, capture := range ordered {
		last := index == len(ordered)-1
		if !last && (!capture.Prefix || capture.Events > newest.Events) {
			continue
		}
		var commits []AnchoredCommit
		for _, sha := range capture.Commits {
			if _, dup := seen[sha]; dup || !ValidAnchoredCommitSHA(sha) {
				continue
			}
			seen[sha] = struct{}{}
			commits = append(commits, AnchoredCommit{SHA: sha, Rule: AnchoredLinkCapturedAtCommit})
		}
		if len(commits) == 0 {
			continue
		}
		if capture.Events <= end {
			// Nothing was said since the boundary before: the commits belong
			// to the segment that ends there.
			if segments := len(body.Segments); segments > 0 {
				body.Segments[segments-1].Commits = append(body.Segments[segments-1].Commits, commits...)
			}
			continue
		}
		body.Segments = append(body.Segments, AnchoredSegment{AnchoredRange: AnchoredRange{
			First: AnchoredRecordName(end), Last: AnchoredRecordName(capture.Events - 1), FirstOrdinal: end, LastOrdinal: capture.Events - 1,
			EndedAt: at(capture), FirstEvent: end, Events: capture.Events - end,
		}, Commits: commits})
		end = capture.Events
	}
	if end < newest.Events {
		body.Uncommitted = &AnchoredUncommitted{AnchoredRange: AnchoredRange{
			First: AnchoredRecordName(end), Last: AnchoredRecordName(newest.Events - 1), FirstOrdinal: end, LastOrdinal: newest.Events - 1,
			EndedAt: at(newest), FirstEvent: end, Events: newest.Events - end,
		}, Places: []AnchoredPlace{}, Unplaced: true}
	}
	ended := newest.CreatedAt.UTC()
	body.EndedAt = &ended
	return body.Canonical(), true
}

// AnchoredRestoreResult says what one restoration request did. Remaining is
// true when sessions are left for the next request: more than one request
// rebuilds, or conversations that could not be read yet.
type AnchoredRestoreResult struct {
	Restored   int  `json:"restored"`
	Unchanged  int  `json:"unchanged"`
	Reported   int  `json:"reported"`
	Unreadable int  `json:"unreadable"`
	Remaining  bool `json:"remaining"`
}
