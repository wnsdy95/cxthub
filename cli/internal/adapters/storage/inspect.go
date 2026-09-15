package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type ReplicaInspection struct {
	Snapshots     int      `json:"snapshots"`
	HistoryEvents int      `json:"history_events"`
	Issues        []string `json:"issues"`
}

// InspectReplica never acquires a mutation lock, recovers a journal, repacks a
// document, or creates a directory. It reports observable content/reference
// failures; it is not evidence of who caused damage or an atomic disk snapshot.
func (s *FileStore) InspectReplica(ctx context.Context) ReplicaInspection {
	r := ReplicaInspection{Issues: []string{}}
	issue := func(label string, err error) {
		if err != nil {
			r.Issues = append(r.Issues, fmt.Sprintf("%s: %v", label, err))
		}
	}
	snaps, err := s.ListSnapshots(ctx, "", "")
	issue("snapshot metadata", err)
	r.Snapshots = len(snaps)
	for _, snap := range snaps {
		_, err := s.GetDoc(ctx, snap.DocHash)
		issue("document "+string(snap.DocHash), err)
		for _, parent := range snap.ReachabilityParents() {
			_, err := s.GetSnapshot(ctx, parent)
			issue("parent "+string(parent), err)
		}
		if snap.MemoryHash != "" {
			_, err := s.GetMemory(ctx, snap.MemoryHash)
			issue("memory "+string(snap.MemoryHash), err)
		}
	}
	refs, err := s.listRefsRaw(ctx, "")
	issue("references", err)
	for _, ref := range refs {
		if ref.Target != "" {
			_, err := s.GetSnapshot(ctx, ref.Target)
			issue("ref "+ref.Name, err)
		}
	}
	events, err := s.listHistoryEvents("")
	issue("history", err)
	r.HistoryEvents = len(events)
	if err == nil {
		_, err = domain.ProjectContextBranches(events)
		issue("branch identities", err)
	}
	for _, e := range events {
		for _, id := range []domain.ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
			if id != "" {
				_, err := s.GetSnapshot(ctx, id)
				issue("history root "+string(id), err)
			}
		}
		if e.MemoryHash != "" {
			_, err := s.GetMemory(ctx, e.MemoryHash)
			issue("history memory "+string(e.MemoryHash), err)
		}
	}
	for _, name := range []string{"working-commit.json"} {
		if _, err := readCxtFile(filepath.Join(s.storeDir(), name)); err == nil {
			r.Issues = append(r.Issues, "pending local transaction: "+name+" (normal mutation retries recovery)")
		} else if !os.IsNotExist(err) {
			issue(name, err)
		}
	}
	return r
}
