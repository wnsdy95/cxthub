package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const remoteGitStoreOrigin = "https://github.com/example/project"

func remoteGitStoreSHA(letter string) string { return strings.Repeat(letter, 40) }

func remoteGitStoreTime(seconds int) time.Time {
	return time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second)
}

func remoteGitStoreCommit(letter string, committed int, parents ...string) domain.RemoteCommit {
	return domain.RemoteCommit{SHA: remoteGitStoreSHA(letter), Parents: append([]string{}, parents...), AuthorName: "Author " + letter,
		AuthorEmail: letter + "@example.com", AuthoredAt: remoteGitStoreTime(committed - 5), CommittedAt: remoteGitStoreTime(committed), Subject: "commit " + letter}
}

func remoteGitStoreSHAs(commits []domain.RemoteCommit) string {
	letters := ""
	for _, commit := range commits {
		letters += commit.SHA[:1]
	}
	return letters
}

// checkRemoteGitPersistence is the contract both stores keep.
func checkRemoteGitPersistence(t *testing.T, ctx context.Context, storage outbound.RemoteGitStore, repo domain.ContentHash) {
	t.Helper()
	const lease = 2 * time.Minute
	a, b, c, d, e, f := remoteGitStoreSHA("a"), remoteGitStoreSHA("b"), remoteGitStoreSHA("c"), remoteGitStoreSHA("d"), remoteGitStoreSHA("e"), remoteGitStoreSHA("f")

	// A repository never read has no state and empty lists.
	if _, err := storage.GetRemoteGitState(ctx, repo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("state before any read: %v", err)
	}
	if err := storage.RequestRemoteGitSync(ctx, repo, remoteGitStoreTime(0)); err != nil {
		t.Fatalf("asking for a read of a repository never read: %v", err)
	}
	if _, err := storage.GetRemoteGitState(ctx, repo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("asking for a read made a state: %v", err)
	}
	if commits, err := storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, 10); err != nil || commits == nil || len(commits) != 0 {
		t.Fatalf("commits before any read: %#v %v", commits, err)
	}
	if branches, err := storage.ListRemoteBranches(ctx, repo); err != nil || branches == nil || len(branches) != 0 {
		t.Fatalf("branches before any read: %#v %v", branches, err)
	}
	if pulls, err := storage.ListRemotePulls(ctx, repo); err != nil || pulls == nil || len(pulls) != 0 {
		t.Fatalf("pull requests before any read: %#v %v", pulls, err)
	}
	if frontier, err := storage.RemoteCommitFrontier(ctx, repo, 10); err != nil || frontier == nil || len(frontier) != 0 {
		t.Fatalf("frontier before any read: %#v %v", frontier, err)
	}
	if missing, err := storage.MissingRemoteCommits(ctx, repo, []string{a, b}); err != nil || !reflect.DeepEqual(missing, []string{a, b}) {
		t.Fatalf("missing before any read: %v %v", missing, err)
	}

	// The first claim creates the state; the pass starts at the claim.
	state, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(0), lease)
	if err != nil || state.RepoID != repo || state.GitOrigin != remoteGitStoreOrigin || state.AttemptedAt == nil || !state.AttemptedAt.Equal(remoteGitStoreTime(0)) ||
		state.SyncedAt != nil || state.PullsComplete || state.PullPage != 1 {
		t.Fatalf("first claim: %+v %v", state, err)
	}
	if stored, err := storage.GetRemoteGitState(ctx, repo); err != nil || !reflect.DeepEqual(stored, state) {
		t.Fatalf("the claimed state:\n got %+v\nwant %+v\n%v", stored, state, err)
	}
	// While the claim is held nobody else gets one, whatever the origin.
	for _, origin := range []string{remoteGitStoreOrigin, remoteGitStoreOrigin + "-moved"} {
		if _, err := storage.ClaimRemoteGitSync(ctx, repo, origin, remoteGitStoreTime(119), lease); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("a second claim of %s while the first is held: %v", origin, err)
		}
	}
	// A request during the pass is kept, and does not make it another pass.
	if err := storage.RequestRemoteGitSync(ctx, repo, remoteGitStoreTime(5)); err != nil {
		t.Fatal(err)
	}
	during, err := storage.GetRemoteGitState(ctx, repo)
	if err != nil || during.RequestedAt == nil || !during.RequestedAt.Equal(remoteGitStoreTime(5)) || !during.SamePass(state) {
		t.Fatalf("a request during the pass: %+v %v", during, err)
	}
	// A claim nobody finished ends with its lease.
	expired, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(120), lease)
	if err != nil || !expired.AttemptedAt.Equal(remoteGitStoreTime(120)) || expired.RequestedAt == nil {
		t.Fatalf("a claim after the lease ended: %+v %v", expired, err)
	}

	// Finishing stores the state and releases the claim.
	finished := expired.Ended(remoteGitStoreTime(120), false, false)
	finished.DefaultBranch, finished.PullsComplete, finished.PullPage = "main", true, 0
	if err := storage.FinishRemoteGitSync(ctx, finished); err != nil {
		t.Fatal(err)
	}
	if stored, err := storage.GetRemoteGitState(ctx, repo); err != nil || !reflect.DeepEqual(stored, finished) {
		t.Fatalf("the finished state:\n got %+v\nwant %+v\n%v", stored, finished, err)
	}
	if _, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(419), lease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a claim before the next pass is due: %v", err)
	}
	// Asking for a read pulls the next pass closer.
	if err := storage.RequestRemoteGitSync(ctx, repo, remoteGitStoreTime(200)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(199), lease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a claim before the requested pass: %v", err)
	}
	requested, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(200), lease)
	if err != nil || !requested.RequestedAt.Equal(remoteGitStoreTime(200)) || !requested.AttemptedAt.Equal(remoteGitStoreTime(200)) ||
		requested.DefaultBranch != "main" || !requested.PullsComplete || !requested.SyncedAt.Equal(remoteGitStoreTime(120)) {
		t.Fatalf("the requested pass: %+v %v", requested, err)
	}

	// Commits are stored once and never changed.
	first, second, third, fourth := remoteGitStoreCommit("a", 10), remoteGitStoreCommit("b", 20, a), remoteGitStoreCommit("c", 20, b), remoteGitStoreCommit("d", 30, c, e)
	if added, err := storage.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{first, second}); err != nil || added != 2 {
		t.Fatalf("first page: %d %v", added, err)
	}
	if added, err := storage.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{second, third, fourth, fourth}); err != nil || added != 2 {
		t.Fatalf("a page that overlaps the first: %d %v", added, err)
	}
	rewritten := second
	rewritten.Subject, rewritten.Parents = "rewritten", []string{}
	if added, err := storage.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{first, rewritten}); err != nil || added != 0 {
		t.Fatalf("a page of stored commits: %d %v", added, err)
	}
	if added, err := storage.PutRemoteCommits(ctx, repo, nil); err != nil || added != 0 {
		t.Fatalf("an empty page: %d %v", added, err)
	}
	// Asked-for commits come back in the order asked, each once.
	got, err := storage.GetRemoteCommits(ctx, repo, []string{d, e, b, d})
	if err != nil || !reflect.DeepEqual(got, []domain.RemoteCommit{fourth, second}) {
		t.Fatalf("stored commits:\n got %+v\nwant %+v\n%v", got, []domain.RemoteCommit{fourth, second}, err)
	}
	if root, err := storage.GetRemoteCommits(ctx, repo, []string{a}); err != nil || len(root) != 1 || root[0].Parents == nil || len(root[0].Parents) != 0 {
		t.Fatalf("a commit without parents: %#v %v", root, err)
	}
	if missing, err := storage.MissingRemoteCommits(ctx, repo, []string{f, a, e, f, d}); err != nil || !reflect.DeepEqual(missing, []string{f, e}) {
		t.Fatalf("missing: %v %v", missing, err)
	}
	// What is not a commit is never stored, and neither is the rest of its page.
	invalid := remoteGitStoreCommit("f", 40)
	invalid.AuthoredAt = time.Time{}
	if _, err := storage.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{remoteGitStoreCommit("9", 40), invalid}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a commit without an author time: %v", err)
	}
	if missing, err := storage.MissingRemoteCommits(ctx, repo, []string{remoteGitStoreSHA("9")}); err != nil || len(missing) != 1 {
		t.Fatalf("a refused page stored something: %v %v", missing, err)
	}
	for name, read := range map[string]func() error{
		"missing": func() error { _, err := storage.MissingRemoteCommits(ctx, repo, []string{"main"}); return err },
		"get":     func() error { _, err := storage.GetRemoteCommits(ctx, repo, []string{a[:12]}); return err },
		"list": func() error {
			_, err := storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, 0)
			return err
		},
		"frontier": func() error {
			_, err := storage.RemoteCommitFrontier(ctx, repo, 0)
			return err
		},
		"copies": func() error {
			_, err := storage.FindRemoteCommitCopies(ctx, repo, []domain.RemoteCommitKey{{AuthorEmail: "a@example.com"}})
			return err
		},
	} {
		if err := read(); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s with what is not a commit number, a limit or a key: %v", name, err)
		}
	}

	// The list is by commit time, newest first, then by commit number.
	all, err := storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, 10)
	if err != nil || remoteGitStoreSHAs(all) != "dcba" || !reflect.DeepEqual(all[0], fourth) {
		t.Fatalf("the commit list: %s %v", remoteGitStoreSHAs(all), err)
	}
	page, err := storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, 2)
	if err != nil || remoteGitStoreSHAs(page) != "dc" {
		t.Fatalf("first page: %s %v", remoteGitStoreSHAs(page), err)
	}
	// A page starts after the last commit of the one before it, also when
	// commits share a commit time.
	page, err = storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursorAt(page[1]), 2)
	if err != nil || remoteGitStoreSHAs(page) != "ba" {
		t.Fatalf("second page: %s %v", remoteGitStoreSHAs(page), err)
	}
	if page, err = storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursorAt(page[1]), 2); err != nil || page == nil || len(page) != 0 {
		t.Fatalf("after the last page: %#v %v", page, err)
	}

	// A search finds commits by a part of the subject, whatever the letter
	// case, and by the beginning of the commit number; newest first.
	for query, want := range map[string]string{"COMMIT": "dcba", "mit b": "b", strings.Repeat("c", 7): "c", "nothing like it": "", "": ""} {
		found, err := storage.SearchRemoteCommits(ctx, repo, query, 10)
		if err != nil || found == nil || remoteGitStoreSHAs(found) != want {
			t.Fatalf("search for %q: %s %v", query, remoteGitStoreSHAs(found), err)
		}
	}
	if found, err := storage.SearchRemoteCommits(ctx, repo, "commit", 2); err != nil || remoteGitStoreSHAs(found) != "dc" {
		t.Fatalf("a search cut at its limit: %s %v", remoteGitStoreSHAs(found), err)
	}
	if _, err := storage.SearchRemoteCommits(ctx, repo, "commit", 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a search without a limit: %v", err)
	}

	// The frontier is the parents that stored commits name and that are not
	// stored, by commit number.
	if frontier, err := storage.RemoteCommitFrontier(ctx, repo, 10); err != nil || !reflect.DeepEqual(frontier, []string{e}) {
		t.Fatalf("frontier: %v %v", frontier, err)
	}
	seventh := remoteGitStoreCommit("7", 50, f, e, remoteGitStoreSHA("8"))
	if added, err := storage.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{seventh}); err != nil || added != 1 {
		t.Fatalf("a commit naming more unknown parents: %d %v", added, err)
	}
	if frontier, err := storage.RemoteCommitFrontier(ctx, repo, 10); err != nil || !reflect.DeepEqual(frontier, []string{remoteGitStoreSHA("8"), e, f}) {
		t.Fatalf("frontier of two commits: %v %v", frontier, err)
	}
	if frontier, err := storage.RemoteCommitFrontier(ctx, repo, 2); err != nil || !reflect.DeepEqual(frontier, []string{remoteGitStoreSHA("8"), e}) {
		t.Fatalf("a bounded frontier: %v %v", frontier, err)
	}
	// A commit the state lists as unreadable is not on the frontier.
	skipping := requested.Ended(remoteGitStoreTime(200), false, true).WithUnreadable(e)
	if err := storage.FinishRemoteGitSync(ctx, skipping); err != nil {
		t.Fatal(err)
	}
	if frontier, err := storage.RemoteCommitFrontier(ctx, repo, 10); err != nil || !reflect.DeepEqual(frontier, []string{remoteGitStoreSHA("8"), f}) {
		t.Fatalf("frontier without the unreadable commit: %v %v", frontier, err)
	}
	if stored, err := storage.GetRemoteGitState(ctx, repo); err != nil || !reflect.DeepEqual(stored, skipping) || !stored.Remaining {
		t.Fatalf("the state with an unreadable commit:\n got %+v\nwant %+v\n%v", stored, skipping, err)
	}

	// A copy keeps the author time, the author email and the subject.
	copied := second
	copied.SHA, copied.Parents, copied.CommittedAt, copied.AuthorName = remoteGitStoreSHA("1"), []string{d}, remoteGitStoreTime(60), "Somebody else"
	otherAuthor := second
	otherAuthor.SHA, otherAuthor.AuthorEmail = remoteGitStoreSHA("2"), "other@example.com"
	otherSubject := second
	otherSubject.SHA, otherSubject.Subject = remoteGitStoreSHA("3"), "commit b again"
	otherTime := second
	otherTime.SHA, otherTime.AuthoredAt = remoteGitStoreSHA("4"), second.AuthoredAt.Add(time.Second)
	if added, err := storage.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{copied, otherAuthor, otherSubject, otherTime}); err != nil || added != 4 {
		t.Fatalf("copies and near copies: %d %v", added, err)
	}
	copies, err := storage.FindRemoteCommitCopies(ctx, repo, []domain.RemoteCommitKey{second.Key(), second.Key(), first.Key(),
		{AuthoredAt: remoteGitStoreTime(999), AuthorEmail: "nobody@example.com", Subject: "nothing"}})
	if err != nil || remoteGitStoreSHAs(copies) != "1ab" || !reflect.DeepEqual(copies[0], copied) {
		t.Fatalf("copies: %s %v", remoteGitStoreSHAs(copies), err)
	}
	if none, err := storage.FindRemoteCommitCopies(ctx, repo, nil); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("copies of nothing: %#v %v", none, err)
	}

	// The branch list is replaced as a whole.
	if err := storage.ReplaceRemoteBranches(ctx, repo, []domain.RemoteBranch{{Name: "topic", SHA: c}, {Name: "main", SHA: d}, {Name: "Zeta", SHA: a}}); err != nil {
		t.Fatal(err)
	}
	if branches, err := storage.ListRemoteBranches(ctx, repo); err != nil || !reflect.DeepEqual(branches, []domain.RemoteBranch{{Name: "Zeta", SHA: a}, {Name: "main", SHA: d}, {Name: "topic", SHA: c}}) {
		t.Fatalf("branches by name: %+v %v", branches, err)
	}
	if err := storage.ReplaceRemoteBranches(ctx, repo, []domain.RemoteBranch{{Name: "main", SHA: a}, {Name: "main", SHA: b}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a name listed twice: %v", err)
	}
	if err := storage.ReplaceRemoteBranches(ctx, repo, []domain.RemoteBranch{{Name: "main", SHA: "main"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a branch without a commit number: %v", err)
	}
	// A moved branch, a new one and a deleted one.
	if err := storage.ReplaceRemoteBranches(ctx, repo, []domain.RemoteBranch{{Name: "main", SHA: c}, {Name: "feature/new", SHA: b}}); err != nil {
		t.Fatal(err)
	}
	if branches, err := storage.ListRemoteBranches(ctx, repo); err != nil || !reflect.DeepEqual(branches, []domain.RemoteBranch{{Name: "feature/new", SHA: b}, {Name: "main", SHA: c}}) {
		t.Fatalf("the replaced branch list: %+v %v", branches, err)
	}

	// A pull request is stored with its commit list, in the provider's order.
	one := domain.RemotePull{Number: 1, Title: "First", State: domain.RemotePullMerged, HeadRef: "topic", HeadSHA: c, BaseRef: "main", MergeSHA: d,
		Commits: []string{b, c}, UpdatedAt: remoteGitStoreTime(70)}
	two := domain.RemotePull{Number: 12, Title: "From a fork", State: domain.RemotePullOpen, HeadRef: "main", HeadSHA: e, BaseRef: "main", Fork: true,
		Commits: []string{}, UpdatedAt: remoteGitStoreTime(80)}
	for _, pull := range []domain.RemotePull{one, two} {
		if err := storage.PutRemotePull(ctx, repo, pull); err != nil {
			t.Fatal(err)
		}
	}
	if pulls, err := storage.ListRemotePulls(ctx, repo); err != nil || !reflect.DeepEqual(pulls, []domain.RemotePull{two, one}) {
		t.Fatalf("pull requests, highest number first:\n got %+v\nwant %+v\n%v", pulls, []domain.RemotePull{two, one}, err)
	}
	// Storing it again replaces it and its commit list.
	two.Commits, two.HeadSHA, two.UpdatedAt, two.State = []string{c, a, f}, f, remoteGitStoreTime(90), domain.RemotePullClosed
	if err := storage.PutRemotePull(ctx, repo, two); err != nil {
		t.Fatal(err)
	}
	if pulls, err := storage.ListRemotePulls(ctx, repo); err != nil || !reflect.DeepEqual(pulls, []domain.RemotePull{two, one}) {
		t.Fatalf("a replaced pull request:\n got %+v\nwant %+v\n%v", pulls, []domain.RemotePull{two, one}, err)
	}
	invalidPull := one
	invalidPull.Number, invalidPull.State = 3, "draft"
	if err := storage.PutRemotePull(ctx, repo, invalidPull); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a pull request in an unknown state: %v", err)
	}

	// Another repository has its own cache.
	elsewhere := domain.HashContent([]byte(string(repo) + "/elsewhere"))
	if commits, err := storage.GetRemoteCommits(ctx, elsewhere, []string{a}); err != nil || len(commits) != 0 {
		t.Fatalf("a commit of another repository: %+v %v", commits, err)
	}
	if _, err := storage.GetRemoteGitState(ctx, elsewhere); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the state of another repository: %v", err)
	}

	// Finishing a pass of a repository that has no state is refused.
	if err := storage.FinishRemoteGitSync(ctx, domain.NewRemoteGitState(elsewhere, remoteGitStoreOrigin, remoteGitStoreTime(0))); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("finishing without a state: %v", err)
	}
	for name, claim := range map[string]func() error{
		"no origin": func() error {
			_, err := storage.ClaimRemoteGitSync(ctx, repo, "", remoteGitStoreTime(1000), lease)
			return err
		},
		"no lease": func() error {
			_, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(1000), 0)
			return err
		},
	} {
		if err := claim(); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("a claim with %s: %v", name, err)
		}
	}

	// A cache belongs to one origin. When the origin differs, the claim
	// removes what was read and starts over, without waiting for the next pass.
	if stored, err := storage.GetRemoteGitState(ctx, repo); err != nil || !stored.NextAt.After(remoteGitStoreTime(250)) {
		t.Fatalf("the next pass of the first origin: %+v %v", stored, err)
	}
	moved, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin+"-moved", remoteGitStoreTime(250), lease)
	if err != nil || moved.GitOrigin != remoteGitStoreOrigin+"-moved" || moved.SyncedAt != nil || moved.RequestedAt != nil || moved.DefaultBranch != "" ||
		moved.PullsComplete || moved.PullPage != 1 || len(moved.Unreadable) != 0 || !moved.AttemptedAt.Equal(remoteGitStoreTime(250)) {
		t.Fatalf("the first claim of another origin: %+v %v", moved, err)
	}
	if stored, err := storage.GetRemoteGitState(ctx, repo); err != nil || !reflect.DeepEqual(stored, moved) {
		t.Fatalf("the state of the other origin:\n got %+v\nwant %+v\n%v", stored, moved, err)
	}
	if commits, err := storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, 10); err != nil || len(commits) != 0 {
		t.Fatalf("commits of the first origin were kept: %s %v", remoteGitStoreSHAs(commits), err)
	}
	if branches, err := storage.ListRemoteBranches(ctx, repo); err != nil || len(branches) != 0 {
		t.Fatalf("branches of the first origin were kept: %+v %v", branches, err)
	}
	if pulls, err := storage.ListRemotePulls(ctx, repo); err != nil || len(pulls) != 0 {
		t.Fatalf("pull requests of the first origin were kept: %+v %v", pulls, err)
	}
	// The new claim is held like any other.
	if _, err := storage.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(251), lease); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the first origin claimed while the other is being read: %v", err)
	}
}

func TestFSRemoteGitPersistence(t *testing.T) {
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	ctx := context.Background()
	checkRemoteGitPersistence(t, ctx, storage, repo)

	// A cache changed on disk into something the store would never write is
	// an integrity failure, not another cache.
	if added, err := storage.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{remoteGitStoreCommit("a", 10)}); err != nil || added != 1 {
		t.Fatalf("a commit of the second origin: %d %v", added, err)
	}
	path := storage.remoteGitPath(repo)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"sha":"`+remoteGitStoreSHA("a")+`"`, `"sha":"main"`, 1)
	if tampered == string(raw) {
		t.Fatal("the stored cache does not hold the commit to change")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, 10); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("a commit without a number on disk: %v", err)
	}
	if _, err := storage.GetRemoteGitState(ctx, repo); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("the state beside it: %v", err)
	}
}
