package domain

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// anchoredPublicationBody is the well-formed report with content (segment 1:
// records 0-1, events 0-5; segment 2: records 2-3, no event; uncommitted:
// records 4-5, events 6-9, on branch main) and two turns in the uncommitted
// part, the second beginning at record 5, event 8, forty-five seconds in.
func anchoredPublicationBody() AnchoredSessionBody {
	body := anchoredTestBodyWithContent()
	body.Uncommitted.StartedAt, body.Uncommitted.EndedAt = anchoredTestTime(40), anchoredTestTime(50)
	body.Uncommitted.Turns = []AnchoredTurnMark{{FirstOrdinal: 4, FirstEvent: 6, StartedAt: anchoredTestTime(40)}, {FirstOrdinal: 5, FirstEvent: 8, StartedAt: anchoredTestTime(45)}}
	return body
}

func anchoredRemoteHolding(shas ...string) func(AnchoredCommit) (string, bool) {
	return func(commit AnchoredCommit) (string, bool) {
		for _, sha := range shas {
			if commit.SHA == sha {
				return sha, true
			}
		}
		return "", false
	}
}

func anchoredPushes(times map[string][]time.Time) func(string) []time.Time {
	return func(branch string) []time.Time { return times[branch] }
}

func TestSegmentOpensWhenOneOfItsCommitsIsOnTheRemote(t *testing.T) {
	body := anchoredPublicationBody()
	if got := body.PublishableRanges(AnchoredPublicationFacts{}); len(got) != 0 {
		t.Fatalf("with nothing pushed: %+v", got)
	}
	// The second segment ends at two commits; one of them on the remote opens it.
	got := body.PublishableRanges(AnchoredPublicationFacts{OnRemote: anchoredRemoteHolding(anchoredTestSHA("c"))})
	want := []AnchoredPublishedRange{{FirstOrdinal: 2, LastOrdinal: 3, FirstEvent: 6, Events: 0, By: AnchoredPublicationReason{Kind: AnchoredPublishedByCommit, Commit: anchoredTestSHA("c")}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("second segment: %+v", got)
	}
	// A copy on the remote stands for the commit the session made.
	copySHA := anchoredTestSHA("9")
	got = body.PublishableRanges(AnchoredPublicationFacts{OnRemote: func(commit AnchoredCommit) (string, bool) {
		return copySHA, commit.SHA == anchoredTestSHA("a")
	}})
	want = []AnchoredPublishedRange{{FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 6, By: AnchoredPublicationReason{Kind: AnchoredPublishedByCommit, Commit: copySHA}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("first segment through its copy: %+v", got)
	}
}

func TestUncommittedPartOpensUpToTheTurnsBegunWhenItsBranchWasPushed(t *testing.T) {
	body := anchoredPublicationBody()
	branch := AnchoredPublicationReason{Kind: AnchoredPublishedByBranchPush, Branch: "main"}
	open := func(pushes ...time.Time) []AnchoredPublishedRange {
		return body.PublishableRanges(AnchoredPublicationFacts{PushedAt: anchoredPushes(map[string][]time.Time{"main": pushes, "elsewhere": {*anchoredTestTime(59)}})})
	}
	if got := open(); len(got) != 0 {
		t.Fatalf("a branch that was never pushed: %+v", got)
	}
	if got := open(*anchoredTestTime(39)); len(got) != 0 {
		t.Fatalf("a push before the part began: %+v", got)
	}
	// Pushed during the first turn: that turn, and not the one that began later.
	first := []AnchoredPublishedRange{{FirstOrdinal: 4, LastOrdinal: 4, FirstEvent: 6, Events: 2, By: branch}}
	if got := open(*anchoredTestTime(42)); !reflect.DeepEqual(got, first) {
		t.Fatalf("pushed during the first turn: %+v", got)
	}
	// The latest push decides; a push at the very moment a turn began includes it.
	whole := []AnchoredPublishedRange{{FirstOrdinal: 4, LastOrdinal: 5, FirstEvent: 6, Events: 4, By: branch}}
	if got := open(*anchoredTestTime(41), *anchoredTestTime(45)); !reflect.DeepEqual(got, whole) {
		t.Fatalf("pushed when the second turn began: %+v", got)
	}
	// A part that only passed through a worktree without a branch never opens this way.
	detached := anchoredPublicationBody()
	detached.Uncommitted.Places = []AnchoredPlace{{Worktree: "app-detached", Rule: AnchoredPlaceHeadLog}}
	if got := detached.PublishableRanges(AnchoredPublicationFacts{PushedAt: func(string) []time.Time { return []time.Time{*anchoredTestTime(59)} }}); len(got) != 0 {
		t.Fatalf("a part with no branch: %+v", got)
	}
	// Without turn marks the part is one unit: open only when all of it lies before the push.
	unmarked := anchoredPublicationBody()
	unmarked.Uncommitted.Turns = nil
	facts := func(second int) AnchoredPublicationFacts {
		return AnchoredPublicationFacts{PushedAt: anchoredPushes(map[string][]time.Time{"main": {*anchoredTestTime(second)}})}
	}
	if got := unmarked.PublishableRanges(facts(49)); len(got) != 0 {
		t.Fatalf("a part that went on after the push: %+v", got)
	}
	if got := unmarked.PublishableRanges(facts(50)); !reflect.DeepEqual(got, whole) {
		t.Fatalf("a part that ended by the push: %+v", got)
	}
	// A turn without a time cannot be said to have begun, nor any turn after it.
	untimed := anchoredPublicationBody()
	untimed.Uncommitted.Turns[1].StartedAt = nil
	if got := untimed.PublishableRanges(facts(59)); !reflect.DeepEqual(got, first) {
		t.Fatalf("a turn without a time: %+v", got)
	}
}

func TestSpawnedSessionOpensAsAWholeWithThePointItWasSpawnedAt(t *testing.T) {
	child := anchoredPublicationBody()
	child.Sub, child.Parent, child.Spawn = true, "parent-session", &AnchoredSpawn{AtRecord: "p3", AtOrdinal: 3, Rule: AnchoredSpawnToolCall}
	asked := []int{}
	facts := func(open bool) AnchoredPublicationFacts {
		return AnchoredPublicationFacts{ParentOpen: func(parent string, ordinal int) bool {
			asked = append(asked, ordinal)
			return open && parent == "parent-session"
		}}
	}
	if got := child.PublishableRanges(facts(false)); len(got) != 0 {
		t.Fatalf("a parent that is not open there: %+v", got)
	}
	want := []AnchoredPublishedRange{{FirstOrdinal: 0, LastOrdinal: 5, FirstEvent: 0, Events: 10, By: AnchoredPublicationReason{Kind: AnchoredPublishedBySpawn, Parent: "parent-session"}}}
	if got := child.PublishableRanges(facts(true)); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(asked, []int{3, 3}) {
		t.Fatalf("an open parent: %+v asked=%v", got, asked)
	}
	// Without a spawn point nothing says where in the parent it began.
	child.Spawn = nil
	if got := child.PublishableRanges(facts(true)); len(got) != 0 {
		t.Fatalf("a spawned session without a point: %+v", got)
	}
}

func TestWhatWasOpenedStaysOpen(t *testing.T) {
	first, later := time.Date(2031, 1, 3, 0, 0, 0, 0, time.UTC), time.Date(2031, 1, 4, 0, 0, 0, 0, time.FixedZone("local", 9*3600))
	body := anchoredPublicationBody()
	onRemote := AnchoredPublicationFacts{OnRemote: anchoredRemoteHolding(anchoredTestSHA("a"))}
	open, changed := AddPublishedRanges(nil, body.PublishableRanges(onRemote), first)
	if !changed || len(open) != 1 || !open[0].At.Equal(first) || ValidateAnchoredPublishedRanges(open) != nil {
		t.Fatalf("first opening: %+v %v", open, changed)
	}
	// The same facts again add nothing.
	if again, changed := AddPublishedRanges(open, body.PublishableRanges(onRemote), later); changed || !reflect.DeepEqual(again, open) {
		t.Fatalf("the same facts again: %+v %v", again, changed)
	}
	// The commit is gone from the remote (a forced push): nothing is taken back.
	if kept, changed := AddPublishedRanges(open, body.PublishableRanges(AnchoredPublicationFacts{}), later); changed || !reflect.DeepEqual(kept, open) {
		t.Fatalf("after the commit left the remote: %+v %v", kept, changed)
	}
	// More is pushed: only what is new gets the later time.
	more := AnchoredPublicationFacts{OnRemote: anchoredRemoteHolding(anchoredTestSHA("a"), anchoredTestSHA("b")), PushedAt: anchoredPushes(map[string][]time.Time{"main": {*anchoredTestTime(42)}})}
	open, changed = AddPublishedRanges(open, body.PublishableRanges(more), later)
	if !changed || len(open) != 3 || !open[0].At.Equal(first) || !open[1].At.Equal(later) || open[1].At.Location() != time.UTC {
		t.Fatalf("after more was pushed: %+v %v", open, changed)
	}
	if got := PublishedOrdinals(open); !reflect.DeepEqual(got, [][2]int{{0, 5}}) {
		t.Fatalf("open records: %v", got)
	}
	if got := PublishedEvents(open); !reflect.DeepEqual(got, [][2]int{{0, 8}}) {
		t.Fatalf("open events: %v", got)
	}
	if !OrdinalOpen(open, 4) || OrdinalOpen(open, 5) || OrdinalOpen(nil, 0) {
		t.Fatal("which records are open")
	}
	// The session is cut differently later: record 1 now belongs to a segment
	// whose commit was never pushed. What was open stays open.
	recut := anchoredPublicationBody()
	recut.Segments[0].LastOrdinal, recut.Segments[0].Last, recut.Segments[0].Events = 0, "m1", 3
	recut.Segments[1].FirstOrdinal, recut.Segments[1].First, recut.Segments[1].FirstEvent, recut.Segments[1].Events = 1, "m2", 3, 3
	if err := recut.Validate(); err != nil {
		t.Fatal(err)
	}
	kept, changed := AddPublishedRanges(open, recut.PublishableRanges(AnchoredPublicationFacts{OnRemote: anchoredRemoteHolding(anchoredTestSHA("a"))}), later)
	if changed || !reflect.DeepEqual(PublishedOrdinals(kept), [][2]int{{0, 5}}) {
		t.Fatalf("after the session was cut again: %+v %v", kept, changed)
	}
	for name, broken := range map[string]AnchoredPublishedRange{
		"no time":         {FirstOrdinal: 0, LastOrdinal: 1, By: AnchoredPublicationReason{Kind: AnchoredPublishedByCommit}},
		"ends before it":  {FirstOrdinal: 2, LastOrdinal: 1, At: first, By: AnchoredPublicationReason{Kind: AnchoredPublishedByCommit}},
		"an unknown kind": {FirstOrdinal: 0, LastOrdinal: 1, At: first, By: AnchoredPublicationReason{Kind: "guessed"}},
	} {
		if err := ValidateAnchoredPublishedRanges([]AnchoredPublishedRange{broken}); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

func TestOpenViewHoldsOnlyWhatIsOpen(t *testing.T) {
	at := time.Date(2031, 1, 3, 0, 0, 0, 0, time.UTC)
	body := anchoredPublicationBody()
	hash, _ := body.Hash()
	session := AnchoredSession{RepoID: HashContent([]byte("repo")), Provider: ProviderClaude, SessionID: "session-1", Uploader: "member-a", Revision: 2, BodyHash: hash, CreatedAt: at, UpdatedAt: at, Body: body}
	if _, ok := session.OpenView(nil); ok {
		t.Fatal("a session nothing is open of was shown")
	}
	commit := func(first, last, firstEvent, events int) AnchoredPublishedRange {
		return AnchoredPublishedRange{FirstOrdinal: first, LastOrdinal: last, FirstEvent: firstEvent, Events: events, At: at, By: AnchoredPublicationReason{Kind: AnchoredPublishedByCommit}}
	}
	// Only the second segment is open: the first keeps its place and says nothing.
	view, ok := session.OpenView([]AnchoredPublishedRange{commit(2, 3, 6, 0)})
	if !ok || len(view.Body.Segments) != 2 || !view.Body.Segments[0].Hidden || view.Body.Segments[0].First != "" || len(view.Body.Segments[0].Commits) != 0 ||
		view.Body.Segments[1].Hidden || view.Body.Segments[1].First != "m3" || len(view.Body.Segments[1].Commits) != 2 || view.Body.Uncommitted != nil {
		t.Fatalf("second segment open: %+v", view.Body)
	}
	// What is the uploader's alone is not in the view.
	if view.Uploader != "member-a" || view.Body.Machine != "" || view.Body.AgentCalls != nil || view.Body.Content == nil || len(view.Body.Content.Parts) != 0 ||
		view.Body.Content.Events != 10 || view.Body.Records != 2 || view.Body.EndedAt != nil {
		t.Fatalf("what the view carries: %+v", view.Body)
	}
	// The first segment's commit carries an author address in the report; the view does not.
	whole := []AnchoredPublishedRange{commit(0, 1, 0, 6), commit(2, 3, 6, 0),
		{FirstOrdinal: 4, LastOrdinal: 4, FirstEvent: 6, Events: 2, At: at, By: AnchoredPublicationReason{Kind: AnchoredPublishedByBranchPush, Branch: "main"}}}
	view, _ = session.OpenView(whole)
	if view.Body.Segments[0].Commits[0].AuthorEmail != "" || view.Body.Segments[0].Commits[0].Subject != "first" || session.Body.Segments[0].Commits[0].AuthorEmail == "" {
		t.Fatalf("commit facts in the view: %+v", view.Body.Segments[0].Commits)
	}
	// The uncommitted part is cut to its open beginning: places by branch only, no worktree, no turn marks.
	part := view.Body.Uncommitted
	if part == nil || !part.Partial || part.FirstOrdinal != 4 || part.LastOrdinal != 4 || part.FirstEvent != 6 || part.Events != 2 || part.First != "" || part.Turns != nil ||
		!reflect.DeepEqual(part.Places, []AnchoredPlace{{Branch: "main", Rule: AnchoredPlaceRecorded}}) {
		t.Fatalf("the open beginning of the uncommitted part: %+v", part)
	}
	// All of it open: the part is whole, still without worktree names.
	view, _ = session.OpenView(append(whole, AnchoredPublishedRange{FirstOrdinal: 4, LastOrdinal: 5, FirstEvent: 6, Events: 4, At: at, By: AnchoredPublicationReason{Kind: AnchoredPublishedByBranchPush, Branch: "main"}}))
	if part := view.Body.Uncommitted; part == nil || part.Partial || part.LastOrdinal != 5 || part.Events != 4 || part.Last != "m6" || len(part.Places) != 1 || part.Places[0].Worktree != "" {
		t.Fatalf("the whole uncommitted part: %+v", view.Body.Uncommitted)
	}
	// A range that covers only a piece of a segment shows no segment.
	if _, ok := session.OpenView([]AnchoredPublishedRange{commit(0, 0, 0, 3)}); ok {
		t.Fatal("a segment only partly open was shown")
	}

	summary, ok := session.OpenSummary([]AnchoredPublishedRange{commit(2, 3, 6, 0)})
	if !ok || !summary.Shared || summary.Uploader != "member-a" || summary.Segments != 1 || summary.Records != 2 || summary.Machine != "" || summary.Uncommitted != nil ||
		!reflect.DeepEqual(summary.Commits, []AnchoredSummaryCommit{{SHA: anchoredTestSHA("b"), SegmentNo: 2, Rule: AnchoredLinkFolderAndTime}, {SHA: anchoredTestSHA("c"), SegmentNo: 2, Rule: AnchoredLinkFolderAndTime}}) {
		t.Fatalf("summary of what is open: %+v", summary)
	}
	if own := session.Summary(); own.Shared || own.Uploader != "member-a" || own.Segments != 2 || own.Machine != "workstation" {
		t.Fatalf("the uploader's own summary: %+v", own)
	}
	// A report never carries the marks of a view.
	for name, change := range map[string]func(*AnchoredSessionBody){
		"a hidden segment": func(b *AnchoredSessionBody) { b.Segments[0].Hidden = true },
		"a partial rest":   func(b *AnchoredSessionBody) { b.Uncommitted.Partial = true },
	} {
		report := anchoredPublicationBody()
		change(&report)
		if err := report.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("a report with %s was accepted: %v", name, err)
		}
	}
}

// An event is read from the document that holds it, at its place within that
// document.
func TestAnchoredContentAccessFindsTheDocumentOfAnEvent(t *testing.T) {
	first, second, third := HashContent([]byte("first")), HashContent([]byte("second")), HashContent([]byte("third"))
	access := AnchoredContentAccess{Parts: []AnchoredContentPart{{Doc: first, Events: 4}, {Doc: second, Events: 1}, {Doc: third, Events: 5}}, Events: 10}
	for index, want := range map[int]struct {
		doc        ContentHash
		first, end int
		found      bool
	}{
		0: {first, 0, 4, true}, 3: {first, 0, 4, true}, 4: {second, 4, 5, true}, 5: {third, 5, 10, true}, 9: {third, 5, 10, true},
		10: {}, -1: {}, 1000: {},
	} {
		doc, begins, end, found := access.Part(index)
		if doc != want.doc || begins != want.first || end != want.end || found != want.found {
			t.Errorf("event %d: document %s, events [%d, %d), found %v", index, doc, begins, end, found)
		}
	}
	if _, _, _, found := (AnchoredContentAccess{}).Part(0); found {
		t.Fatal("a conversation without documents held an event")
	}
}
