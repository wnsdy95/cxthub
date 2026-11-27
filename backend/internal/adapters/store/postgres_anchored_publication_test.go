//go:build postgres

package store

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGAnchoredPublicationPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	// What is open belongs to a stored session.
	missing := domain.AnchoredPublication{Provider: domain.ProviderClaude, SessionID: "session-1", Ranges: []domain.AnchoredPublishedRange{
		{FirstOrdinal: 0, LastOrdinal: 1, At: remoteGitStoreTime(1), By: domain.AnchoredPublicationReason{Kind: domain.AnchoredPublishedByCommit, Commit: anchoredStoreSHA("a")}}}}
	if err := st.PutAnchoredPublication(ctx, repo, missing); err == nil {
		t.Fatal("a publication without its session was stored")
	}
	for _, session := range []domain.AnchoredSession{
		anchoredStoreSession(t, repo, domain.ProviderClaude, "session-1", "uploader-1", anchoredStoreSHA("a")),
		anchoredStoreSession(t, repo, domain.ProviderCodex, "session-2", "uploader-1", anchoredStoreSHA("b")),
	} {
		if err := st.PutAnchoredSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	checkAnchoredPublicationPersistence(t, ctx, st, repo)

	// The table keeps the promises for a writer that skips the application.
	for name, statement := range map[string]string{
		"a list that is not a list": `UPDATE anchored_session_publications SET body='{}' WHERE repo_id=$1`,
		"an empty list":             `UPDATE anchored_session_publications SET body='[]' WHERE repo_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, statement, repo); err == nil {
			t.Errorf("%s was accepted by the table", name)
		}
	}
	// Removing a session removes what was open of it; removing the
	// repository removes the rest.
	var remaining int
	if _, err := st.pool.Exec(ctx, `DELETE FROM anchored_sessions WHERE repo_id=$1 AND provider='codex'`, repo); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM anchored_session_publications WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("publications after one session was removed: %d %v", remaining, err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM repos WHERE id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM anchored_session_publications WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("publications outlived their repository: %d %v", remaining, err)
	}
}

func TestPGRemotePushPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkRemotePushPersistence(t, ctx, st, repo)

	a := remoteGitStoreSHA("a")
	if err := st.RecordRemotePushes(ctx, repo, []domain.RemoteBranchPush{{Branch: "main", SHA: a, SeenAt: remoteGitStoreTime(60)}}); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"a push without a branch":        `UPDATE remote_git_pushes SET branch='' WHERE repo_id=$1`,
		"a push without a commit number": `UPDATE remote_git_pushes SET sha='main' WHERE repo_id=$1`,
		"a push without its time":        `UPDATE remote_git_pushes SET seen_at=NULL WHERE repo_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, statement, repo); err == nil {
			t.Errorf("%s was accepted by the table", name)
		}
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM repos WHERE id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM remote_git_pushes WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("pushes outlived their repository: %d %v", remaining, err)
	}
}
