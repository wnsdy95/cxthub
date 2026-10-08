package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type RepairReport struct {
	Backup   string   `json:"backup"`
	Repaired []string `json:"repaired"`
	Issues   []string `json:"issues"`
}

// RepairFromReplica restores verified server objects in place. It never swaps
// the live directory or overwrites healthy mutable local pointers. Each damaged
// predecessor is durably quarantined before atomic replacement; interrupted
// runs can be retried without replaying already repaired objects. Normal ref
// and snapshot locks exclude captures modifying those records during repair.
func (s *FileStore) RepairFromReplica(ctx context.Context, source *FileStore, repoID string, refs []domain.Ref, backup string) (RepairReport, error) {
	report := RepairReport{Backup: backup, Repaired: []string{}, Issues: []string{}}
	if source.repoRoot == s.repoRoot {
		return report, fmt.Errorf("repair source must be isolated")
	}
	// Both replicas keep every dependency alive through final metadata adoption.
	// Shared leases also permit the source's scoped verified-chunk callback.
	err := source.WithObjectsRetained(ctx, func() error {
		return s.WithObjectsRetained(ctx, func() error {
			var err error
			report, err = s.repairFromRetainedReplica(ctx, source, repoID, refs, backup)
			return err
		})
	})
	return report, err
}

