package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// anchoredRemovalHas reports whether the repository still holds a document.
func anchoredRemovalHas(t *testing.T, fixture *remoteGitTestFixture, doc domain.ContentHash) bool {
	t.Helper()
	have, err := fixture.storage.HasDocs(systemTestContext(), fixture.repo, []domain.ContentHash{doc})
	if err != nil {
		t.Fatal(err)
	}
	return len(have) == 1
}

// A removed session is gone for every reader, with its summaries and its
// conversation document, and with the sessions it spawned. It is not taken
// again until it is allowed again. Its uploader removes and allows it, and so
// does whoever manages the repository.
func TestRemovedSessionIsGoneAndNotTakenAgainUntilAllowed(t *testing.T) {
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
	body := func(doc domain.ContentHash) domain.AnchoredSessionBody {
		return domain.AnchoredSessionBody{
			RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 2,
			Content: &domain.AnchoredContent{Doc: doc, Events: 2},
			Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}}},
		}
	}
	spawned := func(parent string, doc domain.ContentHash) domain.AnchoredSessionBody {
		out := body(doc)
		out.Sub, out.Parent = true, parent
		out.Spawn = &domain.AnchoredSpawn{AtRecord: "m1", AtOrdinal: 0, Rule: domain.AnchoredSpawnToolCall}
		return out
	}
	parentDoc := anchoredSearchDoc(t, fixture, "Plan the parser.", "The parser is committed.")
	childDoc := anchoredSearchDoc(t, fixture, "Review the parser.", "The parser is reviewed.")
	grandDoc := anchoredSearchDoc(t, fixture, "Check the parser tests.", "The parser tests pass.")
	otherDoc := anchoredSearchDoc(t, fixture, "Plan the lexer.", "The lexer is committed.")
	put := func(actor context.Context, id string, report domain.AnchoredSessionBody) {
		t.Helper()
		if _, err := service.PutAnchoredSession(actor, repo, domain.ProviderClaude, id, report); err != nil {
			t.Fatalf("report of %s: %v", id, err)
		}
	}
	put(alice, "parent", body(parentDoc))
	put(alice, "child", spawned("parent", childDoc))
	put(alice, "grandchild", spawned("child", grandDoc))
	put(alice, "other", body(otherDoc))
	// Another member's session names the parent as its parent; it is not the
	// uploader's and stays.
	put(bob, "stranger", spawned("parent", otherDoc))
	for _, id := range []string{"parent", "other"} {
		if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, id, pushed, domain.AnchoredSummaryBody{Summary: "Summary of " + id + "."}); err != nil {
			t.Fatal(err)
		}
	}
	fixture.nextPass(t)

	listed := func(reader context.Context) map[string]bool {
		t.Helper()
		sessions, err := service.ListAnchoredSessions(reader, repo)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, session := range sessions {
			out[session.SessionID] = true
		}
		return out
	}
	if got := listed(alice); len(got) != 5 {
		t.Fatalf("before: %v", got)
	}

	// Who may remove. A member who is neither the uploader nor a manager is
	// refused the same way for a session that exists and for one that does not.
	for _, id := range []string{"parent", "missing"} {
		if _, err := service.RemoveAnchoredSession(bob, repo, domain.ProviderClaude, id); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("another member removing %s: %v", id, err)
		}
	}
	if _, err := service.RemoveAnchoredSession(visitor, repo, domain.ProviderClaude, "parent"); err == nil {
		t.Fatal("a reader without an account removed a session")
	}
	if _, err := service.RemoveAnchoredSession(manager, repo, domain.ProviderClaude, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a manager removing a session that does not exist: %v", err)
	}
	if got := listed(alice); len(got) != 5 {
		t.Fatalf("a refused removal removed something: %v", got)
	}

	// The uploader removes the session; the sessions it spawned go with it,
	// each after its parent, and nothing of another account.
	receipt, err := service.RemoveAnchoredSession(alice, repo, domain.ProviderClaude, "parent")
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, each := range receipt.Removed {
		if !each.ByYou || each.RemovedAt.IsZero() || each.Provider != domain.ProviderClaude {
			t.Fatalf("removal: %+v", each)
		}
		removed = append(removed, each.SessionID)
	}
	if len(removed) != 3 || removed[0] != "parent" || removed[1] != "child" || removed[2] != "grandchild" {
		t.Fatalf("removed: %v", removed)
	}
	for name, reader := range map[string]context.Context{"the uploader": alice, "another member": bob, "a reader without an account": visitor} {
		got := listed(reader)
		if got["parent"] || got["child"] || got["grandchild"] || !got["other"] || !got["stranger"] {
			t.Fatalf("what %s lists after the removal: %v", name, got)
		}
		if _, err := service.GetAnchoredSession(reader, repo, domain.ProviderClaude, "parent"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s opening the removed session: %v", name, err)
		}
		summaries, err := service.ListAnchoredSummaries(reader, repo, []string{pushed})
		if err != nil || len(summaries) != 1 || summaries[0].SessionID != "other" {
			t.Fatalf("summaries %s reads after the removal: %+v %v", name, summaries, err)
		}
		segments, err := service.AnchoredCommitSegments(reader, repo, pushed)
		if err != nil {
			t.Fatal(err)
		}
		for _, segment := range segments {
			if segment.SessionID == "parent" || segment.SessionID == "child" || segment.SessionID == "grandchild" {
				t.Fatalf("%s still reads a segment of a removed session: %+v", name, segment)
			}
		}
	}
	// The conversation documents of the removed sessions are deleted; one
	// that another report names stays.
	for name, doc := range map[string]domain.ContentHash{"parent": parentDoc, "child": childDoc, "grandchild": grandDoc} {
		if anchoredRemovalHas(t, fixture, doc) {
			t.Fatalf("the document of %s is still stored", name)
		}
	}
	if !anchoredRemovalHas(t, fixture, otherDoc) {
		t.Fatal("a document another report names was deleted")
	}

	// The uploader lists its own removals; another member has none.
	own, err := service.ListAnchoredSessionRemovals(alice, repo)
	if err != nil || len(own) != 3 || own[0].SessionID != "child" || !own[0].ByYou {
		t.Fatalf("the uploader's removals: %+v %v", own, err)
	}
	if others, err := service.ListAnchoredSessionRemovals(bob, repo); err != nil || others == nil || len(others) != 0 {
		t.Fatalf("another member's removals: %#v %v", others, err)
	}
	if _, err := service.ListAnchoredSessionRemovals(visitor, repo); err == nil {
		t.Fatal("a reader without an account listed removals")
	}

	// Removing again changes nothing and answers the removal that stands.
	again, err := service.RemoveAnchoredSession(alice, repo, domain.ProviderClaude, "parent")
	if err != nil || len(again.Removed) != 1 || !again.Removed[0].RemovedAt.Equal(receipt.Removed[0].RemovedAt) {
		t.Fatalf("removing twice: %+v %v", again, err)
	}

	// A removed session is not taken again, from its uploader or anyone else,
	// and no summary is taken for it.
	parentAgain := anchoredSearchDoc(t, fixture, "Plan the parser.", "The parser is committed.")
	for name, actor := range map[string]context.Context{"its uploader": alice, "another member": bob} {
		if _, err := service.PutAnchoredSession(actor, repo, domain.ProviderClaude, "parent", body(parentAgain)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("a report of the removed session from %s: %v", name, err)
		}
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "parent", pushed, domain.AnchoredSummaryBody{Summary: "Back again."}); err == nil {
		t.Fatal("a summary of the removed session was stored")
	}

	// Who may allow it again: the account it belonged to, or a manager.
	for _, id := range []string{"parent", "missing"} {
		if err := service.AllowAnchoredSession(bob, repo, domain.ProviderClaude, id); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("another member allowing %s: %v", id, err)
		}
	}
	if err := service.AllowAnchoredSession(manager, repo, domain.ProviderClaude, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a manager allowing a session that was not removed: %v", err)
	}
	if err := service.AllowAnchoredSession(alice, repo, domain.ProviderClaude, "parent"); err != nil {
		t.Fatal(err)
	}
	// Nothing comes back by itself; the next report stores the session again.
	if got := listed(alice); got["parent"] {
		t.Fatalf("allowing brought the session back: %v", got)
	}
	put(alice, "parent", body(parentAgain))
	if got := listed(alice); !got["parent"] || got["child"] {
		t.Fatalf("after the report was sent again: %v", got)
	}
	if err := service.AllowAnchoredSession(alice, repo, domain.ProviderClaude, "parent"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("allowing what is not removed: %v", err)
	}

	// Whoever manages the repository removes another member's session, and
	// the member reads that it was not removed by them.
	if _, err := service.RemoveAnchoredSession(manager, repo, domain.ProviderClaude, "other"); err != nil {
		t.Fatal(err)
	}
	own, err = service.ListAnchoredSessionRemovals(alice, repo)
	if err != nil {
		t.Fatal(err)
	}
	byManager := false
	for _, each := range own {
		if each.SessionID == "other" {
			byManager = !each.ByYou
		}
	}
	if !byManager {
		t.Fatalf("a removal by a manager: %+v", own)
	}
	// The document is still named by another member's report and stays.
	if !anchoredRemovalHas(t, fixture, otherDoc) {
		t.Fatal("a document another member's report names was deleted with a session")
	}
	if err := service.AllowAnchoredSession(manager, repo, domain.ProviderClaude, "other"); err != nil {
		t.Fatalf("a manager allowing a session again: %v", err)
	}
}

