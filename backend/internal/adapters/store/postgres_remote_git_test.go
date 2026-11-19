//go:build postgres

package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGRemoteGitPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkRemoteGitPersistence(t, ctx, st, repo)

	// The tables keep the promises for a writer that skips the application.
	a, b := remoteGitStoreSHA("a"), remoteGitStoreSHA("b")
	if _, err := st.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{remoteGitStoreCommit("a", 10)}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRemoteBranches(ctx, repo, []domain.RemoteBranch{{Name: "main", SHA: a}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRemotePull(ctx, repo, domain.RemotePull{Number: 1, State: domain.RemotePullOpen, HeadRef: "topic", HeadSHA: a, BaseRef: "main",
		Commits: []string{a}, UpdatedAt: remoteGitStoreTime(10)}); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"a commit without a full number":   `UPDATE remote_git_commits SET sha='main' WHERE repo_id=$1`,
		"a parent without a full number":   `UPDATE remote_git_commits SET parents=ARRAY['main'] WHERE repo_id=$1`,
		"a parent that is nothing":         `UPDATE remote_git_commits SET parents=ARRAY['` + b + `',NULL] WHERE repo_id=$1`,
		"a subject over the limit":         `UPDATE remote_git_commits SET subject=repeat('s',1025) WHERE repo_id=$1`,
		"a commit without an author time":  `UPDATE remote_git_commits SET authored_at=NULL WHERE repo_id=$1`,
		"a branch without a name":          `UPDATE remote_git_branches SET name='' WHERE repo_id=$1`,
		"a branch without a commit number": `UPDATE remote_git_branches SET sha='' WHERE repo_id=$1`,
		"a pull request numbered zero":     `INSERT INTO remote_git_pulls(repo_id,number,body) VALUES($1,0,'{}')`,
		"a commit list without its pull":   `INSERT INTO remote_git_pull_commits(repo_id,number,position,sha) VALUES($1,2,0,'` + a + `')`,
		"a listed commit without a number": `UPDATE remote_git_pull_commits SET sha='first' WHERE repo_id=$1`,
		"a place beyond the list":          `UPDATE remote_git_pull_commits SET position=250 WHERE repo_id=$1`,
		"a second commit at one place":     `INSERT INTO remote_git_pull_commits(repo_id,number,position,sha) VALUES($1,1,0,'` + b + `')`,
		"a state without a next pass":      `UPDATE remote_git_state SET next_at=NULL WHERE repo_id=$1`,
		"a cache without its repository":   `INSERT INTO remote_git_branches(repo_id,name,sha) VALUES('sha256:` + remoteGitStoreSHA("0") + remoteGitStoreSHA("0")[:24] + `','main','` + a + `')`,
	} {
		arguments := []any{repo}
		if name == "a cache without its repository" {
			arguments = nil
		}
		if _, err := st.pool.Exec(ctx, statement, arguments...); err == nil {
			t.Errorf("%s was accepted by the tables", name)
		}
	}
	// Removing a pull request removes its commit list.
	if _, err := st.pool.Exec(ctx, `DELETE FROM remote_git_pulls WHERE repo_id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM remote_git_pull_commits WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("a commit list outlived its pull request: %d %v", remaining, err)
	}
	// Removing the repository removes everything read for it.
	if err := st.PutRemotePull(ctx, repo, domain.RemotePull{Number: 2, State: domain.RemotePullOpen, HeadRef: "topic", HeadSHA: a, BaseRef: "main",
		Commits: []string{a}, UpdatedAt: remoteGitStoreTime(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM repos WHERE id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"remote_git_state", "remote_git_commits", "remote_git_branches", "remote_git_pulls", "remote_git_pull_commits"} {
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("%s outlived its repository: %d %v", table, remaining, err)
		}
	}
}

// Of the workers that ask for a pass at once, one gets it.
func TestPGRemoteGitClaimIsGivenOnce(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	const workers = 8
	claim := func(now time.Time, origin string) int {
		t.Helper()
		var wait sync.WaitGroup
		results := make(chan error, workers)
		for worker := 0; worker < workers; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, err := st.ClaimRemoteGitSync(ctx, repo, origin, now, time.Minute)
				results <- err
			}()
		}
		wait.Wait()
		close(results)
		won := 0
		for err := range results {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, domain.ErrNotFound):
				t.Fatalf("a claim failed instead of being refused: %v", err)
			}
		}
		return won
	}
	// The state does not exist yet, exists and is due, and belongs to another origin.
	if won := claim(remoteGitStoreTime(0), remoteGitStoreOrigin); won != 1 {
		t.Fatalf("%d workers claimed a repository never read", won)
	}
	if won := claim(remoteGitStoreTime(60), remoteGitStoreOrigin); won != 1 {
		t.Fatalf("%d workers claimed the pass after the lease ended", won)
	}
	if won := claim(remoteGitStoreTime(120), remoteGitStoreOrigin+"-moved"); won != 1 {
		t.Fatalf("%d workers started the other origin over", won)
	}
	if won := claim(remoteGitStoreTime(121), remoteGitStoreOrigin+"-moved"); won != 0 {
		t.Fatalf("%d workers claimed a pass that is held", won)
	}
}

// A claim inside a repository transaction that fails leaves nothing behind:
// neither the claim nor the removal of what another origin left.
func TestPGRemoteGitClaimFollowsItsTransaction(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	first, err := st.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin, remoteGitStoreTime(0), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRemoteCommits(ctx, repo, []domain.RemoteCommit{remoteGitStoreCommit("a", 10)}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRemoteGitSync(ctx, first.Ended(remoteGitStoreTime(0), false, false)); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort the transaction")
	if err := st.WithinRepository(ctx, repo, func(tx context.Context) error {
		if _, err := st.ClaimRemoteGitSync(tx, repo, remoteGitStoreOrigin+"-moved", remoteGitStoreTime(10), time.Minute); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("the transaction: %v", err)
	}
	state, err := st.GetRemoteGitState(ctx, repo)
	if err != nil || state.GitOrigin != remoteGitStoreOrigin || state.SyncedAt == nil {
		t.Fatalf("the state after the failed transaction: %+v %v", state, err)
	}
	if commits, err := st.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, 10); err != nil || len(commits) != 1 {
		t.Fatalf("commits after the failed transaction: %d %v", len(commits), err)
	}
	// The claim is free again, so the other origin can still start over.
	if _, err := st.ClaimRemoteGitSync(ctx, repo, remoteGitStoreOrigin+"-moved", remoteGitStoreTime(11), time.Minute); err != nil {
		t.Fatalf("the claim after the failed transaction: %v", err)
	}
}
