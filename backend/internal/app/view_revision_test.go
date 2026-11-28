package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// An open page follows the repository's mark and reads its lists again when
// the mark is another one. So everything the Sessions tab shows moves it, in
// the transaction that stores it; and what the tab does not show leaves it,
// or every open page would read again for nothing.
func TestViewRevisionMovesWithEverythingTheSessionsTabShows(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	service, repo, provider := fixture.service, fixture.repo, fixture.provider
	alice, viewer := anchoredActor("member-a"), anchoredActor("viewer")
	mark := func() string {
		t.Helper()
		revision, err := service.ViewRevision(viewer, repo)
		if err != nil || revision == "" {
			t.Fatalf("the mark: %q %v", revision, err)
		}
		return revision
	}
	first := mark()
	if again := mark(); again != first {
		t.Fatalf("reading the mark moved it: %q then %q", first, again)
	}
	seen := map[string]string{first: "a repository nothing was stored in"}
	moves := func(what string, write func() error) {
		t.Helper()
		before := mark()
		if err := write(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		after := mark()
		if after == before {
			t.Fatalf("%s left the mark at %q", what, before)
		}
		// A mark never comes back: a page that missed a few changes still
		// sees that its own mark is not the current one.
		if earlier, repeated := seen[after]; repeated {
			t.Fatalf("%s gave the mark %q, which was the mark after %s", what, after, earlier)
		}
		seen[after] = what
	}
	stays := func(what string, act func() error) {
		t.Helper()
		before := mark()
		if err := act(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if after := mark(); after != before {
			t.Fatalf("%s moved the mark from %q to %q", what, before, after)
		}
	}

	// A read of the Git host moves the mark twice: when it ends, and when
	// sessions were opened after it. What it stores on its way, the branch
	// list and the commits, moves nothing: a page that read in between would
	// read half a pass.
	pushed := provider.chain(2, "")
	provider.heads["main"] = pushed
	counters := func() domain.RepositoryRevision {
		t.Helper()
		revision, err := fixture.storage.RepositoryRevision(systemTestContext(), repo)
		if err != nil {
			t.Fatal(err)
		}
		return revision
	}
	before := counters()
	moves("the end of a read of the Git host", func() error { _, err := fixture.pass(t); return err })
	if after := counters(); after.Graph != before.Graph+2 || after.Evidence != before.Evidence || after.Pending != before.Pending {
		t.Fatalf("a read of the Git host moved the counters from %+v to %+v", before, after)
	}
	// A pass that is not due stores nothing.
	stays("a read that was not due", func() error {
		if _, err := fixture.pass(t); !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		return nil
	})

	// A session: its report, its summary, its archive, its removal.
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 2,
		Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1},
			Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}}},
	}
	moves("a session's report", func() error {
		_, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body)
		return err
	})
	moves("a session's summary", func() error {
		_, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", pushed, domain.AnchoredSummaryBody{Summary: "Added the parser."})
		return err
	})
	moves("archiving a session", func() error {
		_, err := service.SetAnchoredSessionArchived(alice, repo, domain.ProviderClaude, "session-1", true)
		return err
	})
	moves("restoring a session", func() error {
		_, err := service.SetAnchoredSessionArchived(alice, repo, domain.ProviderClaude, "session-1", false)
		return err
	})
	moves("removing a session", func() error {
		_, err := service.RemoveAnchoredSession(alice, repo, domain.ProviderClaude, "session-1")
		return err
	})
	moves("allowing a removed session again", func() error {
		return service.AllowAnchoredSession(alice, repo, domain.ProviderClaude, "session-1")
	})

	// Notes, worktree lists and branch registrations.
	var note domain.AnchoredNote
	moves("a note", func() (err error) {
		note, err = service.CreateAnchoredNote(alice, repo, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: pushed}, "why this commit")
		return err
	})
	moves("a note rewritten", func() error {
		_, err := service.UpdateAnchoredNote(alice, repo, note.ID, "why this commit, in more words")
		return err
	})
	moves("a note removed", func() error { return service.DeleteAnchoredNote(alice, repo, note.ID) })
	moves("a worktree list", func() error {
		_, err := service.ReportWorktrees(alice, repo, domain.WorktreeReport{Machine: "laptop", Worktrees: []domain.WorktreeReportEntry{{Name: "app", Branch: "main"}}})
		return err
	})
	moves("a branch registration", func() error {
		_, err := service.RegisterBranchIDs(alice, repo, []domain.BranchRegistration{{Name: "main", LocalID: strings.Repeat("a", 32)}})
		return err
	})

	// A push the Git host delivered: the branch ledger learns of it at once.
	moves("a push the Git host delivered", func() error {
		_, err := service.ObserveRemotePush(systemTestContext(), remoteGitTestOrigin, "refs/heads/main", strings.Repeat("1", 40), "delivery-1")
		return err
	})
	// The read it asked for moves the mark when it has ended.
	fixture.clock = fixture.clock.Add(domain.RemoteGitRequestSpacing)
	moves("the read a delivery asked for", func() error { _, err := fixture.pass(t); return err })

	// What changes nothing a page shows leaves the mark. A pull request the
	// Git host delivered is not among them:
	// TestViewRevisionMovesWithADeliveredPullRequest follows it.
	stays("a request for a read", func() error { return service.RequestRemoteGitRefresh(alice, repo) })
	stays("reading the lists", func() error {
		if _, err := service.ListAnchoredSessions(viewer, repo); err != nil {
			return err
		}
		_, err := service.ReadRemoteGit(viewer, repo, domain.RemoteCommitCursor{}, 10)
		return err
	})
	stays("a write that is refused", func() error {
		if _, err := service.PutAnchoredSession(viewer, repo, domain.ProviderClaude, "session-2", body); !errors.Is(err, domain.ErrForbidden) {
			return errors.Join(errors.New("a viewer's report was not refused"), err)
		}
		return nil
	})

	// A repository this server does not hold has no mark.
	if _, err := service.ViewRevision(viewer, hh("a repository nobody registered")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the mark of a repository this server does not hold: %v", err)
	}
	// What is no repository identifier is refused as such, not looked for.
	if _, err := service.ViewRevision(viewer, "not a repository"); !errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the mark of what is no repository: %v", err)
	}
}

