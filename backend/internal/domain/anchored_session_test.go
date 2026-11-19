package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func anchoredTestTime(second int) *time.Time {
	at := time.Date(2031, 1, 2, 3, 0, second, 0, time.UTC)
	return &at
}

func anchoredTestSHA(letter string) string { return strings.Repeat(letter, 40) }

// anchoredTestBody is a well-formed report: two segments and an uncommitted
// part covering six records.
func anchoredTestBody() AnchoredSessionBody {
	return AnchoredSessionBody{
		RuleVersion: AnchoredSessionRuleVersion, Scope: AnchoredScopeFolder, Machine: "workstation",
		StartedAt: anchoredTestTime(0), EndedAt: anchoredTestTime(50), Records: 6, Commands: 3,
		Segments: []AnchoredSegment{
			{AnchoredRange: AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, Commands: 1, StartedAt: anchoredTestTime(0), EndedAt: anchoredTestTime(10)},
				Commits: []AnchoredCommit{{SHA: anchoredTestSHA("a"), Rule: AnchoredLinkPrintedNumber, CreatedAt: anchoredTestTime(9), AuthoredAt: anchoredTestTime(9), AuthorEmail: "dev@example.test", Subject: "first"}}},
			{AnchoredRange: AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3, Commands: 1}, ClosesTurn: true,
				Commits: []AnchoredCommit{{SHA: anchoredTestSHA("b"), Rule: AnchoredLinkFolderAndTime}, {SHA: anchoredTestSHA("c"), Rule: AnchoredLinkFolderAndTime}}},
		},
		Uncommitted: &AnchoredUncommitted{AnchoredRange: AnchoredRange{First: "m5", Last: "m6", FirstOrdinal: 4, LastOrdinal: 5, Commands: 1},
			Places: []AnchoredPlace{{Worktree: "app", Branch: "main", Rule: AnchoredPlaceRecorded}, {Worktree: "app-detached", Rule: AnchoredPlaceHeadLog}}},
		AgentCalls: []AnchoredAgentCall{{Tool: "sendInput", From: "lead", To: []string{"worker"}, Status: "completed", At: anchoredTestTime(20), Record: "t1/i1"}},
	}
}

