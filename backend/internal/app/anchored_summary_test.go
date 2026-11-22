package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A summary is written by the session's uploader at a commit a segment of the
// session ends at, and read under the rule of that segment.
func TestSummaryIsReadWithTheSegmentItBelongsTo(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo := fixture.service, fixture.provider, fixture.repo
	alice, bob, visitor := anchoredActor("member-a"), anchoredActor("member-b"), context.Background()
	pushed, local, other := provider.chain(1, ""), anchoredTestSHA("b"), anchoredTestSHA("c")
	provider.heads["main"] = pushed
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.UpsertUser(systemTestContext(), domain.User{ID: "member-a", Username: "alice", Email: "alice@example.test", Name: "Alice"}); err != nil {
		t.Fatal(err)
	}
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 4,
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1}, Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m4", FirstOrdinal: 2, LastOrdinal: 3}, Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber}}},
		},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	first := domain.AnchoredSummaryBody{Summary: "Added the parser.", KeyFacts: []string{"It reads TOML."}}
	second := domain.AnchoredSummaryBody{Summary: "Reworked the parser.", OpenTasks: []string{"Document it."}}

	// Only the uploader writes, and only at a commit the session made.
	if _, err := service.PutAnchoredSummary(bob, repo, domain.ProviderClaude, "session-1", pushed, first); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another member wrote a summary: %v", err)
	}
	if _, err := service.PutAnchoredSummary(visitor, repo, domain.ProviderClaude, "session-1", pushed, first); err == nil {
		t.Fatal("a reader without an account wrote a summary")
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", other, first); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a summary at a commit the session did not make: %v", err)
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-0", pushed, first); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a summary of a session that is not stored: %v", err)
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", pushed, domain.AnchoredSummaryBody{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a summary that says nothing: %v", err)
	}
	receipt, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", pushed, first)
	if err != nil || !receipt.Changed || receipt.BodyHash == "" {
		t.Fatalf("stored: %+v %v", receipt, err)
	}
	if again, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", pushed, first); err != nil || again.Changed || again.BodyHash != receipt.BodyHash {
		t.Fatalf("the same text again: %+v %v", again, err)
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", local, second); err != nil {
		t.Fatal(err)
	}

	// The uploader reads both; anyone else the one whose commit is pushed.
	own, err := service.ListAnchoredSummaries(alice, repo, []string{pushed, local})
	if err != nil || len(own) != 2 {
		t.Fatalf("what the uploader reads: %+v %v", own, err)
	}
	for _, summary := range own {
		want := 1
		if summary.Commit == local {
			want = 2
		}
		if summary.SegmentNo != want || summary.Uploader != "member-a" || summary.Provider != domain.ProviderClaude || summary.SessionID != "session-1" {
			t.Fatalf("a summary does not say where it belongs: %+v", summary)
		}
	}
	for name, reader := range map[string]context.Context{"another member": bob, "a reader without an account": visitor} {
		shared, err := service.ListAnchoredSummaries(reader, repo, []string{pushed, local})
		if err != nil || len(shared) != 1 || shared[0].Commit != pushed || shared[0].Body.Summary != "Added the parser." || shared[0].SegmentNo != 1 || shared[0].UploaderName != "alice" {
			t.Fatalf("what %s reads: %+v %v", name, shared, err)
		}
	}

	// Once the local commit is pushed, its summary opens with its segment.
	provider.commits[local] = domain.RemoteCommit{SHA: local, Parents: []string{pushed}, AuthorName: "Member A", AuthorEmail: "member-a@example.com",
		AuthoredAt: remoteGitTestTime(70), CommittedAt: remoteGitTestTime(70), Subject: "Pushed work"}
	provider.heads["main"] = local
	fixture.nextPass(t)
	if shared, err := service.ListAnchoredSummaries(bob, repo, []string{local}); err != nil || len(shared) != 1 || shared[0].Body.Summary != "Reworked the parser." {
		t.Fatalf("after the push: %+v %v", shared, err)
	}

	if _, err := service.ListAnchoredSummaries(alice, repo, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a read that names no commit: %v", err)
	}
	if _, err := service.ListAnchoredSummaries(alice, repo, []string{"abc1234"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a read by a short commit number: %v", err)
	}
}
