package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// An archived session is left out of every list, search and read along a
// branch, for its uploader too, and is still opened by its key. Its uploader
// archives and restores it, and so does whoever manages the repository.
func TestArchivedSessionIsLeftOutOfListsAndStillOpened(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob, manager, visitor := anchoredActor("member-a"), anchoredActor("member-b"), anchoredActor("manager"), context.Background()
	current, err := fixture.storage.GetRepo(systemTestContext(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.AddMember(systemTestContext(), domain.Membership{RepositoryID: current.RepositoryID, UserID: "manager", Role: domain.RoleMaintainer, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	pushed := provider.chain(1, "")
	provider.heads["main"] = pushed
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	talked := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 2,
		Content: &domain.AnchoredContent{Events: 2, Parts: []domain.AnchoredContentPart{{Doc: anchoredSearchDoc(t, fixture, "Plan the parser.", "The parser is committed."), Events: 2}}},
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
			Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}}},
	}
	quiet := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 2,
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1},
			Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}}},
	}
	for id, body := range map[string]domain.AnchoredSessionBody{"session-1": talked, "session-2": quiet} {
		if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, id, body); err != nil {
			t.Fatal(err)
		}
	}
	for id, text := range map[string]string{"session-1": "Added the parser.", "session-2": "Reviewed the parser."} {
		if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, id, pushed, domain.AnchoredSummaryBody{Summary: text}); err != nil {
			t.Fatal(err)
		}
	}
	fixture.nextPass(t)

	// seen says what a reader finds: the sessions listed as archived, and
	// how many sessions the commit, its summaries, the branch and a search
	// for a word of the conversation give.
	type found struct{ sessions, archived, segments, summaries, memory, hits int }
	seen := func(reader context.Context) found {
		t.Helper()
		var result found
		listed, err := service.ListAnchoredSessions(reader, repo)
		if err != nil {
			t.Fatal(err)
		}
		result.sessions = len(listed)
		for _, session := range listed {
			if session.ArchivedAt != nil {
				if session.SessionID != "session-1" {
					t.Fatalf("the wrong session is archived: %+v", session)
				}
				result.archived++
			}
		}
		segments, err := service.AnchoredCommitSegments(reader, repo, pushed)
		if err != nil {
			t.Fatal(err)
		}
		summaries, err := service.ListAnchoredSummaries(reader, repo, []string{pushed})
		if err != nil {
			t.Fatal(err)
		}
		memory, err := service.ReadAnchoredMemory(reader, repo, "main", []string{pushed}, 5)
		if err != nil {
			t.Fatal(err)
		}
		search, err := service.SearchAnchored(reader, repo, "parser")
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range search.Hits {
			if hit.Kind == domain.AnchoredHitSegment {
				result.hits++
			}
		}
		result.segments, result.summaries, result.memory = len(segments), len(summaries), len(memory.Summaries)
		return result
	}
	readers := map[string]context.Context{"the uploader": alice, "another member": bob, "a reader without an account": visitor}
	for name, reader := range readers {
		if got := seen(reader); got != (found{sessions: 2, segments: 2, summaries: 2, memory: 2, hits: 2}) {
			t.Fatalf("what %s finds before: %+v", name, got)
		}
	}

	// Who may archive. A member who is neither is refused the same way for a
	// session that exists and for one that does not.
	for _, id := range []string{"session-1", "missing"} {
		if _, err := service.SetAnchoredSessionArchived(bob, repo, domain.ProviderClaude, id, true); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("another member archiving %s: %v", id, err)
		}
	}
	if _, err := service.SetAnchoredSessionArchived(visitor, repo, domain.ProviderClaude, "session-1", true); err == nil {
		t.Fatal("a reader without an account archived a session")
	}
	if _, err := service.SetAnchoredSessionArchived(manager, repo, domain.ProviderClaude, "missing", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a manager archiving a session that is not stored: %v", err)
	}
	if _, err := service.SetAnchoredSessionArchived(alice, repo, domain.ProviderClaude, "a/b", true); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a key that is not one: %v", err)
	}
	if got := seen(bob); got.archived != 0 {
		t.Fatalf("a refused request archived: %+v", got)
	}

	receipt, err := service.SetAnchoredSessionArchived(alice, repo, domain.ProviderClaude, "session-1", true)
	if err != nil || !receipt.Archived || receipt.ArchivedAt == nil {
		t.Fatalf("the uploader archiving: %+v %v", receipt, err)
	}
	if again, err := service.SetAnchoredSessionArchived(manager, repo, domain.ProviderClaude, "session-1", true); err != nil || !again.Archived || again.ArchivedAt == nil || !again.ArchivedAt.Equal(*receipt.ArchivedAt) {
		t.Fatalf("archiving what is archived: %+v %v", again, err)
	}
	for name, reader := range readers {
		if got := seen(reader); got != (found{sessions: 2, archived: 1, segments: 1, summaries: 1, memory: 1}) {
			t.Fatalf("what %s finds of an archived session: %+v", name, got)
		}
	}
	// It is still opened by its key; only its uploader is told who archived it.
	if own, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "session-1"); err != nil || own.ArchivedAt == nil || own.ArchivedBy != "member-a" {
		t.Fatalf("the uploader opening it: %+v %v", own, err)
	}
	if shared, err := service.GetAnchoredSession(bob, repo, domain.ProviderClaude, "session-1"); err != nil || shared.ArchivedAt == nil || shared.ArchivedBy != "" || len(shared.Body.Segments) != 1 {
		t.Fatalf("another member opening it: %+v %v", shared, err)
	}
	if access, err := service.AnchoredSessionContentAccess(bob, repo, domain.ProviderClaude, "session-1"); err != nil || len(access.Parts) != 1 {
		t.Fatalf("its conversation: %+v %v", access, err)
	}

	// A report uploaded again does not bring it back.
	talked.Machine = "laptop"
	if next, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", talked); err != nil || !next.Changed {
		t.Fatalf("uploading again: %+v %v", next, err)
	}
	if got := seen(bob); got.archived != 1 || got.segments != 1 {
		t.Fatalf("after uploading again: %+v", got)
	}

	// Whoever manages the repository restores it, and its uploader archives
	// and restores it again.
	if restored, err := service.SetAnchoredSessionArchived(manager, repo, domain.ProviderClaude, "session-1", false); err != nil || restored.Archived || restored.ArchivedAt != nil {
		t.Fatalf("a manager restoring: %+v %v", restored, err)
	}
	if got := seen(bob); got != (found{sessions: 2, segments: 2, summaries: 2, memory: 2, hits: 2}) {
		t.Fatalf("after restoring: %+v", got)
	}
	if _, err := service.SetAnchoredSessionArchived(manager, repo, domain.ProviderClaude, "session-1", true); err != nil {
		t.Fatal(err)
	}
	if own, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "session-1"); err != nil || own.ArchivedBy != "manager" {
		t.Fatalf("archived by a manager: %+v %v", own, err)
	}
	if restored, err := service.SetAnchoredSessionArchived(alice, repo, domain.ProviderClaude, "session-1", false); err != nil || restored.Archived {
		t.Fatalf("the uploader restoring what a manager archived: %+v %v", restored, err)
	}
	if got := seen(visitor); got.archived != 0 || got.segments != 2 {
		t.Fatalf("at the end: %+v", got)
	}
}