func TestAnchoredSessionBodyValidates(t *testing.T) {
	if err := anchoredTestBody().Validate(); err != nil {
		t.Fatalf("a well-formed report was rejected: %v", err)
	}
	empty := AnchoredSessionBody{RuleVersion: AnchoredSessionRuleVersion, Scope: AnchoredScopeOriginURL}
	if err := empty.Validate(); err != nil {
		t.Fatalf("a session with no records was rejected: %v", err)
	}
	sub := anchoredTestBody()
	sub.Sub, sub.Parent, sub.Spawn = true, "parent-session", &AnchoredSpawn{AtRecord: "p1", Rule: AnchoredSpawnToolCall}
	if err := sub.Validate(); err != nil {
		t.Fatalf("a spawned session was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*AnchoredSessionBody){
		"an unknown rule version":        func(b *AnchoredSessionBody) { b.RuleVersion = 2 },
		"an unknown scope":               func(b *AnchoredSessionBody) { b.Scope = "elsewhere" },
		"a parent that is not an id":     func(b *AnchoredSessionBody) { b.Parent = "../other" },
		"a spawn point without a parent": func(b *AnchoredSessionBody) { b.Spawn = &AnchoredSpawn{AtRecord: "p1", Rule: AnchoredSpawnToolCall} },
		"a spawn point with an unknown rule": func(b *AnchoredSessionBody) {
			b.Parent, b.Spawn = "parent", &AnchoredSpawn{AtRecord: "p1", Rule: "guessed"}
		},
		"a spawn point before the first record": func(b *AnchoredSessionBody) {
			b.Parent, b.Spawn = "parent", &AnchoredSpawn{AtRecord: "p1", AtOrdinal: -1, Rule: AnchoredSpawnToolCall}
		},
		"a negative count":                     func(b *AnchoredSessionBody) { b.CopiedRecords = -1 },
		"a copied-from entry that is no id":    func(b *AnchoredSessionBody) { b.CopiedFrom = []string{"a b"} },
		"a gap between segments":               func(b *AnchoredSessionBody) { b.Segments[1].FirstOrdinal = 3 },
		"overlapping segments":                 func(b *AnchoredSessionBody) { b.Segments[1].FirstOrdinal = 1 },
		"a segment that ends before it starts": func(b *AnchoredSessionBody) { b.Segments[0].LastOrdinal = -1 },
		"a segment without a commit":           func(b *AnchoredSessionBody) { b.Segments[0].Commits = nil },
		"a short commit number":                func(b *AnchoredSessionBody) { b.Segments[0].Commits[0].SHA = "aaaaaaa" },
		"an upper-case commit number":          func(b *AnchoredSessionBody) { b.Segments[0].Commits[0].SHA = strings.Repeat("A", 40) },
		"an unknown link rule":                 func(b *AnchoredSessionBody) { b.Segments[0].Commits[0].Rule = "guessed" },
		"a commit named twice":                 func(b *AnchoredSessionBody) { b.Segments[1].Commits[1].SHA = anchoredTestSHA("a") },
		"a segment without its first record":   func(b *AnchoredSessionBody) { b.Segments[0].First = "" },
		"a segment that ends before starting": func(b *AnchoredSessionBody) {
			b.Segments[0].EndedAt = anchoredTestTime(0)
			b.Segments[0].StartedAt = anchoredTestTime(5)
		},
		"an uncommitted part that skips":      func(b *AnchoredSessionBody) { b.Uncommitted.FirstOrdinal = 5 },
		"records the parts do not cover":      func(b *AnchoredSessionBody) { b.Records = 7 },
		"a worktree path instead of a name":   func(b *AnchoredSessionBody) { b.Uncommitted.Places[0].Worktree = "/home/dev/app" },
		"a place with an unknown rule":        func(b *AnchoredSessionBody) { b.Uncommitted.Places[0].Rule = "guessed" },
		"unplaced with a place":               func(b *AnchoredSessionBody) { b.Uncommitted.Unplaced = true },
		"placed without a place":              func(b *AnchoredSessionBody) { b.Uncommitted.Places = nil },
		"an agent call without a sender":      func(b *AnchoredSessionBody) { b.AgentCalls[0].From = "" },
		"an agent call to nobody named":       func(b *AnchoredSessionBody) { b.AgentCalls[0].To = []string{""} },
		"a machine label with a control code": func(b *AnchoredSessionBody) { b.Machine = "work\nstation" },
	} {
		t.Run(name, func(t *testing.T) {
			body := anchoredTestBody().Canonical()
			mutate(&body)
			if err := body.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("%s was accepted: %v", name, err)
			}
		})
	}
}

func TestAnchoredSessionBodyHashIsTheSameForTheSameContent(t *testing.T) {
	body := anchoredTestBody()
	want, err := body.Hash()
	if err != nil {
		t.Fatal(err)
	}
	// The same instants in another zone, and a report that went through JSON.
	zone := time.FixedZone("east", 9*3600)
	shifted := anchoredTestBody()
	moved := shifted.StartedAt.In(zone)
	shifted.StartedAt = &moved
	created := shifted.Segments[0].Commits[0].CreatedAt.In(zone)
	shifted.Segments[0].Commits[0].CreatedAt = &created
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AnchoredSessionBody
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]AnchoredSessionBody{"another zone": shifted, "through JSON": decoded, "canonical": body.Canonical()} {
		if got, err := other.Hash(); err != nil || got != want {
			t.Fatalf("%s: hash %s, want %s (%v)", name, got, want, err)
		}
	}
	changed := anchoredTestBody()
	changed.Segments[0].Commits[0].Rule = AnchoredLinkFolderAndTime
	if got, _ := changed.Hash(); got == want {
		t.Fatal("a different rule gave the same hash")
	}
	if !reflect.DeepEqual(body, anchoredTestBody()) {
		t.Fatal("hashing changed the report")
	}
}

