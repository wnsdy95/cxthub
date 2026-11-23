//go:build postgres

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// The search reads conversations through the document reads the server
// already has, each in a read of its own, after the repository transaction
// that decided what the reader may read. A conversation whose document is
// not prepared yet is left out and the answer says that more may be found.
// The pool of this fixture holds two connections.
func TestPGSearchReadsWhatTheReaderMayRead(t *testing.T) {
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
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "search-" + suffix, OwnerID: users["owner"], Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC()}
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
	provider := newRemoteGitTestProvider(100)
	pushed, local := provider.chain(1, ""), anchoredTestSHA("b")
	commit := provider.commits[pushed]
	commit.Subject = "Add the Parser"
	provider.commits[pushed] = commit
	provider.heads["main"], provider.heads["parser-work"] = pushed, pushed
	worker, err := NewRemoteGitSync(RemoteGitSyncConfig{Metadata: storage, Blobs: storage, Repositories: storage, Reader: provider})
	if err != nil {
		t.Fatal(err)
	}
	clock := remoteGitTestTime(100000)
	worker.now = func() time.Time { return clock }
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
	doc.CIR.Envelope.CIRVersion, doc.CIR.Envelope.SourceProvider = "1", "claude"
	for index, text := range []string{"Plan the parser.", "The parser is committed.", "A hidden parser idea.", "Parser leftovers."} {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: index, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := storage.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	alice, bob := anchoredActor(users["member-a"]), anchoredActor(users["member-b"])
	body := domain.AnchoredSessionBody{
		RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 4,
		Content: &domain.AnchoredContent{Doc: doc.Hash, Events: 4},
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m2", FirstOrdinal: 0, LastOrdinal: 1, FirstEvent: 0, Events: 2},
				Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber, Subject: "Add the Parser"}}},
			{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m3", FirstOrdinal: 2, LastOrdinal: 2, FirstEvent: 2, Events: 1},
				Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber, Subject: "Parser rework"}}},
		},
		Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m4", Last: "m4", FirstOrdinal: 3, LastOrdinal: 3, FirstEvent: 3, Events: 1}, Places: []domain.AnchoredPlace{}, Unplaced: true},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateAnchoredNote(alice, repo, domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: pushed}, "The parser was reviewed."); err != nil {
		t.Fatal(err)
	}

	if err != nil {
		t.Fatal(err)
	}
	var own domain.AnchoredSearchResult
	for round := 0; round < 5; round++ {
		if own, err = service.SearchAnchored(alice, repo, "Parser"); err != nil {
			t.Fatal(err)
		}
		if !own.Truncated {
			break
		}
	}
	want := []string{"branch:parser-work", "commit:Add the Parser", "commit:Parser rework", "note:The parser was reviewed.",
		"segment:A hidden parser idea.", "segment:Plan the parser.", "segment:The parser is committed."}
	// The fourth matching event is passed over: one session gives three hits.
	if got := anchoredSearchKinds(own); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("what the uploader finds: %v truncated=%v", got, own.Truncated)
	}
	shared, err := service.SearchAnchored(bob, repo, "parser")
	if err != nil {
		t.Fatal(err)
	}
	wantShared := []string{"branch:parser-work", "commit:Add the Parser", "note:The parser was reviewed.", "segment:Plan the parser.", "segment:The parser is committed."}
	if got := anchoredSearchKinds(shared); strings.Join(got, "|") != strings.Join(wantShared, "|") || shared.Truncated {
		t.Fatalf("what another member finds: %v truncated=%v", got, shared.Truncated)
	}
	for _, query := range []string{"hidden", "leftovers", "rework"} {
		if found, err := service.SearchAnchored(bob, repo, query); err != nil || len(found.Hits) != 0 {
			t.Fatalf("another member found %q, which is not pushed: %+v %v", query, found.Hits, err)
		}
	}
}
