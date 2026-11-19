//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func anchoredRestoreCapturePG(t *testing.T, st *store.PostgresStore, repo domain.ContentHash, session string, minute int, texts []string) domain.ContentHash {
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
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, Branch: "main", DocHash: doc.Hash, Provider: domain.ProviderClaude,
		Fidelity: domain.FidelityFull, SessionID: session, CreatedAt: time.Date(2031, 1, 2, 3, minute, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	return doc.Hash
}

// Restoration reads the captures' conversations through the document reads
// the server already has, and stores and opens in a repository transaction.
// The pool of this fixture holds two connections.
func TestPGSessionsAreRestoredFromCaptures(t *testing.T) {
	service, st, repo := remoteGitPG(t)
	ctx := systemTestContext()
	suffix := domain.NewID("")[:10]
	users := map[string]string{}
	for _, name := range []string{"owner", "maintainer", "member"} {
		user := domain.User{ID: domain.NewID("u_"), Username: name + "-" + suffix, Email: name + suffix + "@example.test", Name: "Test"}
		if err := st.UpsertUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		users[name] = user.ID
	}
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "restore-" + suffix, OwnerID: users["owner"], Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC()}
	if err := st.CreateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	for name, role := range map[string]domain.MemberRole{"maintainer": domain.RoleMaintainer, "member": domain.RoleMember} {
		if err := st.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: users[name], Role: role, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	current, err := st.GetRepo(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	current.RepositoryID = repository.ID
	if _, err := st.PutRepo(ctx, current); err != nil {
		t.Fatal(err)
	}
	texts := []string{"plan", "write", "commit it", "done", "more", "last"}
	first := anchoredRestoreCapturePG(t, st, repo, "session-old", 10, texts[:3])
	newest := anchoredRestoreCapturePG(t, st, repo, "session-old", 20, texts)
	sha := fmt.Sprintf("%040x", 0xabc)
	if err := st.ApplyHistoryEvent(ctx, domain.HistoryEvent{ID: fmt.Sprintf("%032x", 0xabc), RepoID: string(repo), BranchID: "main", Branch: "main", Kind: "publish",
		Source: first, Target: first, GitAfter: sha, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	maintainer := anchoredActor(users["maintainer"])
	if _, err := service.RestoreAnchoredSessions(anchoredActor(users["member"]), repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member restored: %v", err)
	}
	// A capture's conversation is read once the document worker prepared it:
	// until then the session is left for the next request.
	result, err := service.RestoreAnchoredSessions(maintainer, repo)
	if err != nil || result != (domain.AnchoredRestoreResult{Unreadable: 1, Remaining: true}) {
		t.Fatalf("restoration before the documents were prepared: %+v %v", result, err)
	}
	workers, err := service.DocumentWorkersForProfile(documentquery.LegacyCompatibility)
	if err != nil {
		t.Fatal(err)
	}
	// Each request reads as far as the documents are prepared and asks for
	// the next one; the worker prepares it in between.
	for round := 0; round < 5 && result.Remaining; round++ {
		if err := workers.Range.Process(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if result, err = service.RestoreAnchoredSessions(maintainer, repo); err != nil {
			t.Fatal(err)
		}
	}
	if result != (domain.AnchoredRestoreResult{Restored: 1}) {
		t.Fatalf("restoration: %+v", result)
	}
	stored, err := st.GetAnchoredSession(context.Background(), repo, domain.ProviderClaude, "session-old")
	if err != nil || stored.Uploader != domain.AnchoredRestoredUploader || stored.Body.Content.Doc != newest || len(stored.Body.Segments) != 1 ||
		stored.Body.Segments[0].LastOrdinal != 2 || stored.Body.Uncommitted == nil || stored.Body.Uncommitted.Events != 3 {
		t.Fatalf("restored report: %+v %v", stored.Body, err)
	}
	links, err := st.ListAnchoredCommitLinks(context.Background(), repo, sha)
	if err != nil || len(links) != 1 || links[0].Rule != domain.AnchoredLinkCapturedAtCommit {
		t.Fatalf("links: %+v %v", links, err)
	}
	if again, err := service.RestoreAnchoredSessions(maintainer, repo); err != nil || again != (domain.AnchoredRestoreResult{Unchanged: 1}) {
		t.Fatalf("restoring again: %+v %v", again, err)
	}
}
