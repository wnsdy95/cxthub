//go:build postgres

package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) importRepo(ctx context.Context, f *frozenFS, source *FSStore, r domain.Repo) error {
	prefix := "repos/" + hexOf(r.ID)
	// Components first. Verify content hashes independently, including currently
	// unreferenced objects, so queued uploads retain all of their input chunks.
	for _, kind := range []struct{ dir, wire string }{{"chunks", "chunk"}, {"memory_chunks", "memory_chunk"}, {"docs", "doc"}, {"memories", "memory"}} {
		paths := []string{}
		for p := range f.files {
			if strings.HasPrefix(p, prefix+"/objects/"+kind.dir+"/") {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		for _, p := range paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			h, ok := hashFromObjectName(filepath.Base(p))
			if !ok {
				return domain.ErrIntegrity
			}
			raw, err := f.read(p)
			if err != nil {
				return err
			}
			body, err := docDecompress(raw)
			if err != nil {
				return err
			}
			switch kind.wire {
			case "chunk", "memory_chunk":
				if domain.HashContent(body) != h {
					return domain.ErrIntegrity
				}
			case "doc":
				if err = verifyFrozenDoc(ctx, source, r.ID, h, body); err != nil {
					return fmt.Errorf("document %s: %w", h, err)
				}
			case "memory":
				if _, err = source.GetMemory(ctx, r.ID, h); err != nil {
					return err
				}
			}
			if err = s.importBlob(ctx, r.ID, kind.wire, h, raw); err != nil {
				return err
			}
			f.report.Records[kind.wire]++
		}
	}
	if err := importJSON(f, prefix+"/objects/settingsobjs", func(p string, b domain.SettingsBundle, _ []byte) error {
		h, ok := hashFromObjectName(filepath.Base(p))
		if !ok {
			return domain.ErrIntegrity
		}
		if e := s.PutSettingsObject(ctx, r.ID, h, b); e != nil {
			return e
		}
		f.report.Records["settings_objects"]++
		return nil
	}); err != nil {
		return err
	}
	if err := importJSON(f, prefix+"/snapshots", func(p string, snap domain.Snapshot, _ []byte) error {
		if snap.RepoID != r.ID || filepath.Base(p) != hexOf(snap.ID) {
			return domain.ErrIntegrity
		}
		if e := s.PutSnapshot(ctx, snap); e != nil {
			return e
		}
		if _, e := s.db(ctx).Exec(ctx, `UPDATE snapshots SET created_at=$3 WHERE repo_id=$1 AND id=$2`, r.ID, snap.ID, snap.CreatedAt); e != nil {
			return e
		}
		got, e := s.GetSnapshot(ctx, r.ID, snap.ID)
		if e != nil {
			return e
		}
		// PostgreSQL timestamps have microsecond precision. Membership is a
		// query projection, reconstructed from imported history rather than stored.
		normalize := func(v domain.Snapshot) domain.Snapshot {
			v.CreatedAt = v.CreatedAt.UTC().Truncate(time.Microsecond)
			v.Branches = nil
			if len(v.Parents) == 0 {
				v.Parents = nil
			}
			if len(v.GraftParents) == 0 {
				v.GraftParents = nil
			}
			if len(v.Models) == 0 {
				v.Models = nil
			}
			return v
		}
		if !reflect.DeepEqual(normalize(snap), normalize(got)) {
			return fmt.Errorf("snapshot metadata changed: %s", snap.ID)
		}
		f.report.Records["snapshots"]++
		return nil
	}); err != nil {
		return err
	}
	// Preserve events as historical facts, without replaying their business
	// commands (which would move today's branches while importing past events).
	if err := importJSON(f, prefix+"/history", func(_ string, e domain.HistoryEvent, b []byte) error {
		if e.RepoID != string(r.ID) {
			return domain.ErrIntegrity
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return err
		}
		_, err := s.db(ctx).Exec(ctx, `INSERT INTO context_history(repo_id,id,event,received_at) VALUES($1,$2,$3,$4)`, r.ID, e.ID, b, e.CreatedAt)
		f.report.Records["history"]++
		return err
	}); err != nil {
		return err
	}
	refs, err := source.listRefsRaw(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err = domain.ValidateRef(ref); err != nil {
			return err
		}
		if ref.Target != "" {
			if err = requireSnapshotIDsPG(ctx, ctx.Value(repositoryTxKey{}).(*repositoryTx), r.ID, ref.Target); err != nil {
				return fmt.Errorf("ref target missing: %w", err)
			}
		}
		if _, err = s.db(ctx).Exec(ctx, `INSERT INTO refs(repo_id,kind,name,target,symbolic,branch_id) VALUES($1,$2,$3,NULLIF($4,''),$5,$6)`, r.ID, ref.Kind, ref.Name, string(ref.Target), ref.Symbolic, ref.BranchID); err != nil {
			return err
		}
		if ref.Kind == domain.RefBranch {
			if _, err = s.db(ctx).Exec(ctx, `INSERT INTO branches(repo_id,name) VALUES($1,$2)`, r.ID, ref.Name); err != nil {
				return err
			}
		}
		f.report.Records["refs"]++
	}
	if err = importJSON(f, prefix+"/pending", func(_ string, p domain.Pending, _ []byte) error {
		if e := s.PutPending(ctx, r.ID, p); e != nil {
			return e
		}
		f.report.Records["pending"]++
		return nil
	}); err != nil {
		return err
	}
	if err = importJSON(f, prefix+"/unsync", func(_ string, p domain.Unsync, _ []byte) error {
		if e := s.PutUnsync(ctx, r.ID, p); e != nil {
			return e
		}
		f.report.Records["unsync"]++
		return nil
	}); err != nil {
		return err
	}
	if err = importJSON(f, prefix+"/memmeta", func(_ string, p domain.MemoryDigest, _ []byte) error { return s.PutMemoryMeta(ctx, r.ID, p) }); err != nil {
		return err
	}
	if _, ok := f.files[prefix+"/reflog.jsonl"]; ok {
		b, e := f.read(prefix + "/reflog.jsonl")
		if e != nil {
			return e
		}
		scanner := bufio.NewScanner(bytes.NewReader(b))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var entry domain.RefLogEntry
			if e = json.Unmarshal(scanner.Bytes(), &entry); e != nil {
				return e
			}
			if _, e = s.db(ctx).Exec(ctx, `INSERT INTO reflog(repo_id,kind,name,old,new,created_at) VALUES($1,$2,$3,$4,$5,$6)`, r.ID, entry.Kind, entry.Name, entry.Old, entry.New, entry.CreatedAt); e != nil {
				return e
			}
			f.report.Records["reflog"]++
		}
		if e = scanner.Err(); e != nil {
			return e
		}
	}
	if _, ok := f.files[prefix+"/view-revision.json"]; ok {
		b, e := f.read(prefix + "/view-revision.json")
		if e != nil {
			return e
		}
		var rev domain.RepositoryRevision
		if e = json.Unmarshal(b, &rev); e != nil {
			return e
		}
		if _, e = s.db(ctx).Exec(ctx, `INSERT INTO repository_revisions(repo_id,graph,pending,evidence) VALUES($1,$2,$3,$4)`, r.ID, rev.Graph, rev.Pending, rev.Evidence); e != nil {
			return e
		}
	}
	var missing bool
	if err = s.db(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM snapshots s CROSS JOIN LATERAL unnest(s.parents||s.graft_parents) p WHERE s.repo_id=$1 AND NOT EXISTS(SELECT 1 FROM snapshots parent WHERE parent.repo_id=s.repo_id AND parent.id=p))`, r.ID).Scan(&missing); err != nil {
		return err
	}
	if missing {
		return fmt.Errorf("missing parent snapshot")
	}
	return ensureNoReachabilityCycle(ctx, ctx.Value(repositoryTxKey{}).(*repositoryTx), r.ID)
}

// Verify the complete document identity with bounded component memory. This
// reproduces the public chunk-format contract; it never trusts a read/search cache.
func verifyFrozenDoc(ctx context.Context, source *FSStore, repo, want domain.ContentHash, body []byte) error {
	man, chunked := domain.ParseDocChunkManifest(body)
	if !chunked {
		var cir domain.CIRDocument
		if err := json.Unmarshal(body, &cir); err != nil {
			return domain.ErrIntegrity
		}
		return domain.ValidateSessionDocHash(domain.SessionDoc{Hash: want, CIR: cir})
	}
	if !domain.SupportedChunkFormat(man.Format) || !json.Valid(man.Envelope) {
		return domain.ErrIntegrity
	}
	h := sha256.New()
	h.Write([]byte(`{"envelope":`))
	h.Write(man.Envelope)
	h.Write([]byte(`,"events":[`))
	first := true
	for _, ch := range man.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := source.GetChunk(ctx, repo, ch)
		if err != nil {
			return err
		}
		if domain.HashContent(b) != ch {
			return domain.ErrIntegrity
		}
		if man.Format == domain.ChunkFormatV2 {
			h.Write(b)
		} else {
			for _, event := range domain.SplitDocChunk(b) {
				if !first {
					h.Write([]byte(","))
				}
				first = false
				h.Write(event)
			}
		}
	}
	h.Write([]byte(`]}`))
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != string(want) {
		return domain.ErrIntegrity
	}
	return nil
}
