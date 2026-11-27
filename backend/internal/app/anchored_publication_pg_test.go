//go:build postgres

package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A report opens what is pushed inside the transaction that stores it, and a
// read of the provider in the transaction right after the one that finishes
// it. The pool of this fixture holds two connections, so a store call that
// left its transaction would show.
func TestPGSessionOpensInTheTransactionThatLearnsOfThePush(t *testing.T) {
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
	repository := domain.Repository{ID: domain.NewID("ws_"), Name: "test-" + suffix, OwnerID: users["owner"], Visibility: domain.VisibilityPrivate, CreatedAt: time.Now().UTC()}
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
	pushed := provider.chain(1, "")
	provider.heads["main"] = pushed
	worker, err := NewRemoteGitSync(RemoteGitSyncConfig{Metadata: storage, Blobs: storage, Repositories: storage, Reader: provider})
	if err != nil {
		t.Fatal(err)
	}
	clock := remoteGitTestTime(100000)
	worker.now = func() time.Time { return clock }
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}

	alice, bob := anchoredActor(users["member-a"]), anchoredActor(users["member-b"])
	local := anchoredTestSHA("e")
	body := domain.AnchoredSessionBody{RuleVersion: domain.AnchoredSessionRuleVersion, Scope: domain.AnchoredScopeFolder, Records: 3,
		Segments: []domain.AnchoredSegment{
			{AnchoredRange: domain.AnchoredRange{First: "m1", Last: "m1", FirstOrdinal: 0, LastOrdinal: 0}, Commits: []domain.AnchoredCommit{{SHA: pushed, Rule: domain.AnchoredLinkPrintedNumber}}},
			{AnchoredRange: domain.AnchoredRange{First: "m2", Last: "m2", FirstOrdinal: 1, LastOrdinal: 1}, Commits: []domain.AnchoredCommit{{SHA: local, Rule: domain.AnchoredLinkPrintedNumber}}},
		},
		Uncommitted: &domain.AnchoredUncommitted{AnchoredRange: domain.AnchoredRange{First: "m3", Last: "m3", FirstOrdinal: 2, LastOrdinal: 2,
			StartedAt: anchoredAt(clock.Add(time.Minute)), EndedAt: anchoredAt(clock.Add(time.Minute))},
			Places: []domain.AnchoredPlace{{Worktree: "app", Branch: "main", Rule: domain.AnchoredPlaceRecorded}}},
	}
	if _, err := service.PutAnchoredSession(alice, repo, domain.ProviderClaude, "session-1", body); err != nil {
		t.Fatal(err)
	}
	open := func() [][2]int {
		t.Helper()
		ranges, err := storage.GetAnchoredPublication(context.Background(), repo, domain.ProviderClaude, "session-1")
		if err != nil {
			t.Fatal(err)
		}
		return domain.PublishedOrdinals(ranges)
	}
	if got := open(); !reflect.DeepEqual(got, [][2]int{{0, 1}}) {
		t.Fatalf("open when reported: %v", got)
	}

	// The local commit is pushed on main: its segment opens, and the
	// uncommitted part, begun before the push was seen, with it.
	provider.commits[local] = domain.RemoteCommit{SHA: local, Parents: []string{pushed}, AuthorName: "Member A", AuthorEmail: "member-a@example.com",
		AuthoredAt: remoteGitTestTime(70), CommittedAt: remoteGitTestTime(70), Subject: "Pushed work"}
	provider.heads["main"] = local
	// The server is told of the push, as a delivery from the provider or the
	// pusher's cxt does: without being told it reads again only after
	// domain.RemoteGitSyncEvery.
	clock = clock.Add(5 * time.Minute)
	if err := storage.RequestRemoteGitSync(context.Background(), repo, clock); err != nil {
		t.Fatal(err)
	}
	if err := worker.SyncRemoteGit(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if got := open(); !reflect.DeepEqual(got, [][2]int{{0, 3}}) {
		t.Fatalf("open after the push: %v", got)
	}
	pushes, err := storage.ListRemotePushes(context.Background(), repo, []string{"main"})
	if err != nil || len(pushes) != 1 || pushes[0].SHA != local || !pushes[0].SeenAt.Equal(clock) {
		t.Fatalf("pushes recorded: %+v %v", pushes, err)
	}
	summaries, err := service.ListAnchoredSessions(bob, repo)
	if err != nil || len(summaries) != 1 || !summaries[0].Shared || summaries[0].Segments != 2 || summaries[0].Uncommitted == nil {
		t.Fatalf("what another member lists: %+v %v", summaries, err)
	}
	notes, err := service.ListAnchoredNotes(bob, repo)
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes: %+v %v", notes, err)
	}
}
