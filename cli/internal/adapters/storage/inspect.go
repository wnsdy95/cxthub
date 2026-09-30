package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type ReplicaInspection struct {
	// Completed means every inspection phase finished, even if damage was found.
	Completed        bool     `json:"completed"`
	Snapshots        int      `json:"snapshots"`
	DocumentsChecked int      `json:"documents_checked"`
	HistoryEvents    int      `json:"history_events"`
	Issues           []string `json:"issues"`
}

// InspectReplica never acquires a mutation lock, recovers a journal, repacks a
// document, or creates a directory. It reports observable content/reference
// failures; it is not evidence of who caused damage or an atomic disk snapshot.
func (s *FileStore) InspectReplica(ctx context.Context) ReplicaInspection {
	return s.inspectReplica(ctx, nil)
}

// beforeRead is an optional per-call test hook at actual read boundaries. It
// neither replaces validation nor adds shared state to the store.
func (s *FileStore) inspectReplica(ctx context.Context, beforeRead func(string)) ReplicaInspection {
	r := ReplicaInspection{Issues: []string{}}
	stop := func(label string) bool {
		if err := ctx.Err(); err != nil {
			r.Issues = append(r.Issues, fmt.Sprintf("incomplete inspection at %s: %v", label, err))
			return true
		}
		return false
	}
	read := func(label string) bool {
		if stop(label) {
			return true
		}
		if beforeRead != nil {
			beforeRead(label)
		}
		return stop(label)
	}
	// Check cancellation after reads too, including helpers that do not accept a
	// context. A canceled read is incomplete inspection, not replica damage.
	issue := func(label string, err error) bool {
		if stop(label) {
			return true
		}
		if err != nil {
			r.Issues = append(r.Issues, fmt.Sprintf("%s: %v", label, err))
		}
		return false
	}
	if stop("entry") || read("snapshot metadata") {
		return r
	}
	snaps, err := s.ListSnapshots(ctx, "", "")
	r.Snapshots = len(snaps)
	if issue("snapshot metadata", err) {
		return r
	}
	for _, snap := range snaps {
		label := "document " + string(snap.DocHash)
		if read(label) {
			return r
		}
		if issue(label, s.VerifyDoc(ctx, snap.DocHash)) {
			return r
		}
		r.DocumentsChecked++
		for _, parent := range snap.ReachabilityParents() {
			label := "parent " + string(parent)
			if read(label) {
				return r
			}
			_, err := s.GetSnapshot(ctx, parent)
			if issue(label, err) {
				return r
			}
		}
		for _, hash := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
			if stop("settings") {
				return r
			}
			if hash != "" {
				label := "settings " + string(hash)
				if read(label) {
					return r
				}
				_, err := s.GetSettingsObject(ctx, hash)
				if issue(label, err) {
					return r
				}
			}
		}
		if stop("memory") {
			return r
		}
		if snap.MemoryHash != "" {
			label := "memory " + string(snap.MemoryHash)
			if read(label) {
				return r
			}
			_, err := s.GetMemory(ctx, snap.MemoryHash)
			if issue(label, err) {
				return r
			}
		}
	}
	if read("references") {
		return r
	}
	refs, err := s.listRefsRaw(ctx, "")
	if issue("references", err) {
		return r
	}
	for _, ref := range refs {
		if stop("ref " + ref.Name) {
			return r
		}
		if ref.Target != "" {
			label := "ref " + ref.Name
			if read(label) {
				return r
			}
			_, err := s.GetSnapshot(ctx, ref.Target)
			if issue(label, err) {
				return r
			}
		}
	}
	if read("history") {
		return r
	}
	events, err := s.listHistoryEvents("")
	r.HistoryEvents = len(events)
	if issue("history", err) {
		return r
	}
	if err == nil {
		_, err = domain.ProjectContextBranches(events)
		if issue("branch identities", err) {
			return r
		}
	}
	for _, e := range events {
		if stop("history event " + e.ID) {
			return r
		}
		for _, id := range []domain.ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
			if stop("history root") {
				return r
			}
			if id != "" {
				label := "history root " + string(id)
				if read(label) {
					return r
				}
				_, err := s.GetSnapshot(ctx, id)
				if issue(label, err) {
					return r
				}
			}
		}
		if stop("history memory") {
			return r
		}
		if e.MemoryHash != "" {
			label := "history memory " + string(e.MemoryHash)
			if read(label) {
				return r
			}
			_, err := s.GetMemory(ctx, e.MemoryHash)
			if issue(label, err) {
				return r
			}
		}
	}
	for _, kind := range []string{"worktrees", "branch-bindings"} {
		if read(kind) {
			return r
		}
		dir := filepath.Join(s.storeDir(), kind)
		entries, err := readCxtDir(dir)
		if stop(kind) {
			return r
		}
		if os.IsNotExist(err) {
			continue
		}
		if issue(kind, err) {
			return r
		}
		for _, entry := range entries {
			if stop(kind + " entry " + entry.Name()) {
				return r
			}
			if kind == "worktrees" {
				clone := *s
				clone.worktreeID = entry.Name()
				label := "worktree " + entry.Name()
				if read(label) {
					return r
				}
				position, err := clone.readPosition()
				if stop(label) {
					return r
				}
				if err == domain.ErrNotFound {
					continue
				}
				if issue(label, err) {
					return r
				}
				if err != nil {
					continue
				}
				for _, id := range []domain.ContentHash{position.Snapshot, position.SharedTarget, position.MemorySource} {
					if stop("worktree context") {
						return r
					}
					if id != "" {
						label := "worktree context " + string(id)
						if read(label) {
							return r
						}
						snap, err := s.GetSnapshot(ctx, id)
						if err == nil && snap.RepoID != position.RepoID {
							err = domain.ErrHashMismatch
						}
						if issue(label, err) {
							return r
						}
					}
				}
				if stop("worktree memory") {
					return r
				}
				if position.MemoryHash != "" {
					label := "worktree memory " + position.MemoryHash
					if read(label) {
						return r
					}
					_, err := s.GetMemory(ctx, position.MemoryHash)
					if issue(label, err) {
						return r
					}
				}
				if stop("worktree selection") {
					return r
				}
				if position.Selection != nil {
					if issue("worktree selection", domain.ValidateHistoryEvent(*position.Selection)) {
						return r
					}
				}
			} else {
				path := filepath.Join(dir, entry.Name())
				label := "local binding " + entry.Name()
				if read(label) {
					return r
				}
				raw, err := readCxtFile(path)
				if issue(label, err) {
					return r
				}
				if err != nil {
					continue
				}
				var record localBranchRecord
				err = json.Unmarshal(raw, &record)
				if issue(label, err) {
					return r
				}
				if err != nil {
					continue
				}
				if read(label) {
					return r
				}
				_, err = s.readLocalBinding(record.Event.RepoID, record.Event.LocalBranch)
				if issue(label, err) {
					return r
				}
				if path != s.localBranchPath(record.Event.LocalBranch) {
					if issue("local binding path", domain.ErrHashMismatch) {
						return r
					}
				}
			}
		}
	}
	for _, name := range []string{"working-commit.json", "checkout-transition.json"} {
		if read(name) {
			return r
		}
		_, err := readCxtFile(filepath.Join(s.storeDir(), name))
		if stop(name) {
			return r
		}
		if err == nil {
			r.Issues = append(r.Issues, "pending local transaction: "+name+" (normal mutation retries recovery)")
		} else if !os.IsNotExist(err) {
			if issue(name, err) {
				return r
			}
		}
	}
	if stop("completion") {
		return r
	}
	r.Completed = true
	return r
}
