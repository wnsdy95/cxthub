//go:build postgres

package store

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGAnchoredSessionPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkAnchoredSessionPersistence(t, ctx, st, repo)
	checkRestoredAnchoredSessionPersistence(t, ctx, st, repo)
}

// Removing a report removes its index entries with it, and the checks the
// table itself makes hold even for a writer that skips the application.
func TestPGAnchoredSessionTableConstraints(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	session := anchoredStoreSession(t, repo, domain.ProviderClaude, "session-1", "uploader-1", anchoredStoreSHA("a"))
	if err := st.PutAnchoredSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"an unknown tool":            `UPDATE anchored_sessions SET provider='other' WHERE repo_id=$1`,
		"an identifier with a slash": `UPDATE anchored_sessions SET session_id='a/b' WHERE repo_id=$1`,
		"a blank uploader":           `UPDATE anchored_sessions SET uploader=' ' WHERE repo_id=$1`,
		"a short commit number":      `UPDATE anchored_session_commits SET commit_sha='abc1234' WHERE repo_id=$1`,
		"an unknown link rule":       `UPDATE anchored_session_commits SET rule='guessed' WHERE repo_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, statement, repo); err == nil {
			t.Errorf("%s was accepted by the table", name)
		}
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO anchored_session_commits(repo_id,provider,session_id,commit_sha,segment_no,rule) VALUES($1,'claude','no-such-session',$2,1,'printed-number')`, repo, anchoredStoreSHA("b")); err == nil {
		t.Error("an index entry without its report was accepted")
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM anchored_sessions WHERE repo_id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM anchored_session_commits WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("index entries outlived their report: %d %v", remaining, err)
	}
}
