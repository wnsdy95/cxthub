//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) importJobs(ctx context.Context, f *frozenFS, repos []domain.Repo) error {
	db := s.db(ctx)
	// Scope-less FS tree records are bound only by matching their opaque filename
	// against a known repository/origin pair. Never infer an owner from the OID.
	origins := map[domain.ContentHash]map[string]bool{}
	origin := func(repo domain.ContentHash, url string) {
		if url != "" {
			if origins[repo] == nil {
				origins[repo] = map[string]bool{}
			}
			origins[repo][url] = true
		}
	}
	for _, r := range repos {
		origin(r.ID, r.GitRemoteURL)
	}
	knownRepos := map[domain.ContentHash]domain.Repo{}
	for _, repo := range repos {
		knownRepos[repo.ID] = repo
	}
	if e := importJSON(f, "doc-jobs", func(path string, j domain.DocFinalizationJob, _ []byte) error {
		repo, ok := knownRepos[j.RepoID]
		if !ok {
			return domain.ErrIntegrity
		}
		if err := repo.RequiredDocIdentity.Validate(); err != nil {
			return err
		}
		if j.DocIdentity == domain.DocumentIdentityRootV1 && repo.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
			return fmt.Errorf("root job without repository requirement: %w", domain.ErrIntegrity)
		}
		if filepath.Base(path) != opaqueName(string(j.RepoID)+":"+j.ID)+".json" {
			return domain.ErrIntegrity
		}
		var e error
		j, e = verifyFrozenDocJob(ctx, f, j, time.Now().UTC())
		if e != nil {
			return e
		}
		if e := s.writeDocJob(ctx, j); e != nil {
			return e
		}
		f.report.Records["doc_jobs"]++
		return nil
	}); e != nil {
		return e
	}
	if e := importJSON(f, "pr-jobs", func(_ string, j domain.PRPromotionJob, b []byte) error {
		if e := j.Validate(); e != nil {
			return e
		}
		origin(j.RepoID, j.GitOrigin)
		_, e := db.Exec(ctx, `INSERT INTO pr_promotion_jobs(repo_id,id,payload,state,created_at,next_attempt,lease_until,version) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, j.RepoID, j.ID, b, j.State, j.CreatedAt, j.NextAttempt, j.LeaseUntil, j.Version)
		f.report.Records["pr_jobs"]++
		return e
	}); e != nil {
		return e
	}
	if e := importJSON(f, "git-changes", func(_ string, j domain.GitChangeJob, b []byte) error {
		if e := j.Validate(); e != nil {
			return e
		}
		origin(j.RepoID, j.GitOrigin)
		_, e := db.Exec(ctx, `INSERT INTO git_change_jobs(repo_id,id,payload,state,next_attempt,lease_until,version) VALUES($1,$2,$3,$4,$5,$6,$7)`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil, j.Version)
		f.report.Records["git_changes"]++
		return e
	}); e != nil {
		return e
	}
	if e := importJSON(f, "git-scans", func(_ string, j domain.GitScanJob, b []byte) error {
		if e := j.Validate(); e != nil {
			return e
		}
		origin(j.RepoID, j.GitOrigin)
		_, e := db.Exec(ctx, `INSERT INTO git_scan_jobs(repo_id,id,payload,state,next_attempt,lease_until) VALUES($1,$2,$3,$4,$5,$6)`, j.RepoID, j.ID, b, j.State, j.NextAttempt, j.LeaseUntil)
		f.report.Records["git_scans"]++
		return e
	}); e != nil {
		return e
	}
	if e := importJSON(f, "git-deltas", func(_ string, j domain.GitDeltaRecord, b []byte) error {
		if e := j.Validate(); e != nil {
			return e
		}
		origin(j.RepoID, j.GitOrigin)
		_, e := db.Exec(ctx, `INSERT INTO git_commit_deltas(repo_id,id,origin,commit_oid,parent_oid,payload,keys,inverse_keys) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, j.RepoID, j.ID, j.GitOrigin, j.Delta.Commit, j.Delta.Parent, b, j.Keys, j.InverseKeys)
		f.report.Records["git_deltas"]++
		return e
	}); e != nil {
		return e
	}
	if e := importJSON(f, "git-observations", func(p string, j domain.GitRefObservation, b []byte) error {
		if e := j.Validate(); e != nil {
			return e
		}
		origin(j.RepoID, j.GitOrigin)
		info, e := os.Stat(filepath.Join(f.root, p))
		if e != nil {
			return e
		}
		_, e = db.Exec(ctx, `INSERT INTO git_ref_observations(repo_id,id,payload,observed_at) VALUES($1,$2,$3,$4)`, j.RepoID, j.ID, b, info.ModTime())
		f.report.Records["git_observations"]++
		return e
	}); e != nil {
		return e
	}
	if e := importJSON(f, "git-head-scans", func(_ string, j domain.GitHeadScan, b []byte) error {
		if e := j.Validate(); e != nil {
			return e
		}
		origin(j.RepoID, j.GitOrigin)
		_, e := db.Exec(ctx, `INSERT INTO git_head_scans(repo_id,origin,payload) VALUES($1,$2,$3)`, j.RepoID, j.GitOrigin, b)
		f.report.Records["git_head_scans"]++
		return e
	}); e != nil {
		return e
	}
	scope := func(p, oid string) (domain.ContentHash, string, error) {
		matches := 0
		var found domain.ContentHash
		var url string
		for repo, urls := range origins {
			for u := range urls {
				if filepath.Base(p) == opaqueName(string(repo)+":"+u+":"+oid)+".json" {
					matches++
					found = repo
					url = u
				}
			}
		}
		if matches != 1 {
			return "", "", fmt.Errorf("cannot prove Git evidence ownership")
		}
		return found, url, nil
	}
	if e := importJSON(f, "git-tree-nodes", func(p string, n domain.GitTreeNode, b []byte) error {
		if e := n.Validate(); e != nil {
			return e
		}
		repo, url, e := scope(p, n.OID)
		if e != nil {
			return e
		}
		_, e = db.Exec(ctx, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload) VALUES($1,$2,$3,$4)`, repo, url, n.OID, b)
		f.report.Records["git_tree_nodes"]++
		return e
	}); e != nil {
		return e
	}
	return importJSON(f, "git-commit-trees", func(p string, c domain.GitCommitTree, b []byte) error {
		if e := c.Validate(); e != nil {
			return e
		}
		repo, url, e := scope(p, c.Commit)
		if e != nil {
			return e
		}
		_, e = db.Exec(ctx, `INSERT INTO git_commit_trees(repo_id,origin,commit_oid,payload) VALUES($1,$2,$3,$4)`, repo, url, c.Commit, b)
		f.report.Records["git_commit_trees"]++
		return e
	})
}

// Compare protocol projections without clock noise or storage ordering.
func sameImportManifest(a, b domain.Manifest) bool {
	a.UpdatedAt = b.UpdatedAt
	canonical := func(m domain.Manifest) []byte {
		sortImportManifest(&m)
		out, _ := json.Marshal(m)
		return out
	}
	return string(canonical(a)) == string(canonical(b))
}
