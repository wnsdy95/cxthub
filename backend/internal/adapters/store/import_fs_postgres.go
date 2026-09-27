//go:build postgres

package store

// Offline transfer, not a second runtime adapter. Only frozen personal-repository
// FS datasets are currently supported. Unknown records fail before DB writes;
// organization/OAuth/integration datasets require an explicit additional mapper.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type FSImportReport struct {
	Applied      bool           `json:"applied"`
	Files        int            `json:"files"`
	Bytes        int64          `json:"bytes"`
	SourceHash   string         `json:"source_hash"`
	Records      map[string]int `json:"records"`
	DerivedFiles int            `json:"derived_files_rebuilt_on_demand"`
	LegacyFiles  int            `json:"legacy_files_preserved_in_backup"`
}
type frozenFS struct {
	root   string
	files  map[string]string
	report FSImportReport
}

func inspectFrozenFS(root string) (*frozenFS, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	f := &frozenFS{root: absolute, files: map[string]string{}, report: FSImportReport{Records: map[string]int{}}}
	manifest := sha256.New()
	err = filepath.WalkDir(absolute, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in frozen source")
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular source file")
		}
		rel, err := filepath.Rel(absolute, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		category, err := classifyFSImport(rel)
		if err != nil {
			return err
		}
		r, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, r)
		r.Close()
		if err != nil {
			return err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		f.files[rel] = sum
		fmt.Fprintf(manifest, "%s\x00%s\x00%d\n", rel, sum, n)
		f.report.Files++
		f.report.Bytes += n
		if category == "derived" {
			f.report.DerivedFiles++
		}
		if category == "legacy" {
			f.report.LegacyFiles++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for path := range f.files {
		parts := strings.Split(path, "/")
		if parts[0] == "workspaces" {
			if err := f.requireOwnershipMigration(); err != nil {
				return nil, err
			}
		}
		if parts[0] == "repos" {
			if len(parts) < 3 || domain.ValidateContentHash(domain.ContentHash("sha256:"+parts[1])) != nil {
				return nil, fmt.Errorf("invalid source repository directory")
			}
			if _, ok := f.files["repos/"+parts[1]+"/repo.json"]; !ok {
				return nil, fmt.Errorf("source repository lacks identity record")
			}
		}
	}
	f.report.SourceHash = hex.EncodeToString(manifest.Sum(nil))
	return f, nil
}

func (f *frozenFS) requireOwnershipMigration() error {
	journal, err := f.read("ownership-migration-v1.complete.json")
	if err != nil {
		return fmt.Errorf("legacy ownership migration must be completed before freezing the source")
	}
	var done struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(journal, &done) != nil || done.Version != "repository-ownership-v1" {
		return domain.ErrIntegrity
	}
	return nil
}
func classifyFSImport(path string) (string, error) {
	p := strings.Split(path, "/")
	switch p[0] {
	case "users", "repositories", "members", "sessions", "invites", "repository-aliases", "repository-invite-targets", "doc-jobs", "pr-jobs", "git-changes", "git-scans", "git-deltas", "git-observations", "git-head-scans", "git-tree-nodes", "git-commit-trees":
		if strings.HasSuffix(path, ".json") {
			return "record", nil
		}
	case "git-comparisons":
		return "derived", nil // derived index over git-deltas
	case "workspaces", "ownership-migration-v1.complete.json":
		return "legacy", nil
	case "repos":
		if len(p) < 3 {
			break
		}
		switch p[2] {
		case "repo.json", "reflog.jsonl", "context-protocol", "view-revision.json", "HEAD":
			if len(p) != 3 {
				break
			}
			return "record", nil
		case "refs":
			if len(p) < 5 || (p[3] != "heads" && p[3] != "sessions" && p[3] != "tags") {
				break
			}
			return "record", nil
		case "snapshots", "history", "pending", "unsync", "memmeta":
			if len(p) != 4 {
				break
			}
			return "record", nil
		case "read-index-v1", "read-index-v2":
			return "derived", nil
		case "objects":
			if len(p) == 5 {
				switch p[3] {
				case "docs", "chunks", "memories", "memory_chunks", "settingsobjs":
					return "record", nil
				}
			}
		}
	}
	return "", fmt.Errorf("unsupported frozen-source record: %s (no data imported)", path)
}

// CheckImportTargetEmpty is a preliminary guard before applying schema changes.
// ImportFrozenFS repeats the check under table locks before touching data.
func (s *PostgresStore) CheckImportTargetEmpty(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() AND tablename NOT IN ('schema_migrations','ownership_migrations')`)
	if err != nil {
		return err
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		var occupied bool
		if err = s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{name}.Sanitize()+")").Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return fmt.Errorf("target table %s is not empty; schema and data left unchanged", name)
		}
	}
	return nil
}
func (f *frozenFS) read(path string) ([]byte, error) {
	expected, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("unlisted source file: %s", path)
	}
	b, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != expected {
		return nil, fmt.Errorf("source changed: %s", path)
	}
	return b, nil
}
func importJSON[T any](f *frozenFS, prefix string, fn func(string, T, []byte) error) error {
	paths := []string{}
	for p := range f.files {
		if strings.HasPrefix(p, prefix+"/") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		b, err := f.read(p)
		if err != nil {
			return err
		}
		var v T
		if err = json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("invalid source JSON: %s", p)
		}
		if err = fn(p, v, b); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// ImportFrozenFS applies one all-or-nothing transaction, or rolls that transaction
// back for a full dry run. The caller must stop writers and preserve the source.
// It never opens an FS runtime (which might recover journals or migrate records).
func (s *PostgresStore) ImportFrozenFS(ctx context.Context, root string, apply bool) (FSImportReport, error) {
	f, err := inspectFrozenFS(root)
	if err != nil {
		return FSImportReport{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return f.report, err
	}
	defer rollbackPG(tx)
	// Lock every runtime table before checking emptiness, excluding only schema
	// bookkeeping. A concurrent server cannot race the empty-target guard.
	rows, err := tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() ORDER BY tablename`)
	if err != nil {
		return f.report, err
	}
	tables := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return f.report, err
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return f.report, err
	}
	for _, table := range tables {
		if table == "schema_migrations" || table == "ownership_migrations" {
			continue
		}
		q := pgx.Identifier{table}.Sanitize()
		if _, err = tx.Exec(ctx, "LOCK TABLE "+q+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return f.report, err
		}
		var occupied bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+q+")").Scan(&occupied); err != nil {
			return f.report, err
		}
		if occupied {
			return f.report, fmt.Errorf("target table %s is not empty; no data imported", table)
		}
	}
	ctx = context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: s})
	if err = s.importIdentities(ctx, f); err != nil {
		return f.report, err
	}
	source := &FSStore{dataDir: f.root}
	repos := []domain.Repo{}
	for p := range f.files {
		if strings.HasPrefix(p, "repos/") && strings.HasSuffix(p, "/repo.json") {
			b, e := f.read(p)
			if e != nil {
				return f.report, e
			}
			var r domain.Repo
			if e = json.Unmarshal(b, &r); e != nil {
				return f.report, e
			}
			if p != "repos/"+hexOf(r.ID)+"/repo.json" {
				return f.report, domain.ErrIntegrity
			}
			r, e = source.GetRepo(ctx, r.ID)
			if e != nil {
				return f.report, e
			}
			repos = append(repos, r)
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	for _, r := range repos {
		if _, err = s.PutRepo(ctx, r); err != nil {
			return f.report, err
		}
		if _, err = tx.Exec(ctx, `UPDATE repos SET context_protocol=$2, protect_default=$3 WHERE id=$1`, r.ID, r.ContextProtocol, r.ProtectDefault); err != nil {
			return f.report, err
		}
		f.report.Records["repos"]++
	}
	if err = s.importAliases(ctx, f); err != nil {
		return f.report, err
	}
	for _, r := range repos {
		if err = s.importRepo(ctx, f, source, r); err != nil {
			return f.report, fmt.Errorf("repository %s: %w", r.ID, err)
		}
	}
	if err = s.importJobs(ctx, f, repos); err != nil {
		return f.report, err
	}
	// Compare the actual query projection, not only raw row counts.
	for _, r := range repos {
		a, e := source.GetManifest(ctx, r.ID)
		if e != nil {
			return f.report, e
		}
		b, e := s.GetManifest(ctx, r.ID)
		if e != nil {
			return f.report, e
		}
		if !sameImportManifest(a, b) {
			return f.report, fmt.Errorf("repository %s manifest differs after import", r.ID)
		}
	}
	again, err := inspectFrozenFS(root)
	if err != nil {
		return f.report, err
	}
	if again.report.SourceHash != f.report.SourceHash {
		return f.report, fmt.Errorf("source changed during transfer")
	}
	if apply {
		if err = tx.Commit(ctx); err != nil {
			return f.report, err
		}
		f.report.Applied = true
	}
	return f.report, nil
}

func (s *PostgresStore) importIdentities(ctx context.Context, f *frozenFS) error {
	db := s.db(ctx)
	if err := importJSON(f, "users", func(_ string, u domain.User, _ []byte) error {
		if e := s.UpsertUser(ctx, u); e != nil {
			return e
		}
		_, e := db.Exec(ctx, `UPDATE users SET created_at=$2 WHERE id=$1`, u.ID, u.CreatedAt)
		f.report.Records["users"]++
		return e
	}); err != nil {
		return err
	}
	if err := importJSON(f, "repositories", func(_ string, r domain.Repository, _ []byte) error {
		if e := s.CreateRepository(ctx, r); e != nil {
			return e
		}
		_, e := db.Exec(ctx, `UPDATE repositories SET created_at=$2 WHERE id=$1`, r.ID, r.CreatedAt)
		f.report.Records["repositories"]++
		return e
	}); err != nil {
		return err
	}
	if err := importJSON(f, "members", func(_ string, m domain.Membership, raw []byte) error {
		if m.RepositoryID == "" {
			var legacy struct {
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.Unmarshal(raw, &legacy); err != nil {
				return err
			}
			if domain.ValidateRepositoryID(legacy.WorkspaceID) != nil {
				return domain.ErrIntegrity
			}
			// The completed ownership migration leaves old files behind. Runtime
			// ListMembers ignores them; importing them would resurrect revoked roles.
			if err := f.requireOwnershipMigration(); err != nil {
				return err
			}
			f.report.LegacyFiles++
			return nil
		}
		if e := s.AddMember(ctx, m); e != nil {
			return e
		}
		_, e := db.Exec(ctx, `UPDATE memberships SET created_at=$3 WHERE repository_id=$1 AND user_id=$2`, m.RepositoryID, m.UserID, m.CreatedAt)
		f.report.Records["members"]++
		return e
	}); err != nil {
		return err
	}
	if err := importJSON(f, "invites", func(_ string, i domain.Invite, _ []byte) error {
		if e := s.CreateInvite(ctx, i); e != nil {
			return e
		}
		_, e := db.Exec(ctx, `UPDATE invites SET created_at=$2 WHERE token=$1`, i.Token, i.CreatedAt)
		f.report.Records["invites"]++
		return e
	}); err != nil {
		return err
	}
	return importJSON(f, "sessions", func(_ string, v domain.Session, _ []byte) error {
		if e := s.CreateSession(ctx, v); e != nil {
			return e
		}
		_, e := db.Exec(ctx, `UPDATE sessions SET created_at=$2 WHERE token=$1`, v.Token, v.CreatedAt)
		f.report.Records["sessions"]++
		return e
	})
}
func (s *PostgresStore) importAliases(ctx context.Context, f *frozenFS) error {
	if err := importJSON(f, "repository-aliases", func(_ string, a domain.RepositoryPathAlias, _ []byte) error {
		key := a.NamespaceID
		if key == "" {
			key = "handle:" + a.Owner
		}
		tag, e := s.db(ctx).Exec(ctx, `INSERT INTO repository_path_aliases AS a(namespace_key,owner_handle,path,repository_id,context_repo_id) VALUES($1,$2,$3,$4,NULLIF($5,'')) ON CONFLICT(namespace_key,path) DO UPDATE SET context_repo_id=EXCLUDED.context_repo_id WHERE a.repository_id=EXCLUDED.repository_id AND a.owner_handle=EXCLUDED.owner_handle`, key, a.Owner, a.Path, a.RepositoryID, string(a.ContextRepoID))
		if e == nil && tag.RowsAffected() != 1 {
			return domain.ErrIntegrity
		}
		f.report.Records["aliases"]++
		return e
	}); err != nil {
		return err
	}
	return importJSON(f, "repository-invite-targets", func(p string, ids []string, _ []byte) error {
		token := strings.TrimSuffix(filepath.Base(p), ".json")
		for _, id := range ids {
			if _, e := s.db(ctx).Exec(ctx, `INSERT INTO repository_invite_targets(token,repository_id) VALUES($1,$2)`, token, id); e != nil {
				return e
			}
			f.report.Records["invite_targets"]++
		}
		return nil
	})
}

// Copy verified representations unchanged. A conflicting global representation
// fails rather than borrowing another repository's chunks or ownership.
func (s *PostgresStore) importBlob(ctx context.Context, repo domain.ContentHash, kind string, h domain.ContentHash, raw []byte) error {
	db := s.db(ctx)
	if _, e := db.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, h, raw); e != nil {
		return e
	}
	var got []byte
	if e := db.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, h).Scan(&got); e != nil {
		return e
	}
	a, e := docDecompress(raw)
	if e != nil {
		return e
	}
	b, e := docDecompress(got)
	if e != nil {
		return e
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("conflicting representations for %s", h)
	}
	_, e = db.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,$2,$3)`, repo, kind, h)
	return e
}

func sortImportManifest(m *domain.Manifest) {
	sort.Slice(m.SnapshotIndex, func(i, j int) bool { return m.SnapshotIndex[i] < m.SnapshotIndex[j] })
	sort.Slice(m.Refs, func(i, j int) bool {
		if m.Refs[i].Kind == m.Refs[j].Kind {
			return m.Refs[i].Name < m.Refs[j].Name
		}
		return m.Refs[i].Kind < m.Refs[j].Kind
	})
}