func (s *FileStore) repairFromRetainedReplica(ctx context.Context, source *FileStore, repoID string, refs []domain.Ref, backup string) (RepairReport, error) {
	report := RepairReport{Backup: backup, Repaired: []string{}, Issues: []string{}}
	inspection := source.InspectReplica(ctx)
	if !inspection.Completed || len(inspection.Issues) > 0 {
		return report, fmt.Errorf("unverified server replica: %v", inspection.Issues)
	}
	snapshots, err := source.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return report, err
	}
	for _, snap := range snapshots {
		if snap.RepoID != repoID {
			return report, domain.ErrHashMismatch
		}
	}
	for _, ref := range refs {
		if ref.RepoID != repoID {
			return report, domain.ErrHashMismatch
		}
		if err := domain.ValidateRef(ref); err != nil {
			return report, err
		}
		if ref.Target != "" {
			if _, err := source.GetSnapshot(ctx, ref.Target); err != nil {
				return report, fmt.Errorf("repair ref %s/%s points to %s absent from the verified source: %w", ref.Kind, ref.Name, ref.Target, err)
			}
		}
	}
	// Validate every server memory/settings object before the first live write.
	for _, kind := range []string{"memories", "settingsobjs"} {
		entries, err := readCxtDir(filepath.Join(source.storeDir(), "objects", kind))
		if err != nil && !os.IsNotExist(err) {
			return report, err
		}
		for _, entry := range entries {
			hash, ok := hashFromObjectName(entry.Name())
			if !ok {
				continue
			}
			if kind == "memories" {
				_, err = source.GetMemory(ctx, hash)
			} else {
				_, err = source.GetSettingsObject(ctx, hash)
			}
			if err != nil {
				return report, err
			}
		}
	}
	replace := func(path string, data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(s.storeDir(), path)
		if err != nil || strings.HasPrefix(relative, "..") {
			return fmt.Errorf("unsafe repair target")
		}
		previous, err := readCxtFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			// Content-addressing the quarantine name preserves every distinct damaged
			// predecessor even if an earlier repair attempt stopped after backup.
			hash := strings.TrimPrefix(string(domain.HashContent(previous)), "sha256:")
			saved := filepath.Join(backup, ".cxt", relative+"."+hash)
			if err := writeAtomic(saved, previous); err != nil {
				return err
			}
		}
		if err := writeAtomic(path, data); err != nil {
			return err
		}
		report.Repaired = append(report.Repaired, relative)
		return nil
	}
	for _, snap := range snapshots {
		ref := snap.DocumentRef()
		if err := s.inspectDocReference(ctx, ref, nil); err == nil {
			continue
		}
		if ref.Identity == domain.DocumentIdentityRootV1 {
			if err := s.repairRootFromReplica(ctx, source, ref, replace); err != nil {
				return report, err
			}
			continue
		}
		// Legacy whole-object encoding removes damaged chunk dependencies from
		// this read path, preserving the old chunks as local-only evidence.
		doc, err := source.GetDocReference(ctx, ref)
		if err != nil {
			return report, err
		}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			return report, err
		}
		if domain.HashContent(raw) != ref.Hash {
			return report, domain.ErrHashMismatch
		}
		if err := replace(s.objectPath("docs", ref.Hash), docCompress(raw)); err != nil {
			return report, err
		}
	}
	// Include all historical memory objects, not just each current attachment.
	entries, err := readCxtDir(filepath.Join(source.storeDir(), "objects", "memories"))
	if err != nil && !os.IsNotExist(err) {
		return report, err
	}
	for _, entry := range entries {
		hash, ok := hashFromObjectName(entry.Name())
		if !ok {
			continue
		}
		digest, err := source.GetMemory(ctx, hash)
		if err != nil {
			return report, err
		}
		if _, err := s.GetMemory(ctx, hash); err == nil {
			continue
		}
		raw, err := json.Marshal(digest)
		if err != nil {
			return report, err
		}
		if domain.HashContent(raw) != hash {
			return report, domain.ErrHashMismatch
		}
		if err := replace(s.objectPath("memories", hash), raw); err != nil {
			return report, err
		}
	}
	settings, err := readCxtDir(filepath.Join(source.storeDir(), "objects", "settingsobjs"))
	if err != nil && !os.IsNotExist(err) {
		return report, err
	}
	for _, entry := range settings {
		hash, ok := hashFromObjectName(entry.Name())
		if !ok {
			continue
		}
		if _, err := s.GetSettingsObject(ctx, hash); err == nil {
			continue
		}
		if _, err := source.GetSettingsObject(ctx, hash); err != nil {
			return report, err
		}
		raw, err := readCxtFile(source.objectPath("settingsobjs", hash))
		if err != nil {
			return report, err
		}
		if err := replace(s.objectPath("settingsobjs", hash), raw); err != nil {
			return report, err
		}
	}
	for _, snap := range snapshots {
		err := s.withSnapshotMutationLock(ctx, snap.ID, func() error {
			local, err := s.GetSnapshot(ctx, snap.ID)
			if err == nil && local.RepoID == repoID && local.DocumentRef() == snap.DocumentRef() && slices.Equal(local.Parents, snap.Parents) {
				return nil
			}
			raw, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			return replace(s.objectPath("snapshots", snap.ID), raw)
		})
		if err != nil {
			return report, err
		}
	}
	events, err := source.listHistoryEvents(repoID)
	if err != nil {
		return report, err
	}
	// Bypass redo only for inspection: an invalid redo is never discarded or
	// guessed. Objects repaired above may make a previously blocked redo valid.
	err = s.withMutationLock(ctx, "refs", "repo", func() error {
		if err := s.recoverWorkingCommit(); err != nil {
			return fmt.Errorf("local transaction still needs evidence: %w", err)
		}
		if err := s.recoverCheckoutTransition(); err != nil {
			return fmt.Errorf("checkout transition still needs evidence: %w", err)
		}
		if err := s.recoverTrackingAttachment(); err != nil {
			return fmt.Errorf("tracking attachment still needs evidence: %w", err)
		}
		if err := s.recoverWorkingMemory(ctx); err != nil {
			return fmt.Errorf("working memory still needs evidence: %w", err)
		}
		for _, e := range events {
			path := filepath.Join(s.storeDir(), "history", e.ID+".json")
			raw, err := readCxtFile(path)
			var old domain.HistoryEvent
			if err == nil && json.Unmarshal(raw, &old) == nil && reflect.DeepEqual(old, e) {
				continue
			}
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := replace(path, data); err != nil {
				return err
			}
		}
		for _, ref := range refs {
			// Preserve valid local refs even when the server has advanced or diverged.
			if _, err := s.getRefRaw(ctx, repoID, ref.Kind, ref.Name); err == nil {
				continue
			}
			data := string(encodeRef(ref))
			if ref.Symbolic != "" {
				data = "ref: refs/heads/" + strings.TrimPrefix(ref.Symbolic, "refs/heads/") + "\n"
			}
			path := s.refPath(map[domain.RefKind]string{domain.RefBranch: "heads", domain.RefSession: "sessions", domain.RefTag: "tags"}[ref.Kind], ref.Name)
			if ref.Kind == domain.RefHEAD {
				path = filepath.Join(s.storeDir(), "HEAD")
			}
			if err := replace(path, []byte(data)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	report.Issues = s.InspectReplica(ctx).Issues
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	if err := writeAtomic(filepath.Join(backup, ".cxt", "report.json"), data); err != nil {
		return report, err
	}
	if len(report.Issues) > 0 {
		return report, fmt.Errorf("verified server objects restored; %d local issue(s) have no verified replacement", len(report.Issues))
	}
	return report, nil
}

// Root repair never substitutes the canonical CIR hash for the manifest identity.
// The caller retains both stores; the source capability owns its allowed chunk
// set, and the root lock serializes descriptor installation with ordinary pull.
func (s *FileStore) repairRootFromReplica(ctx context.Context, source *FileStore, ref domain.DocumentRef, replace func(string, []byte) error) error {
	supported, err := source.WithVerifiedDocChunks(ctx, ref, func(chunks outbound.DocumentChunks) error {
		if chunks.Representation.DocumentRef() != ref {
			return domain.ErrHashMismatch
		}
		manifest, err := chunks.Representation.ConversationManifest()
		if err != nil {
			return err
		}
		raw, err := domain.CanonicalConversationManifest(manifest)
		if err != nil {
			return err
		}
		_, err = s.withOSLock(ctx, "root-document", hexOf(ref.Hash), syscall.LOCK_EX, true, func() error {
			if _, err := s.readRootDocument(ctx, ref, false); err == nil {
				return nil
			}
			seen := make(map[domain.ContentHash]bool, len(manifest.Chunks))
			for _, chunk := range manifest.Chunks {
				if seen[chunk.Hash] {
					continue
				}
				seen[chunk.Hash] = true
				body, err := chunks.ReadChunk(ctx, chunk.Hash)
				if err != nil {
					return err
				}
				path := s.objectPath("chunks", chunk.Hash)
				current, err := readRootObject(ctx, path, int(chunk.Bytes))
				if err == nil && bytes.Equal(current, body) {
					continue
				}
				if err := replace(path, docCompress(body)); err != nil {
					return err
				}
			}
			// Check the complete destination before making a new descriptor visible.
			if _, err := s.verifyRootDocument(ctx, ref, manifest, false); err != nil {
				return err
			}
			path := s.objectPath("docs", ref.Hash)
			current, err := readRootObject(ctx, path, domain.MaxConversationManifestBytes)
			if err == nil && bytes.Equal(current, raw) {
				return nil
			}
			return replace(path, docCompress(raw))
		})
		return err
	})
	if err != nil {
		return err
	}
	if !supported {
		return domain.ErrUnsupportedDocumentIdentity
	}
	return ctx.Err()
}