func TestAnchoredSessionCommitLinksAndKeys(t *testing.T) {
	links := anchoredTestBody().CommitLinks(ProviderClaude, "session-1")
	want := []AnchoredCommitLink{
		{Provider: ProviderClaude, SessionID: "session-1", CommitSHA: anchoredTestSHA("a"), SegmentNo: 1, Rule: AnchoredLinkPrintedNumber},
		{Provider: ProviderClaude, SessionID: "session-1", CommitSHA: anchoredTestSHA("b"), SegmentNo: 2, Rule: AnchoredLinkFolderAndTime},
		{Provider: ProviderClaude, SessionID: "session-1", CommitSHA: anchoredTestSHA("c"), SegmentNo: 2, Rule: AnchoredLinkFolderAndTime},
	}
	if !reflect.DeepEqual(links, want) {
		t.Fatalf("links = %+v", links)
	}
	for id, valid := range map[string]bool{"0f4b3c2a-1d5e-4f60-8a7b-9c0d1e2f3a4b": true, "a082eeddec4163829": true, "": false, "a/b": false, "a b": false, strings.Repeat("x", 129): false} {
		if ValidAnchoredSessionKey(ProviderCodex, id) != valid {
			t.Errorf("session id %q valid = %v", id, !valid)
		}
	}
	if ValidAnchoredSessionKey("other", "session-1") {
		t.Error("an unknown tool was accepted")
	}
	if !ValidAnchoredCommitSHA(strings.Repeat("a", 64)) || ValidAnchoredCommitSHA(strings.Repeat("a", 41)) {
		t.Error("commit number lengths")
	}
}

func TestAnchoredSessionSummaryListsCommitsAndPlacesWithoutRanges(t *testing.T) {
	updated := *anchoredTestTime(59)
	body := anchoredTestBodyWithContent()
	hash, err := body.Hash()
	if err != nil {
		t.Fatal(err)
	}
	session := AnchoredSession{Provider: ProviderClaude, SessionID: "session-1", Revision: 3, BodyHash: hash, UpdatedAt: updated, Body: body}
	want := AnchoredSessionSummary{Provider: ProviderClaude, SessionID: "session-1", Revision: 3, BodyHash: hash, Records: 6, Commands: 3, Segments: 2,
		Machine: "workstation", StartedAt: anchoredTestTime(0), EndedAt: anchoredTestTime(50), Content: true, UpdatedAt: updated,
		Commits: []AnchoredSummaryCommit{
			{SHA: anchoredTestSHA("a"), SegmentNo: 1, Rule: AnchoredLinkPrintedNumber, Subject: "first", CreatedAt: anchoredTestTime(9), AuthoredAt: anchoredTestTime(9)},
			{SHA: anchoredTestSHA("b"), SegmentNo: 2, Rule: AnchoredLinkFolderAndTime},
			{SHA: anchoredTestSHA("c"), SegmentNo: 2, Rule: AnchoredLinkFolderAndTime},
		},
		Uncommitted: &AnchoredSummaryUncommitted{Commands: 1,
			Places: []AnchoredPlace{{Worktree: "app", Branch: "main", Rule: AnchoredPlaceRecorded}, {Worktree: "app-detached", Rule: AnchoredPlaceHeadLog}}},
	}
	if got := session.Summary(); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v", got)
	}
	// A session that committed nothing and has no content still lists as
	// arrays, so a reader never meets null.
	bare := AnchoredSession{Provider: ProviderCodex, SessionID: "session-2", Body: AnchoredSessionBody{RuleVersion: AnchoredSessionRuleVersion, Scope: AnchoredScopeFolder}}
	encoded, err := json.Marshal(bare.Summary())
	if err != nil {
		t.Fatal(err)
	}
	if text := string(encoded); !strings.Contains(text, `"commits":[]`) || !strings.Contains(text, `"content":false`) || strings.Contains(text, "uncommitted") {
		t.Fatalf("bare summary = %s", text)
	}
	// The summary never carries an author's address or an agent call.
	if text, _ := json.Marshal(session.Summary()); strings.Contains(string(text), "example.test") || strings.Contains(string(text), "sendInput") {
		t.Fatalf("summary leaks report details: %s", text)
	}
}

