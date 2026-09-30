package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestFSRefPreflightSymbolicHEADRechecksRecoveredBranch(t *testing.T) {
	for _, boundary := range []string{"storage", "service"} {
		t.Run(boundary, func(t *testing.T) {
			for _, operation := range []string{"archive", "rename"} {
				for _, prefix := range []string{"", "refs/heads/"} {
					t.Run(operation+"/"+prefix, func(t *testing.T) {
						ctx := context.Background()
						s, branch := contextWriteFS(t)
						repo := branch.RepoID
						if err := s.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: branch.Target, DocHash: branch.Target}); err != nil {
							t.Fatal(err)
						}
						birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: branch.BranchID, Branch: branch.Name, Kind: "birth", Source: branch.Target, Target: branch.Target, CreatedAt: time.Now().UTC()}
						if err := s.ApplyHistoryEvent(ctx, birth); err != nil {
							t.Fatal(err)
						}
						head := domain.Ref{RepoID: repo, Kind: domain.RefHead, Name: domain.HeadRefName, Symbolic: prefix + branch.Name}
						if err := s.CompareAndSwapRef(ctx, repo, head, ""); err != nil {
							t.Fatal(err)
						}
						beforeLog, err := s.ReadReflog(ctx, repo)
						if err != nil {
							t.Fatal(err)
						}
						event := birth
						event.ID, event.Kind, event.BindingParent = strings.Repeat("2", 32), operation, birth.ID
						event.CreatedAt = birth.CreatedAt.Add(time.Second)
						wantHEAD := []byte(string(branch.Target) + "\n")
						if operation == "rename" {
							event.PreviousBranch, event.Branch = branch.Name, "renamed"
							wantHEAD = []byte("ref: refs/heads/renamed\n")
						}
						raw, err := json.Marshal(event)
						if err != nil {
							t.Fatal(err)
						}
						// Persist the redo journal at the same interruption point as the
						// history journal contract: the event has not changed refs yet.
						if err := writeAtomic(s.historyJournal(repo), raw); err != nil {
							t.Fatal(err)
						}
						// Model the service's earlier symbolic target check. With the
						// nonbranch preflight, this read still sees the old live branch.
						observed, err := s.GetRef(ctx, repo, domain.RefBranch, branch.Name)
						if err != nil || observed.Target != branch.Target || observed.BranchID != branch.BranchID {
							t.Fatalf("pre-storage target observation: %+v %v", observed, err)
						}
						if pending, err := os.ReadFile(s.historyJournal(repo)); err != nil || !bytes.Equal(pending, raw) {
							t.Fatalf("journal recovered before CAS: %q %v", pending, err)
						}
						if boundary == "service" {
							// Exercise checkContextWrite, the service's symbolic target
							// observation, and storage recovery through the real API.
							_, err = app.NewService(s, s, nil, nil, nil).UpdateRef(inbound.WithSystemActor(ctx), inbound.UpdateRefInput{RepoID: repo, Ref: head})
						} else {
							err = s.CompareAndSwapRef(ctx, repo, head, "")
						}
						if !errors.Is(err, domain.ErrIntegrity) {
							t.Errorf("stale symbolic HEAD write must reject its recovered-away branch: %v", err)
						}
						gotHEAD, err := os.ReadFile(s.refFile(repo, domain.RefHead, domain.HeadRefName))
						if err != nil || !bytes.Equal(gotHEAD, wantHEAD) {
							t.Errorf("recovered HEAD overwritten: got %q, want %q: %v", gotHEAD, wantHEAD, err)
						}
						if _, err := os.Stat(s.historyJournal(repo)); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("CAS did not complete journal recovery: %v", err)
						}
						if persisted, err := os.ReadFile(s.historyPath(repo, event.ID)); err != nil || !bytes.Equal(persisted, raw) {
							t.Errorf("recovered event not retained: %q %v", persisted, err)
						}
						if _, err := s.GetRef(ctx, repo, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
							t.Errorf("old branch remains after recovery: %v", err)
						}
						if operation == "rename" {
							renamed, err := s.GetRef(ctx, repo, domain.RefBranch, event.Branch)
							if err != nil || renamed.Target != branch.Target || renamed.BranchID != branch.BranchID {
								t.Errorf("renamed branch not preserved: %+v %v", renamed, err)
							}
						}
						root, err := s.GetRef(ctx, repo, domain.RefTag, "cxt/history/v1/"+event.ID+"/source")
						if err != nil || root.Target != branch.Target {
							t.Errorf("recovery lost retained source: %+v %v", root, err)
						}
						afterLog, err := s.ReadReflog(ctx, repo)
						if err != nil || !reflect.DeepEqual(afterLog, beforeLog) {
							t.Errorf("rejected HEAD write changed reflog: before=%+v after=%+v err=%v", beforeLog, afterLog, err)
						}
					})
				}
			}
		})
	}
}

