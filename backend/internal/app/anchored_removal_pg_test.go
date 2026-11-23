//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Removing a session on PostgreSQL: the report, its summaries and its
// conversation document go in one repository transaction, the removal is
// remembered, and a session that grew leaves no earlier document behind.
// The pool of this fixture holds two connections.
func TestPGRemovedSessionIsGoneWithItsDocument(t *testing.T) {
	service, storage, repo := remoteGitPG(t)
	ctx := systemTestContext()
	suffix := domain.NewID("")[:10]
	users := map[string]string{}
	for _, name := range []string{"owner", "member-a", "member-b"} {
		user := domain.User{ID: domain.NewID("u_"), Username: name + "-" + suffix, Email: name + suffix + "@example.test", Name: "Test"}
		if err := storage.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		users[name] = user.ID
	}
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "removal-" + suffix, OwnerID: users["owner"], Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC()}
	if err := storage.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"member-a", "member-b"} {
		if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: users[name], Role: domain.RoleMember, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	current, err := storage.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	current.RepositoryID = repository.ID
	if _, err := storage.PutRepo(ctx, current); err != nil {
		t.Fatal(err)
	}
	document := func(texts ...string) domain.ContentHash {
		t.Helper()
		doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
		doc.CIR.Envelope.CIRVersion, doc.CIR.Envelope.SourceProvider = "1", "claude"
		for index, text := range texts {
			doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: index, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text + " " + suffix}}})
		}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err := storage.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		return doc.Hash
	}
	stored := func(doc domain.ContentHash) bool {
		t.Helper()
		have, err := storage.HasDocs(ctx, repo, []domain.ContentHash{doc})
		if err != nil {
			t.Fatal(err)
		}
		return len(have) == 1
	}
	commit := anchoredTestSHA("a")
	report := func(doc domain.ContentHash, events int) domain.AnchoredSessionBody {
		return domain.AnchoredSessionBody{
			RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: events,
			Content: &domain.AnchoredContent{Doc: doc, Events: events},
			Segments: []domain.AnchoredSegment{{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m9", FirstOrdinal: 0, LastOrdinal: events - 1, FirstEvent: 0, Events: events},
				Commits: []domain.AnchoredCommit{{SHA: commit, Rule: domain.AnchoredLinkPrintedNumber}}}},
		}
	}
	alice, bob := anchoredActor(users["member-a"]), anchoredActor(users["member-b"])

	// A session that grew names a new document; the earlier one is deleted.
	first, second := document("Plan the parser.", "Planned."), document("Plan the parser.", "Planned.", "Write it.")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", report(first, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", report(second, 3)); err != nil {
		t.Fatal(err)
	}
	if stored(first) || !stored(second) {
		t.Fatalf("after the session grew: earlier stored=%v current stored=%v", stored(first), stored(second))
	}
	if _, err := service.PutAnchoredSummary(alice, repo, domain.ProviderClaude, "session-1", commit, domain.AnchoredSummaryBody{Summary: "Planned and wrote the parser."}); err != nil {
		t.Fatal(err)
	}

	// Another member is refused; the uploader removes.
	if _, err := service.RemoveAnchoredSession(bob, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another member removing: %v", err)
	}
	receipt, err := service.RemoveAnchoredSession(alice, repo, domain.ProviderClaude, "session-1")
	if err != nil || len(receipt.Removed) != 1 || !receipt.Removed[0].ByYou {
		t.Fatalf("removal: %+v %v", receipt, err)
	}
	if _, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "session-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the removed session: %v", err)
	}
	if summaries, err := service.ListAnchoredSummaries(alice, repo, []string{commit}); err != nil || len(summaries) != 0 {
		t.Fatalf("summaries of the removed session: %+v %v", summaries, err)
	}
	if stored(second) {
		t.Fatal("the conversation document of the removed session is still stored")
	}
	if own, err := service.ListAnchoredSessionRemovals(alice, repo); err != nil || len(own) != 1 || own[0].SessionID != "session-1" {
		t.Fatalf("the uploader's removals: %+v %v", own, err)
	}

	// It is not taken again until it is allowed again.
	third := document("Plan the parser.", "Planned.", "Write it.", "Again.")
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", report(third, 4)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a report of the removed session: %v", err)
	}
	if err := service.AllowAnchoredSession(alice, repo, domain.ProviderClaude, "session-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", report(third, 4)); err != nil || got.Revision != 1 || !got.Changed {
		t.Fatalf("the report after it was allowed again: %+v %v", got, err)
	}
	if own, err := service.ListAnchoredSessionRemovals(context.WithoutCancel(alice), repo); err != nil || len(own) != 0 {
		t.Fatalf("removals after allowing: %+v %v", own, err)
	}
}
