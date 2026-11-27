package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// anchoredSearchDoc stores a content document whose events say the texts.
func anchoredSearchDoc(t *testing.T, fixture *remoteGitTestFixture, texts ...string) domain.ContentHash {
	t.Helper()
	return anchoredConversationDoc(t, fixture, 0, texts...)
}

// anchoredConversationParts stores a conversation of one message for each
// text as documents of the given sizes, one after another, and returns them
// as the parts of a report's content.
func anchoredConversationParts(t *testing.T, fixture *remoteGitTestFixture, texts []string, sizes ...int) *domain.AnchoredContent {
	t.Helper()
	content := &domain.AnchoredContent{Events: len(texts)}
	first := 0
	for _, size := range sizes {
		content.Parts = append(content.Parts, domain.AnchoredContentPart{Doc: anchoredConversationDoc(t, fixture, first, texts[first:first+size]...), Events: size})
		first += size
	}
	if first != len(texts) {
		t.Fatalf("the sizes hold %d events, the conversation has %d", first, len(texts))
	}
	return content
}

// anchoredConversationDoc stores a document of one message for each text;
// the first message is the one at position first of its conversation.
func anchoredConversationDoc(t *testing.T, fixture *remoteGitTestFixture, first int, texts ...string) domain.ContentHash {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion, doc.CIR.Envelope.SourceProvider = "1", "claude"
	for index, text := range texts {
		role := domain.Role("user")
		if (first+index)%2 == 1 {
			role = "assistant"
		}
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: first + index, Role: role, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := fixture.storage.PutDoc(systemTestContext(), fixture.repo, doc); err != nil {
		t.Fatal(err)
	}
	return doc.Hash
}

// anchoredSearchKinds lists the hits as "kind:what", sorted.
func anchoredSearchKinds(result domain.AnchoredSearchResult) []string {
	listed := []string{}
	for _, hit := range result.Hits {
		what := hit.Title
		switch hit.Kind {
		case domain.AnchoredHitSegment:
			what = hit.Snippet
		case domain.AnchoredHitNote:
			what = hit.Snippet
		}
		listed = append(listed, hit.Kind+":"+what)
	}
	sort.Strings(listed)
	return listed
}

// One query is looked for in pull requests, branches, commits, conversations
// and notes, and a reader is answered only from what that reader may read.
// A conversation kept in several documents is searched as the same
// conversation kept in one.
func TestSearchFindsWhatTheReaderMayRead(t *testing.T) {
	for name, sizes := range map[string][]int{"in one document": {5}, "in three documents": {2, 1, 2}, "a document for each event": {1, 1, 1, 1, 1}} {
		t.Run(name, func(t *testing.T) { searchFindsWhatTheReaderMayRead(t, sizes) })
	}
}

func searchFindsWhatTheReaderMayRead(t *testing.T, sizes []int) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob, visitor := anchoredActor("member-a"), anchoredActor("member-b"), context.Background()
	pushed, local := provider.chain(1, ""), anchoredTestSHA("b")
	commit := provider.commits[pushed]
	commit.Subject = "Add the Parser"
	provider.commits[pushed] = commit
	provider.heads["main"], provider.heads["parser-work"] = pushed, pushed
	provider.pull(7, 60, pushed, pushed)
	pull := provider.pulls[7]
	pull.Title = "Parser fixes"
	provider.pulls[7] = pull
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	texts := []string{"Plan the parser.", "The parser is committed.", "A hidden parser idea.", "Nothing here.", "Parser leftovers."}
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 5, Machine: "laptop",
		Content: anchoredConversationParts(t, fixture, texts, sizes...),
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber, Subject: "Add the Parser"}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3, FirstEvent: 2, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber, Subject: "Parser rework"}}},
		},
		Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m5", Last: "m5", FirstOrdinal: 4, LastOrdinal: 4, FirstEvent: 4, Events: 1},
			Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "topic", Rule: domain.AnchoredPlaceRecorded}}},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	for target, text := range map[domain.AnchoredNoteTarget]string{
		{Kind: domain.AnchoredNoteCommit, Commit: pushed}:                       "The parser was reviewed.",
		{Kind: domain.AnchoredNoteWorktree, Worktree: "app", Machine: "laptop"}: "Parser scratch.",
	} {
		if _, err := service.CreateAnchoredNote(alice, repo, target, text); err != nil {
			t.Fatal(err)
		}
	}

	// The uploader finds everything; one session gives at most three hits.
	own, err := service.SearchAnchored(alice, repo, "  PARSER ")
	if err != nil {
		t.Fatal(err)
	}
	wantOwn := []string{
		"branch:parser-work", "commit:Add the Parser", "commit:Parser rework", "note:Parser scratch.", "note:The parser was reviewed.", "pull:Parser fixes",
		"segment:A hidden parser idea.", "segment:Plan the parser.", "segment:The parser is committed.",
	}
	if got := anchoredSearchKinds(own); strings.Join(got, "|") != strings.Join(wantOwn, "|") || !own.Truncated {
		t.Fatalf("what the uploader finds: %v truncated=%v", got, own.Truncated)
	}
	for _, hit := range own.Hits {
		switch {
		case hit.Kind == domain.AnchoredHitSegment && (hit.Provider != domain.ProviderClaude || hit.SessionID != "session-1" || hit.Event == nil):
			t.Fatalf("a conversation hit does not say where it is: %+v", hit)
		// The event is numbered by its place in the conversation, whichever document holds it.
		case hit.Kind == domain.AnchoredHitSegment && (*hit.Event < 0 || *hit.Event >= len(texts) || texts[*hit.Event] != hit.Snippet):
			t.Fatalf("a conversation hit names another event than the one it shows: %+v", hit)
		case hit.Kind == domain.AnchoredHitSegment && strings.HasPrefix(hit.Snippet, "A hidden") && (hit.SegmentNo != 2 || hit.Rest || hit.Title != "user"):
			t.Fatalf("the hit of the second segment: %+v", hit)
		case hit.Kind == domain.AnchoredHitPull && (hit.Pull != 7 || hit.Branch != "topic-7"):
			t.Fatalf("the pull request hit: %+v", hit)
		case hit.Kind == domain.AnchoredHitNote && (hit.NoteID == "" || hit.Target == nil):
			t.Fatalf("a note hit does not say what it is about: %+v", hit)
		}
	}
	if rest, err := service.SearchAnchored(alice, repo, "leftovers"); err != nil || len(rest.Hits) != 1 || !rest.Hits[0].Rest || rest.Hits[0].SegmentNo != 0 || rest.Truncated {
		t.Fatalf("a hit in the part after the last commit: %+v %v", rest, err)
	}

	// Anyone else finds what is pushed: not the local commit, not the second
	// segment, not the part after the last commit, not the worktree's note.
	wantOpen := []string{"branch:parser-work", "commit:Add the Parser", "note:The parser was reviewed.", "pull:Parser fixes", "segment:Plan the parser.", "segment:The parser is committed."}
	for name, reader := range map[string]context.Context{"another member": bob, "a reader without an account": visitor} {
		found, err := service.SearchAnchored(reader, repo, "parser")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := anchoredSearchKinds(found); strings.Join(got, "|") != strings.Join(wantOpen, "|") || found.Truncated {
			t.Fatalf("what %s finds: %v truncated=%v", name, got, found.Truncated)
		}
		for _, query := range []string{"hidden", "leftovers", "rework", "scratch"} {
			if found, err := service.SearchAnchored(reader, repo, query); err != nil || len(found.Hits) != 0 {
				t.Fatalf("%s found %q, which is not pushed: %+v %v", name, query, found.Hits, err)
			}
		}
	}

	if _, err := service.SearchAnchored(alice, repo, "p"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a one-letter query: %v", err)
	}
	if _, err := service.SearchAnchored(systemTestContext(), repo, "parser"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a search by the server itself: %v", err)
	}
}