func TestFSRefPreflightCASPreservesUnreadableNonbranch(t *testing.T) {
	for _, kind := range []domain.RefKind{domain.RefTag, domain.RefSession, domain.RefHead} {
		for _, corruption := range []string{"malformed bytes", "directory"} {
			t.Run(string(kind)+"/"+corruption, func(t *testing.T) {
				ctx := context.Background()
				s, next := contextWriteFS(t)
				next.Kind, next.BranchID = kind, ""
				if kind == domain.RefHead {
					next.Name = domain.HeadRefName
				}
				if err := s.PutSnapshot(ctx, domain.Snapshot{RepoID: next.RepoID, ID: next.Target, DocHash: next.Target}); err != nil {
					t.Fatal(err)
				}
				path := s.refFile(next.RepoID, next.Kind, next.Name)
				retainedPath := path
				original := []byte("sha256:malformed-ref\n")
				if corruption == "directory" {
					if err := os.MkdirAll(path, 0700); err != nil {
						t.Fatal(err)
					}
					retainedPath = filepath.Join(path, "retained")
				}
				if err := writeAtomic(retainedPath, original); err != nil {
					t.Fatal(err)
				}
				_, readErr := s.GetRef(ctx, next.RepoID, next.Kind, next.Name)
				if readErr == nil || errors.Is(readErr, domain.ErrNotFound) {
					t.Fatalf("fixture must fail an existing ref read: %v", readErr)
				}
				beforeLog, err := s.ReadReflog(ctx, next.RepoID)
				if err != nil {
					t.Fatal(err)
				}
				// An unreadable existing ref is not evidence of absence, even
				// when the caller explicitly supplies an empty CAS expectation.
				// HEAD ignores expected-target CAS by contract; its read error
				// must still prevent overwriting an unreadable existing HEAD.
				err = s.CompareAndSwapRef(ctx, next.RepoID, next, "")
				if corruption == "malformed bytes" {
					if !errors.Is(readErr, domain.ErrIntegrity) || !errors.Is(err, domain.ErrIntegrity) {
						t.Errorf("malformed ref error lost: read=%v CAS=%v", readErr, err)
					}
				} else {
					var before, after *os.PathError
					if !errors.As(readErr, &before) || !errors.As(err, &after) || after.Op != before.Op || after.Path != before.Path || !errors.Is(after, before.Err) {
						t.Errorf("CAS must propagate the read error before attempting a write: read=%v CAS=%v", readErr, err)
					}
					if info, err := os.Stat(path); err != nil || !info.IsDir() {
						t.Errorf("unreadable ref directory replaced: %v", err)
					}
				}
				retained, err := os.ReadFile(retainedPath)
				if err != nil || !bytes.Equal(retained, original) {
					t.Errorf("existing ref bytes changed: got %q want %q err=%v", retained, original, err)
				}
				if got, err := s.GetRef(ctx, next.RepoID, next.Kind, next.Name); err == nil {
					t.Errorf("failed CAS published a valid replacement ref: %+v", got)
				}
				afterLog, err := s.ReadReflog(ctx, next.RepoID)
				if err != nil || !reflect.DeepEqual(afterLog, beforeLog) {
					t.Errorf("failed CAS appended a success reflog: before=%+v after=%+v err=%v", beforeLog, afterLog, err)
				}
			})
		}
	}
}
