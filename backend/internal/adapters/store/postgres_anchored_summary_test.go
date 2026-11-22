//go:build postgres

package store

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGAnchoredSummaryPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	// A summary belongs to a stored session.
	if err := st.PutAnchoredSummary(ctx, anchoredStoreSummary(t, repo, domain.ProviderClaude, "session-1", anchoredStoreSHA("a"), "Added the parser.", 10)); err == nil {
		t.Fatal("a summary without its session was stored")
	}
	for _, session := range []domain.AnchoredSession{
		anchoredStoreSession(t, repo, domain.ProviderClaude, "session-1", "uploader-1", anchoredStoreSHA("a"), anchoredStoreSHA("b")),
		anchoredStoreSession(t, repo, domain.ProviderCodex, "session-2", "uploader-1", anchoredStoreSHA("a")),
	} {
		if err := st.PutAnchoredSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	checkAnchoredSummaryPersistence(t, ctx, st, repo)

	// The table keeps the promises for a writer that skips the application.
	for name, statement := range map[string]string{
		"a body that is not an object": `UPDATE anchored_session_summaries SET body='[]' WHERE repo_id=$1`,
		"a short commit number":        `UPDATE anchored_session_summaries SET commit_sha='abc1234' WHERE repo_id=$1`,
		"a hash that is not one":       `UPDATE anchored_session_summaries SET body_hash='sha256:xyz' WHERE repo_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, statement, repo); err == nil {
			t.Errorf("%s was accepted by the table", name)
		}
	}
	// Removing a session removes its summaries.
	if _, err := st.pool.Exec(ctx, `DELETE FROM anchored_sessions WHERE repo_id=$1 AND provider='claude'`, repo); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM anchored_session_summaries WHERE repo_id=$1`, repo).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("summaries after their session was removed: %d %v", remaining, err)
	}
}
