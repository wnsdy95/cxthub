package domain

import (
	"reflect"
	"testing"
	"time"
)

func anchoredCaptureAt(minute int, events int, prefix bool, commits ...string) AnchoredCapture {
	return AnchoredCapture{Snapshot: HashContent([]byte{byte(minute)}), Doc: HashContent([]byte{byte(minute), 1}), Identity: DocumentIdentityLegacy,
		CreatedAt: time.Date(2031, 1, 2, 3, minute, 0, 0, time.UTC), Commits: commits, Events: events, Prefix: prefix}
}

func TestSessionIsRestoredFromItsCapturesAtTheirGranularity(t *testing.T) {
	a, b, c := anchoredTestSHA("a"), anchoredTestSHA("b"), anchoredTestSHA("c")
	newest := anchoredCaptureAt(40, 12, true)
	body, ok := RestoreAnchoredSession([]AnchoredCapture{
		newest,
		anchoredCaptureAt(10, 4, true, a),
		anchoredCaptureAt(20, 6, true),     // a hook capture: no commit, no boundary
		anchoredCaptureAt(25, 7, false, b), // its conversation is not the beginning of the newest: no boundary
		anchoredCaptureAt(30, 9, true, c),
		anchoredCaptureAt(31, 9, true, a, b), // nothing new since the boundary before; a is named already
	})
	if !ok {
		t.Fatal("nothing restored")
	}
	if err := body.Validate(); err != nil {
		t.Fatalf("the restored report does not validate: %v", err)
	}
	if body.Scope != AnchoredScopeCaptures || body.Records != 12 || body.Content == nil || body.Content.Doc != newest.Doc || body.Content.Events != 12 ||
		!body.StartedAt.Equal(anchoredCaptureAt(10, 0, true).CreatedAt) || !body.EndedAt.Equal(newest.CreatedAt) {
		t.Fatalf("restored report: %+v", body)
	}
	shape := [][3]int{}
	links := [][]string{}
	for _, segment := range body.Segments {
		shape = append(shape, [3]int{segment.FirstOrdinal, segment.LastOrdinal, segment.Events})
		shas := []string{}
		for _, commit := range segment.Commits {
			if commit.Rule != AnchoredLinkCapturedAtCommit {
				t.Fatalf("a restored link by another rule: %+v", commit)
			}
			shas = append(shas, commit.SHA)
		}
		links = append(links, shas)
	}
	if !reflect.DeepEqual(shape, [][3]int{{0, 3, 4}, {4, 8, 5}}) || !reflect.DeepEqual(links, [][]string{{a}, {c, b}}) {
		t.Fatalf("segments %v linked to %v", shape, links)
	}
	if body.Segments[0].First != "event-0" || body.Segments[1].Last != "event-8" {
		t.Fatalf("records are named by their place in the document: %+v", body.Segments)
	}
	part := body.Uncommitted
	if part == nil || part.FirstOrdinal != 9 || part.LastOrdinal != 11 || part.FirstEvent != 9 || part.Events != 3 || !part.Unplaced || len(part.Places) != 0 {
		t.Fatalf("the part after the last commit: %+v", part)
	}
}

func TestRestoredSessionEdges(t *testing.T) {
	if _, ok := RestoreAnchoredSession(nil); ok {
		t.Fatal("restored from no capture")
	}
	if _, ok := RestoreAnchoredSession([]AnchoredCapture{anchoredCaptureAt(10, 0, true)}); ok {
		t.Fatal("restored from an empty conversation")
	}
	// The newest capture published a commit: everything is one segment.
	whole, ok := RestoreAnchoredSession([]AnchoredCapture{anchoredCaptureAt(10, 5, true, anchoredTestSHA("a"))})
	if !ok || len(whole.Segments) != 1 || whole.Uncommitted != nil || whole.Segments[0].LastOrdinal != 4 || whole.Validate() != nil {
		t.Fatalf("one capture at a commit: %+v", whole)
	}
	// No capture published a commit: everything is uncommitted and unplaced.
	loose, ok := RestoreAnchoredSession([]AnchoredCapture{anchoredCaptureAt(10, 2, true), anchoredCaptureAt(20, 5, true)})
	if !ok || len(loose.Segments) != 0 || loose.Uncommitted == nil || loose.Uncommitted.LastOrdinal != 4 || !loose.Uncommitted.Unplaced || loose.Validate() != nil {
		t.Fatalf("captures without commits: %+v", loose)
	}
	// A conversation that shrank (rotated, or masked another way) places no boundary.
	shrunk, ok := RestoreAnchoredSession([]AnchoredCapture{anchoredCaptureAt(10, 9, true, anchoredTestSHA("a")), anchoredCaptureAt(20, 5, true)})
	if !ok || len(shrunk.Segments) != 0 || shrunk.Uncommitted.Events != 5 || shrunk.Validate() != nil {
		t.Fatalf("a longer earlier capture: %+v", shrunk)
	}
	// A client may not claim the server's own rule.
	claimed := anchoredTestBody()
	claimed.Segments[0].Commits[0].Rule = AnchoredLinkCapturedAtCommit
	if err := claimed.Validate(); err == nil {
		t.Fatal("a reported session took the captures' link rule")
	}
	restored := whole
	restored.Machine = "laptop"
	if err := restored.Validate(); err == nil {
		t.Fatal("a restored report named a machine")
	}
}
