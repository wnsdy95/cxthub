package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestAnchoredSearchQueryIsComparedInLowerCaseWithoutSurroundingSpace(t *testing.T) {
	if query, err := NormalizeAnchoredSearchQuery("  Parser FIX "); err != nil || query != "parser fix" {
		t.Fatalf("normalized: %q %v", query, err)
	}
	for _, raw := range []string{"", " ", "p", " p ", strings.Repeat("x", 257)} {
		if _, err := NormalizeAnchoredSearchQuery(raw); !errors.Is(err, ErrValidation) {
			t.Errorf("%q was accepted: %v", raw, err)
		}
	}
	if _, err := NormalizeAnchoredSearchQuery(strings.Repeat("x", 256)); err != nil {
		t.Fatalf("the longest query: %v", err)
	}
}

func TestAnchoredSnippetShowsTheTextAroundTheMatch(t *testing.T) {
	if snippet, ok := AnchoredSnippet("Fixed the Parser\n  today.", "parser"); !ok || snippet != "Fixed the Parser today." {
		t.Fatalf("a short text: %q %v", snippet, ok)
	}
	long := strings.Repeat("before ", 40) + "the needle is here " + strings.Repeat("after ", 40)
	snippet, ok := AnchoredSnippet(long, "needle")
	if !ok || !strings.Contains(snippet, "needle") || !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") || len([]rune(snippet)) > anchoredSnippetRunes+2 {
		t.Fatalf("a long text: %q %v", snippet, ok)
	}
	if _, ok := AnchoredSnippet("nothing of the kind", "needle"); ok {
		t.Fatal("a text without the query gave a snippet")
	}
}

func TestEventPartPlacesAnEventInTheSessionAsItIsGiven(t *testing.T) {
	body := AnchoredSessionBody{
		Segments: []AnchoredSegment{
			{AnchoredRange: AnchoredRange{FirstEvent: 0, Events: 2}},
			{AnchoredRange: AnchoredRange{FirstEvent: 2, Events: 2}},
		},
		Uncommitted: &AnchoredUncommitted{AnchoredRange: AnchoredRange{FirstEvent: 4, Events: 1}},
	}
	for event, want := range map[int][3]int{0: {1, 0, 1}, 1: {1, 0, 1}, 2: {2, 0, 1}, 3: {2, 0, 1}, 4: {0, 1, 1}, 5: {0, 0, 0}, -1: {0, 0, 0}} {
		segment, rest, ok := body.EventPart(event)
		got := [3]int{segment, 0, 0}
		if rest {
			got[1] = 1
		}
		if ok {
			got[2] = 1
		}
		if got != want {
			t.Errorf("event %d: %v, want %v", event, got, want)
		}
	}
	// The view another account is given holds no event of a part that is
	// not open.
	body.Segments[1] = AnchoredSegment{Commits: []AnchoredCommit{}, Hidden: true}
	body.Uncommitted = nil
	for _, event := range []int{2, 3, 4} {
		if _, _, ok := body.EventPart(event); ok {
			t.Errorf("event %d of a part that is not open was placed", event)
		}
	}
	if segment, _, ok := body.EventPart(1); !ok || segment != 1 {
		t.Fatalf("an open event: %d %v", segment, ok)
	}
}
