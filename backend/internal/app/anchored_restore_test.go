package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// anchoredRestoreCapture stores a capture of a session whose conversation is
// the first events of the texts, as the old capture path did.
func anchoredRestoreCapture(t *testing.T, storage *store.FSStore, repo domain.ContentHash, session string, minute int, texts []string) domain.ContentHash {
	t.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, SessionOriginID: session}}}
	for index, text := range texts {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: index, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	ctx := systemTestContext()
	if _, err := storage.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, Branch: "main", DocHash: doc.Hash, Provider: domain.ProviderClaude,
		Fidelity: domain.FidelityFull, SessionID: session, CreatedAt: time.Date(2031, 1, 2, 3, minute, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	return doc.Hash
}

// anchoredRestorePublish records that a capture published a commit, as the
// old capture path left it in the history.
func anchoredRestorePublish(t *testing.T, storage *store.FSStore, repo, capture domain.ContentHash, sha string) {
	t.Helper()
	event := domain.HistoryEvent{ID: fmt.Sprintf("%032x", int(capture[len(capture)-1])), RepoID: string(repo), BranchID: "main", Branch: "main", Kind: "publish",
		Source: capture, Target: capture, GitAfter: sha, CreatedAt: time.Now().UTC()}
	if err := storage.ApplyHistoryEvent(systemTestContext(), event); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsNoCLIReportedAreRestoredFromCaptures(t *testing.T) {
	service, storage, repo := anchoredSessionFixture(t)
	alice, bob := anchoredActor("member-a"), anchoredActor("member-b")
	texts := []string{"plan", "write", "commit it", "done", "more", "also", "next", "and", "last"}
	first := anchoredRestoreCapture(t, storage, repo, "session-old", 10, texts[:4])
	anchoredRestoreCapture(t, storage, repo, "session-old", 20, texts[:6])
	newest := anchoredRestoreCapture(t, storage, repo, "session-old", 30, texts)
	anchoredRestorePublish(t, storage, repo, first, anchoredTestSHA("a"))
	// A capture of another session that a CLI reported already.
	anchoredRestoreCapture(t, storage, repo, "session-1", 5, []string{"reported"})
	if _, err := service.PutAnchoredSession(bob, repo, domain.ProviderClaude, "session-1", anchoredTestBody("m3")); err != nil {
		t.Fatal(err)
	}

	if _, err := service.RestoreAnchoredSessions(alice, repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member who does not maintain the repository restored: %v", err)
	}
	current, err := storage.GetRepo(systemTestContext(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AddMember(systemTestContext(), domain.Membership{RepositoryID: current.RepositoryID, UserID: "maintainer", Role: domain.RoleMaintainer, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	maintainer := anchoredActor("maintainer")
	result, err := service.RestoreAnchoredSessions(maintainer, repo)
	if err != nil || result != (domain.AnchoredRestoreResult{Restored: 1, Reported: 1}) {
		t.Fatalf("restoration: %+v %v", result, err)
	}
	stored, err := storage.GetAnchoredSession(context.Background(), repo, domain.ProviderClaude, "session-old")
	if err != nil || stored.Uploader != domain.AnchoredRestoredUploader || stored.Body.Scope != domain.AnchoredScopeCaptures || stored.Body.Content.Doc != newest ||
		stored.Body.Records != 9 || len(stored.Body.Segments) != 1 || stored.Body.Segments[0].LastOrdinal != 3 ||
		stored.Body.Segments[0].Commits[0] != (domain.AnchoredCommit{SHA: anchoredTestSHA("a"), Rule: domain.AnchoredLinkCapturedAtCommit}) ||
		stored.Body.Uncommitted == nil || !stored.Body.Uncommitted.Unplaced || stored.Body.Uncommitted.Events != 5 {
		t.Fatalf("restored report: %+v %v", stored, err)
	}
	// The same captures again change nothing and are not read again.
	if again, err := service.RestoreAnchoredSessions(maintainer, repo); err != nil || again != (domain.AnchoredRestoreResult{Unchanged: 1, Reported: 1}) {
		t.Fatalf("restoring again: %+v %v", again, err)
	}
	// Nothing of it is pushed, and no account uploaded it: no member reads it.
	for _, member := range []context.Context{alice, bob} {
		if _, err := service.GetAnchoredSession(member, repo, domain.ProviderClaude, "session-old"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("a member read a restored session nothing of is pushed: %v", err)
		}
	}
	// The member who has the session's records uploads the exact report, which replaces the restored one.
	receipt, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-old", anchoredTestBody("m3"))
	if err != nil || !receipt.Changed || receipt.Revision != 2 {
		t.Fatalf("the exact report: %+v %v", receipt, err)
	}
	if own, err := service.GetAnchoredSession(alice, repo, domain.ProviderClaude, "session-old"); err != nil || own.Uploader != "member-a" || own.Body.Scope != domain.AnchoredScopeFolder {
		t.Fatalf("the exact report as stored: %+v %v", own, err)
	}
	if after, err := service.RestoreAnchoredSessions(maintainer, repo); err != nil || after != (domain.AnchoredRestoreResult{Reported: 2}) {
		t.Fatalf("restoring after the exact report: %+v %v", after, err)
	}
	// A client cannot claim to have rebuilt a report from captures.
	claimed, _ := domain.RestoreAnchoredSession([]domain.AnchoredCapture{{Snapshot: newest, Doc: newest, Identity: domain.DocumentIdentityLegacy, CreatedAt: time.Now(), Events: 2, Prefix: true}})
	if _, err := service.PutAnchoredSession(bob, repo, domain.ProviderClaude, "session-claimed", claimed); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a client's report in the captures' scope: %v", err)
	}
	if _, err := service.RestoreAnchoredSessions(anchoredActor("viewer"), repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer restored: %v", err)
	}
}

// A restored segment whose commit is on the remote opens to every member.
func TestRestoredSessionOpensAsItsCommitIsOnTheRemote(t *testing.T) {
	fixture := anchoredPublicationFixture(t)
	service, provider, repo, storage := fixture.service, fixture.provider, fixture.repo, fixture.storage.FSStore
	current, err := storage.GetRepo(systemTestContext(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.AddMember(systemTestContext(), domain.Membership{RepositoryID: current.RepositoryID, UserID: "maintainer", Role: domain.RoleMaintainer, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	pushed := provider.chain(1, "")
	provider.heads["main"] = pushed
	if _, err := fixture.pass(t); err != nil {
		t.Fatal(err)
	}
	first := anchoredRestoreCapture(t, storage, repo, "session-old", 10, []string{"plan", "commit it"})
	anchoredRestoreCapture(t, storage, repo, "session-old", 20, []string{"plan", "commit it", "later"})
	anchoredRestorePublish(t, storage, repo, first, pushed)
	if result, err := service.RestoreAnchoredSessions(anchoredActor("maintainer"), repo); err != nil || result.Restored != 1 {
		t.Fatalf("restoration: %+v %v", result, err)
	}
	bob := anchoredActor("member-b")
	view, err := service.GetAnchoredSession(bob, repo, domain.ProviderClaude, "session-old")
	if err != nil || view.Uploader != domain.AnchoredRestoredUploader || len(view.Body.Segments) != 1 || view.Body.Segments[0].Hidden || view.Body.Uncommitted != nil {
		t.Fatalf("the restored session as a member reads it: %+v %v", view.Body, err)
	}
	access, err := service.AnchoredSessionContentAccess(bob, repo, domain.ProviderClaude, "session-old")
	if err != nil || len(access.Open) != 1 || access.Open[0] != [2]int{0, 2} {
		t.Fatalf("its open conversation: %+v %v", access, err)
	}
	summaries, err := service.ListAnchoredSessions(anchoredActor("member-a"), repo)
	if err != nil || len(summaries) != 1 || !summaries[0].Shared || summaries[0].Uploader != domain.AnchoredRestoredUploader || summaries[0].Commits[0].OnRemote == nil || !*summaries[0].Commits[0].OnRemote {
		t.Fatalf("the restored session listed: %+v %v", summaries, err)
	}
}
