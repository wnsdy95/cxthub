//go:build postgres

package store

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRootPGLegacyIndexCannotHideStoredIdentity(t *testing.T) {
	for _, mode := range []string{"missing_snapshot", "legacy_snapshot"} {
		t.Run(mode, func(t *testing.T) {
			st, ctx := chunkReusePG(t)
			repo, ns := rootPGRepo(t, ctx, st)
			f := rootFixture(t, "needle "+mode+" "+string(repo))
			seedRootPG(t, ctx, st, repo, f)
			if mode == "legacy_snapshot" {
				if err := st.PutSnapshot(ctx, domain.Snapshot{ID: f.hash, RepoID: repo, DocHash: f.hash, Branch: "main", Fidelity: domain.FidelityFull, Provider: domain.ProviderCodex}); err != nil {
					t.Fatal(err)
				}
			}
			// A stale empty projection previously allowed exhausted/negative
			// requests to return success without inspecting the current source.
			if _, err := st.pool.Exec(ctx, `INSERT INTO doc_read_indexes_v3(hash,version,envelope,event_count) VALUES($1,1,$2,0)`, f.hash, f.manifest.Envelope); err != nil {
				t.Fatal(err)
			}
			before := rootPGImage(t, ctx, st, repo, ns)
			if _, err := st.DocReadIndex(ctx, repo, f.hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
				t.Fatal("root accepted through warm legacy index", err)
			}
			for _, query := range []string{"needle", "absent"} {
				if _, err := st.SearchDocEvents(ctx, repo, f.hash, query, -1, 10); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
					t.Fatal("root accepted through warm legacy search", query, err)
				}
			}
			if after := rootPGImage(t, ctx, st, repo, ns); after != before {
				t.Fatal("rejected legacy read mutated root or index state")
			}
		})
	}
}
