//go:build postgres

package store

import (
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGCollectorPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	other := domain.HashContent([]byte(t.Name() + " other " + time.Now().String()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	checkCollectorPersistence(t, ctx, st, repo, other)

	// The tables keep the promises for a writer that skips the application.
	for name, statement := range map[string]string{
		"a collector without its account":    `UPDATE reported_collectors SET account=' ' WHERE repo_id=$1`,
		"a collector with an endless error":  `UPDATE reported_collectors SET run_error=repeat('e', 513) WHERE repo_id=$1`,
		"a claimed push without its branch":  `UPDATE claimed_pushes SET branch='' WHERE repo_id=$1`,
		"a claimed push with a short commit": `UPDATE claimed_pushes SET sha='abc123' WHERE repo_id=$1`,
		"a claimed push without its account": `UPDATE claimed_pushes SET account='' WHERE repo_id=$1`,
	} {
		if _, err := st.pool.Exec(ctx, statement, repo); err == nil {
			t.Errorf("%s was accepted by the table", name)
		}
	}
	// A repository that is removed takes its collectors and claims with it.
	if _, err := st.pool.Exec(ctx, `DELETE FROM repos WHERE id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := st.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reported_collectors WHERE repo_id=$1) + (SELECT count(*) FROM claimed_pushes WHERE repo_id=$1)`, repo).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows left of a removed repository (%v)", left, err)
	}
}

func TestPGRecentRemotePushes(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkRecentRemotePushes(t, ctx, st, repo)
}
