package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAnchoredSummaryBodySaysSomethingWithinItsLimits(t *testing.T) {
	valid := AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"The parser reads TOML."}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (AnchoredSummaryBody{OpenTasks: []string{"Document the parser."}}).Validate(); err != nil {
		t.Fatalf("a summary of open tasks only: %v", err)
	}
	many := make([]string, anchoredSummaryItemsLimit+1)
	for index := range many {
		many[index] = "fact"
	}
	for name, body := range map[string]AnchoredSummaryBody{
		"nothing said":        {},
		"only space":          {Summary: "  \n"},
		"an empty item":       {Summary: "x", KeyFacts: []string{" "}},
		"a summary too long":  {Summary: strings.Repeat("x", anchoredSummaryTextLimit+1)},
		"an item too long":    {Summary: "x", OpenTasks: []string{strings.Repeat("x", anchoredSummaryItemLimit+1)}},
		"too many items":      {Summary: "x", KeyFacts: many},
		"text that is not":    {Summary: "bad \xff bytes"},
		"a zero byte in text": {Summary: "zero \x00 byte"},
	} {
		if err := body.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

func TestAnchoredSummaryHashIsOfItsCanonicalForm(t *testing.T) {
	absent, err := (AnchoredSummaryBody{Summary: "Added the parser."}).Hash()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := (AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{}, OpenTasks: []string{}}).Hash()
	if err != nil || absent != empty {
		t.Fatalf("a list that is absent and one that is empty: %s %s %v", absent, empty, err)
	}
	other, err := (AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"fact"}}).Hash()
	if err != nil || other == absent {
		t.Fatalf("other text has the same hash: %v", err)
	}
	at := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	stored := AnchoredSummary{RepoID: HashContent([]byte("repo")), Provider: ProviderClaude, SessionID: "session-1", Commit: strings.Repeat("a", 40),
		BodyHash: absent, Body: AnchoredSummaryBody{Summary: "Added the parser."}, CreatedAt: at, UpdatedAt: at}
	if err := stored.Validate(); err != nil {
		t.Fatal(err)
	}
	stored.Uploader = "member-a"
	if err := stored.Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("a summary carrying what is only read: %v", err)
	}
}

func TestSegmentOfCommitFindsTheOpenSegmentThatEndsThere(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	body := AnchoredSessionBody{Segments: []AnchoredSegment{
		{Commits: []AnchoredCommit{{SHA: a}}},
		{Commits: []AnchoredCommit{{SHA: b}, {SHA: strings.Repeat("c", 40)}}},
	}}
	if body.SegmentOfCommit(a) != 1 || body.SegmentOfCommit(b) != 2 || body.SegmentOfCommit(strings.Repeat("c", 40)) != 2 || body.SegmentOfCommit(strings.Repeat("d", 40)) != 0 {
		t.Fatal("a commit was not found at its segment")
	}
	body.Segments[1] = AnchoredSegment{Commits: []AnchoredCommit{}, Hidden: true}
	if body.SegmentOfCommit(b) != 0 {
		t.Fatal("a segment that is not open was found")
	}
}
