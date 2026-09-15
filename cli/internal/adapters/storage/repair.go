package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
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
	inspection := source.InspectReplica(ctx)
	if len(inspection.Issues) > 0 {
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
				return report, err
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
	// Whole-object encoding deliberately removes a damaged chunk dependency from
	// the repaired object's read path. Existing chunk bytes stay in place as
	// evidence and valid local-only documents continue to use them.
	for _, snap := range snapshots {
		if _, err := s.GetDoc(ctx, snap.DocHash); err != nil {
			doc, err := source.GetDoc(ctx, snap.DocHash)
			if err != nil {
				return report, err
			}
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				return report, err
			}
			if domain.HashContent(raw) != snap.DocHash {
				return report, domain.ErrHashMismatch
			}
			if err := replace(s.objectPath("docs", snap.DocHash), docCompress(raw)); err != nil {
				return report, err
			}
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
			if err == nil && local.RepoID == repoID && local.DocHash == snap.DocHash && slices.Equal(local.Parents, snap.Parents) {
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