func TestAnchoredSpawnPointIsPlacedInTheParentAsItIsCutNow(t *testing.T) {
	parent := anchoredTestBody()
	for ordinal, want := range map[int]AnchoredSummarySpawn{
		0: {SegmentNo: 1, Rule: AnchoredSpawnToolCall},
		3: {SegmentNo: 2, Rule: AnchoredSpawnToolCall},
		5: {Uncommitted: true, Rule: AnchoredSpawnToolCall},
	} {
		got, found := parent.SpawnedIn(AnchoredSpawn{AtRecord: "p", AtOrdinal: ordinal, Rule: AnchoredSpawnToolCall})
		if !found || got != want {
			t.Errorf("record %d placed at %+v (%v), want %+v", ordinal, got, found, want)
		}
	}
	if _, found := parent.SpawnedIn(AnchoredSpawn{AtRecord: "p", AtOrdinal: 6, Rule: AnchoredSpawnToolCall}); found {
		t.Error("a record the parent no longer holds was placed")
	}
}

func TestAnchoredTurnMarksGoForwardThroughTheUncommittedPart(t *testing.T) {
	// The uncommitted part of the well-formed report holds records 4 and 5
	// and, with content, events 6 to 9.
	marks := func(turns ...AnchoredTurnMark) AnchoredSessionBody {
		body := anchoredTestBodyWithContent()
		body.Uncommitted.Turns = turns
		return body
	}
	for name, body := range map[string]AnchoredSessionBody{
		"no marks":                   marks(),
		"one turn":                   marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6, StartedAt: anchoredTestTime(40)}),
		"two turns":                  marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6}, AnchoredTurnMark{FirstOrdinal: 5, FirstEvent: 8}),
		"a turn that holds no event": marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6}, AnchoredTurnMark{FirstOrdinal: 5, FirstEvent: 6}),
		"a last turn at the end":     marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6}, AnchoredTurnMark{FirstOrdinal: 5, FirstEvent: 10}),
	} {
		if err := body.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, body := range map[string]AnchoredSessionBody{
		"a first turn after the first record": marks(AnchoredTurnMark{FirstOrdinal: 5, FirstEvent: 6}),
		"a first turn after the first event":  marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 7}),
		"a turn at the same record again":     marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6}, AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 7}),
		"a turn beyond the part":              marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6}, AnchoredTurnMark{FirstOrdinal: 6, FirstEvent: 7}),
		"events going back":                   marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 8}, AnchoredTurnMark{FirstOrdinal: 5, FirstEvent: 7}),
		"an event beyond the part":            marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6}, AnchoredTurnMark{FirstOrdinal: 5, FirstEvent: 11}),
	} {
		if err := body.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	// Without content no turn names an event.
	plain := anchoredTestBody()
	plain.Uncommitted.Turns = []AnchoredTurnMark{{FirstOrdinal: 4}, {FirstOrdinal: 5}}
	if err := plain.Validate(); err != nil {
		t.Errorf("turns without content: %v", err)
	}
	plain.Uncommitted.Turns[1].FirstEvent = 1
	if err := plain.Validate(); !errors.Is(err, ErrValidation) {
		t.Errorf("a turn naming an event without content was accepted: %v", err)
	}
	// The marks are part of what is stored and hashed, with their times in UTC.
	local := time.FixedZone("local", 9*3600)
	at := anchoredTestTime(40).In(local)
	zoned := marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6, StartedAt: &at})
	same := marks(AnchoredTurnMark{FirstOrdinal: 4, FirstEvent: 6, StartedAt: anchoredTestTime(40)})
	a, _ := zoned.Hash()
	b, _ := same.Hash()
	c, _ := marks().Hash()
	if a != b || a == c || zoned.Canonical().Uncommitted.Turns[0].StartedAt.Location() != time.UTC {
		t.Errorf("hashes: zoned %s, utc %s, without marks %s", a, b, c)
	}
}