// A pull request the Git host delivered moves no branch, and what it asks for,
// a read of the bound repository, by itself changes nothing a page shows. The
// mark moves all the same, once, when the GitHub worker applies the delivery,
// and that is right: the worker writes every delivery it applies as evidence,
// a push with the observation of its branch and a pull request with only its
// request for a read, and a write of evidence moves the evidence counter in
// the transaction that stores it. An open page reads its lists once more for
// that, and again when the read has ended. Nothing moves when the delivery is
// only received, and a delivery that was applied is not applied again.
func TestViewRevisionMovesWithADeliveredPullRequest(t *testing.T) {
	connections, storage, repo := githubPRObservationFixture(t)
	ctx := context.Background()
	service := NewService(storage, storage, storage)
	mark := func() string {
		t.Helper()
		revision, err := service.ViewRevision(systemTestContext(), repo.ID)
		if err != nil || revision == "" {
			t.Fatalf("the mark: %q %v", revision, err)
		}
		return revision
	}
	counters := func() domain.RepositoryRevision {
		t.Helper()
		revision, err := storage.RepositoryRevision(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		return revision
	}
	before, first := counters(), mark()
	if err := connections.Receive(ctx, "view-revision-pull-request", "pull_request", githubPRObservationBody("opened", strings.Repeat("a", 40), strings.Repeat("b", 40))); err != nil {
		t.Fatal(err)
	}
	if received := mark(); received != first {
		t.Fatalf("a delivery that was only received moved the mark from %q to %q", first, received)
	}
	for range 2 {
		if err := githubWorkerOf(connections).deliver(ctx, githubHash("view-revision-pull-request")); err != nil {
			t.Fatal(err)
		}
	}
	if after := counters(); after.Evidence != before.Evidence+1 || after.Graph != before.Graph || after.Pending != before.Pending {
		t.Fatalf("a delivered pull request moved the counters from %+v to %+v", before, after)
	}
	if applied := mark(); applied == first {
		t.Fatalf("a delivered pull request left the mark at %q", first)
	}
}

// A look at an old read of the Git host asks for a new one and by itself
// changes nothing a page shows: the mark moves when the read has ended.
func TestViewRevisionWaitsForTheReadALookAskedFor(t *testing.T) {
	fixture := newRemoteGitTestFixture(t, 100)
	fixture.provider.heads["main"] = fixture.provider.chain(2, "")
	fixture.clock = time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	viewer := anchoredActor("viewer")
	mark := func() string {
		t.Helper()
		revision, err := fixture.service.ViewRevision(viewer, fixture.repo)
		if err != nil {
			t.Fatal(err)
		}
		return revision
	}
	before := mark()
	if _, err := fixture.service.ReadRemoteGit(viewer, fixture.repo, domain.RemoteCommitCursor{}, 10); err != nil {
		t.Fatal(err)
	}
	if state := fixture.state(t); state.RequestedAt == nil {
		t.Fatalf("the look did not ask: %+v", state)
	}
	if after := mark(); after != before {
		t.Fatalf("a look moved the mark from %q to %q", before, after)
	}
	fixture.clock = time.Now().UTC().Add(time.Second)
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	if after := mark(); after == before {
		t.Fatalf("the read the look asked for left the mark at %q", before)
	}
}
