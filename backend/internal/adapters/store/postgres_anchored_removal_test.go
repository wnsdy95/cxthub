//go:build postgres

package store

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGAnchoredRemovalPersistence(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	checkAnchoredRemovalPersistence(t, ctx, st, repo)
}

// The table keeps the promises for a writer that skips the application.
func TestPGAnchoredRemovalTableConstraints(t *testing.T) {
	st, ctx, repo := sessionArchivePG(t)
	insert := `INSERT INTO anchored_session_removals(repo_id,provider,session_id,uploader,removed_by,removed_at) VALUES($1,$2,$3,$4,$5,now())`
	if _, err := st.pool.Exec(ctx, insert, repo, "claude", "session-1", "uploader-1", "uploader-1"); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]any{
		"a second removal of one session":  {repo, "claude", "session-1", "uploader-1", "maintainer-1"},
		"another tool":                     {repo, "other", "session-2", "uploader-1", "uploader-1"},
		"an identifier that is a path":     {repo, "claude", "../escape", "uploader-1", "uploader-1"},
		"no uploader":                      {repo, "claude", "session-3", " ", "uploader-1"},
		"no remover":                       {repo, "claude", "session-4", "uploader-1", ""},
		"a repository that does not exist": {domain.HashContent([]byte("no such repository")), "claude", "session-5", "uploader-1", "uploader-1"},
	} {
		if _, err := st.pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s was accepted by the table", name)
		}
	}
}
