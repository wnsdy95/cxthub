//go:build postgres

package store

import (
	"strings"
	"testing"
)

func TestPGAnchoredNotePersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkAnchoredNotePersistence(t, ctx, st, repo)

	// The table keeps the promises for a writer that skips the application.
	id := "note_" + strings.Repeat("b", 32)
	for name, statement := range map[string]string{
		"an identifier in another form": `UPDATE anchored_notes SET id='first' WHERE repo_id=$1 AND id='` + id + `'`,
		"a target of no known kind":     `UPDATE anchored_notes SET target_kind='pull' WHERE repo_id=$1 AND id='` + id + `'`,
		"a note without an author":      `UPDATE anchored_notes SET author=' ' WHERE repo_id=$1 AND id='` + id + `'`,
	} {
		if _, err := st.pool.Exec(ctx, statement, repo); err == nil {
			t.Errorf("%s was accepted by the table", name)
		}
	}
}

func TestPGReportedWorktreePersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkReportedWorktreePersistence(t, ctx, st, repo)
	if _, err := st.pool.Exec(ctx, `UPDATE reported_worktrees SET account='' WHERE repo_id=$1`, repo); err == nil {
		t.Error("a list without its account was accepted by the table")
	}
}
