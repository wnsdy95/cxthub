package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestAnchoredSummaryOfKeepsWhatADigestSaysWithinTheServersLimits(t *testing.T) {
	body, said := AnchoredSummaryOf(MemoryDigest{Summary: "Added the parser.", KeyFacts: []string{" It reads TOML. ", "", "  "}, OpenTasks: nil})
	if !said || !reflect.DeepEqual(body, AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"It reads TOML."}, OpenTasks: []string{}}) {
		t.Fatalf("body: %+v %v", body, said)
	}
	if _, said := AnchoredSummaryOf(MemoryDigest{Summary: "  \n", KeyFacts: []string{" "}}); said {
		t.Fatal("a digest that says nothing gave a summary")
	}
	// Text is cut at the start of a character; what is not text is dropped.
	long := strings.Repeat("\uAC00", anchoredSummaryTextLimit)
	many := make([]string, anchoredSummaryItemsLimit+5)
	for index := range many {
		many[index] = strings.Repeat("\uB098", anchoredSummaryItemLimit)
	}
	body, said = AnchoredSummaryOf(MemoryDigest{Summary: long + "\x00\xff", KeyFacts: many, OpenTasks: []string{"zero\x00byte"}})
	if !said || len(body.Summary) > anchoredSummaryTextLimit || !strings.HasPrefix(long, body.Summary) || len(body.Summary) < anchoredSummaryTextLimit-3 {
		t.Fatalf("summary of %d bytes", len(body.Summary))
	}
	if len(body.KeyFacts) != anchoredSummaryItemsLimit || len(body.KeyFacts[0]) > anchoredSummaryItemLimit || !strings.HasPrefix(many[0], body.KeyFacts[0]) || body.OpenTasks[0] != "zerobyte" {
		t.Fatalf("items: %d facts, first of %d bytes, tasks %q", len(body.KeyFacts), len(body.KeyFacts[0]), body.OpenTasks)
	}
}

func TestAnchoredSummaryPointsAreTheEndsOfSegmentsThatEndAtACommit(t *testing.T) {
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	body := AnchoredSessionBody{Content: &AnchoredContent{Events: 9}, Segments: []AnchoredSegment{
		{AnchoredRange: AnchoredRange{FirstEvent: 0, Events: 4}, Commits: []AnchoredCommit{{SHA: a}}},
		{AnchoredRange: AnchoredRange{FirstEvent: 4, Events: 3}, Commits: []AnchoredCommit{{SHA: b}, {SHA: c}}},
	}}
	want := []AnchoredSummaryPoint{{Commits: []string{a}, End: 4}, {Commits: []string{b, c}, End: 7}}
	if got := AnchoredSummaryPoints(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("points: %+v", got)
	}
	// A report without its conversation, and a segment before any event, have no point.
	if got := AnchoredSummaryPoints(AnchoredSessionBody{Segments: body.Segments}); len(got) != 0 {
		t.Fatalf("points without content: %+v", got)
	}
	empty := AnchoredSessionBody{Content: &AnchoredContent{Events: 2}, Segments: []AnchoredSegment{{Commits: []AnchoredCommit{{SHA: a}}}}}
	if got := AnchoredSummaryPoints(empty); len(got) != 0 {
		t.Fatalf("points of a segment with no event before its end: %+v", got)
	}
	// What is remembered is what was done among the commits still named.
	done := map[string]struct{}{c: {}, a: {}, strings.Repeat("d", 40): {}}
	if got := AnchoredSummaryRemembered(want, done); !reflect.DeepEqual(got, []string{a, c}) {
		t.Fatalf("remembered: %v", got)
	}
}

// A session is given the newest summary another session wrote.
func TestBranchSummaryForStepsOverTheSessionsOwn(t *testing.T) {
	memory := AnchoredMemory{Summaries: []AnchoredSummary{
		{Provider: ProviderClaude, SessionID: "s1", Commit: "c3"},
		{Provider: ProviderCodex, SessionID: "s1", Commit: "c2"},
		{Provider: ProviderClaude, SessionID: "s2", Commit: "c1"},
	}}
	if got, ok := BranchSummaryFor(memory, ProviderClaude, "s1"); !ok || got.Commit != "c2" {
		t.Fatalf("the same ID of another tool is another session: %+v %v", got, ok)
	}
	if got, ok := BranchSummaryFor(memory, ProviderCodex, "s1"); !ok || got.Commit != "c3" {
		t.Fatalf("for the other tool: %+v %v", got, ok)
	}
	if got, ok := BranchSummaryFor(memory, ProviderClaude, "s3"); !ok || got.Commit != "c3" {
		t.Fatalf("for a session that wrote none: %+v %v", got, ok)
	}
	only := AnchoredMemory{Summaries: memory.Summaries[:1]}
	if _, ok := BranchSummaryFor(only, ProviderClaude, "s1"); ok {
		t.Fatal("a session was given what it wrote itself")
	}
	if _, ok := BranchSummaryFor(AnchoredMemory{}, ProviderClaude, "s1"); ok {
		t.Fatal("a summary where there is none")
	}
}

// The mark of the tool's own task list goes with the tasks, and only with them.
func TestAnchoredSummaryOfKeepsTheMarkOfTheToolsOwnTasks(t *testing.T) {
	if body, _ := AnchoredSummaryOf(MemoryDigest{Summary: "Work.", OpenTasks: []string{"Document it."}, TasksAuthoritative: true}); !body.TasksAuthoritative {
		t.Fatalf("the mark was lost: %+v", body)
	}
	if body, _ := AnchoredSummaryOf(MemoryDigest{Summary: "Work.", OpenTasks: []string{"Document it."}}); body.TasksAuthoritative {
		t.Fatalf("a list cxt picked out was marked: %+v", body)
	}
	if body, _ := AnchoredSummaryOf(MemoryDigest{Summary: "Work.", TasksAuthoritative: true}); body.TasksAuthoritative {
		t.Fatalf("no task, and a mark: %+v", body)
	}
}