// A report that names another conversation document than the one before
// leaves no earlier document behind.
func TestReportThatGrewLeavesNoEarlierConversationDocument(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, repo := fixture.service, fixture.repo
	alice := anchoredActor("member-a")
	report := func(doc domain.ContentHash, events int) domain.AnchoredSessionBody {
		return domain.AnchoredSessionBody{
			RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: events,
			Content: &domain.AnchoredContent{Doc: doc, Events: events},
			Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m9", FirstOrdinal: 0, LastOrdinal: events - 1, FirstEvent: 0, Events: events},
				Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "main", Rule: domain.AnchoredPlaceRecorded}}},
		}
	}
	first := anchoredSearchDoc(t, fixture, "Plan the parser.", "The parser is planned.")
	second := anchoredSearchDoc(t, fixture, "Plan the parser.", "The parser is planned.", "Now write it.")
	shared := anchoredSearchDoc(t, fixture, "A shared conversation.", "Shared.")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", report(first, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", report(second, 3)); err != nil {
		t.Fatal(err)
	}
	if anchoredRemovalHas(t, fixture, first) || !anchoredRemovalHas(t, fixture, second) {
		t.Fatalf("after the session grew: earlier stored=%v current stored=%v", anchoredRemovalHas(t, fixture, first), anchoredRemovalHas(t, fixture, second))
	}
	// A document two sessions name stays while one of them names it.
	for _, id := range []string{"session-2", "session-3"} {
		if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, id, report(shared, 2)); err != nil {
			t.Fatal(err)
		}
	}
	third := anchoredSearchDoc(t, fixture, "A shared conversation.", "Shared.", "No longer shared.")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-2", report(third, 3)); err != nil {
		t.Fatal(err)
	}
	if !anchoredRemovalHas(t, fixture, shared) {
		t.Fatal("a document another session still names was deleted")
	}
	// The same report again deletes nothing.
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", report(second, 3)); err != nil || !anchoredRemovalHas(t, fixture, second) {
		t.Fatalf("the same report again: %v", err)
	}
}