// anchoredTestBodyWithContent places the three ranges of the well-formed
// report in a content document of ten events; the second range holds none.
func anchoredTestBodyWithContent() AnchoredSessionBody {
	body := anchoredTestBody()
	body.Content = &AnchoredContent{Doc: HashContent([]byte("synthetic content")), Events: 10}
	body.Segments[0].FirstEvent, body.Segments[0].Events = 0, 6
	body.Segments[1].FirstEvent, body.Segments[1].Events = 6, 0
	body.Uncommitted.FirstEvent, body.Uncommitted.Events = 6, 4
	return body
}

func TestAnchoredSessionContentCoversItsDocumentExactly(t *testing.T) {
	if err := anchoredTestBodyWithContent().Validate(); err != nil {
		t.Fatalf("a well-formed report with content was rejected: %v", err)
	}
	root := anchoredTestBodyWithContent()
	root.Content.Identity = DocumentIdentityRootV1
	if err := root.Validate(); err != nil {
		t.Fatalf("a root document was rejected: %v", err)
	}
	for name, change := range map[string]func(*AnchoredSessionBody){
		"a document that is not a hash":       func(b *AnchoredSessionBody) { b.Content.Doc = "content" },
		"an unknown identity":                 func(b *AnchoredSessionBody) { b.Content.Identity = "another-identity" },
		"a document without events":           func(b *AnchoredSessionBody) { b.Content.Events = 0 },
		"more events than a document holds":   func(b *AnchoredSessionBody) { b.Content.Events = 100_000_001 },
		"a gap between two ranges":            func(b *AnchoredSessionBody) { b.Segments[1].FirstEvent = 7 },
		"an overlap of two ranges":            func(b *AnchoredSessionBody) { b.Uncommitted.FirstEvent = 5 },
		"a range that does not start at zero": func(b *AnchoredSessionBody) { b.Segments[0].FirstEvent = 1 },
		"a negative event count":              func(b *AnchoredSessionBody) { b.Segments[1].Events = -1 },
		"ranges shorter than the document":    func(b *AnchoredSessionBody) { b.Uncommitted.Events = 3 },
		"ranges longer than the document":     func(b *AnchoredSessionBody) { b.Uncommitted.Events = 5 },
		"events without content":              func(b *AnchoredSessionBody) { b.Content = nil },
	} {
		body := anchoredTestBodyWithContent()
		change(&body)
		if err := body.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	// Content is part of what the report says: another document is another report.
	first, second := anchoredTestBodyWithContent(), anchoredTestBodyWithContent()
	second.Content.Doc = HashContent([]byte("another document"))
	a, errA := first.Canonical().Hash()
	b, errB := second.Canonical().Hash()
	plain, errC := anchoredTestBody().Canonical().Hash()
	if errA != nil || errB != nil || errC != nil || a == b || a == plain {
		t.Fatalf("hashes: %s %s %s (%v %v %v)", a, b, plain, errA, errB, errC)
	}
	// The canonical form does not share the caller's content value.
	canonical := first.Canonical()
	canonical.Content.Events = 99
	if first.Content.Events != 10 {
		t.Fatal("the canonical form shares its content with the report it was made from")
	}
}
