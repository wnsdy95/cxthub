package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func writeSelectedReplayJSON(t *testing.T, cwd, rel string, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func selectedReplayProof(repo, branchID string, index int, target domain.ContentHash) domain.HistoryEvent {
	return domain.HistoryEvent{ID: fmt.Sprintf("%032x", index), RepoID: repo, BranchID: branchID, Branch: "main", WorktreeID: fmt.Sprintf("%032x", index+100), Kind: "position", Source: target, Target: target, GitAfter: strings.Repeat("a", 40), MemoryPinned: true, CreatedAt: time.Unix(int64(index), 0).UTC()}
}

func writeSelectedReplayBatch(t *testing.T, cwd string, e domain.HistoryEvent) string {
	t.Helper()
	b := rewriteBatch{RepoID: e.RepoID, BranchID: e.BranchID, WorktreeID: e.WorktreeID, Rewrites: map[string]string{e.GitAfter: strings.Repeat("b", 40)}, Final: true}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(rewriteJournalDir(b.WorktreeID), fmt.Sprintf("%x.json", sha256.Sum256(raw)))
	writeSelectedReplayJSON(t, cwd, rel, b)
	return rel
}

func TestSelectedLocalReplayAcrossWorktrees(t *testing.T) {
	for _, kind := range []string{"publications", "rewrites"} {
		for _, mode := range []string{"selected", "empty", "nil-all"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				cwd, c, _, repo, target := publicationFixture(t)
				ctx := context.Background()
				position, err := c.History.CurrentPosition(ctx)
				if err != nil {
					t.Fatal(err)
				}
				ids := map[string]bool{"selected-R": true, "foreign-X": false}
				if mode == "empty" {
					ids = map[string]bool{}
				} else if mode == "nil-all" {
					ids = nil
				}
				var records []domain.HistoryEvent
				unchanged := map[string][]byte{}
				// Same canonical name, different immutable identity; caller is neither R nor X.
				for i, id := range []string{"selected-R", "selected-R", "foreign-X"} {
					e := selectedReplayProof(repo, id, i+1, target)
					records = append(records, e)
					if e.WorktreeID == position.WorktreeID || e.BranchID == position.BranchID {
						t.Fatal("fixture must exercise inactive identity/worktrees")
					}
					if err := c.History.RecordHistory(ctx, e); err != nil {
						t.Fatal(err)
					}
					if kind == "publications" {
						p := commitCapturePass{Version: 1, Initial: target, Proof: e, Outcomes: []commitCaptureOutcome{{Provider: domain.ProviderClaude, State: "absent"}}}
						p.Proof.Source, p.Proof.Target = "", "" // Interrupted, recoverable from exact local observation.
						if err := p.validate(); err != nil {
							t.Fatal(err)
						}
						unchanged[p.relativePath()] = writeSelectedReplayJSON(t, cwd, p.relativePath(), p)
					} else {
						if err := persistPublication(ctx, c, cwd, domain.CaptureAttempt{Proof: e}.Publication()); err != nil {
							t.Fatal(err)
						}
						rel := writeSelectedReplayBatch(t, cwd, e)
						unchanged[rel], err = os.ReadFile(filepath.Join(cwd, rel))
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if kind == "publications" {
					// Also cover journals with no surviving capture pass.
					for i, id := range []string{"selected-R", "foreign-X"} {
						e := selectedReplayProof(repo, id, i+4, target)
						e = domain.CaptureAttempt{Proof: e}.Publication()
						records = append(records, e)
						rel := filepath.Join(".cxt", "worktrees", e.WorktreeID, "publication-journal", e.ID+".json")
						unchanged[rel] = writeSelectedReplayJSON(t, cwd, rel, e)
					}
				}
				replay := replayPublicationsForBranches
				if kind == "rewrites" {
					replay = replayRewriteHistoryForBranches
				}
				for i := 0; i < 2; i++ {
					if err := replay(ctx, c, cwd, ids); err != nil {
						t.Fatal(err)
					}
				}
				events, err := c.History.ListHistory(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range records {
					want := mode == "nil-all" || mode == "selected" && e.BranchID == "selected-R"
					count := 0
					for _, got := range events {
						if got.Kind == "publish" && got.BranchID == e.BranchID && got.WorktreeID == e.WorktreeID && (kind == "publications" || got.GitAfter == strings.Repeat("b", 40)) {
							count++
						}
					}
					if want && count != 1 || !want && count != 0 {
						t.Errorf("%s worktree %s publication count=%d want selected=%v", e.BranchID, e.WorktreeID, count, want)
					}
					if !want {
						for rel, before := range unchanged {
							if strings.Contains(rel, e.WorktreeID) {
								after, err := os.ReadFile(filepath.Join(cwd, rel))
								if err != nil || string(after) != string(before) {
									t.Errorf("excluded journal changed: %s (%v)", rel, err)
								}
							}
						}
						for _, got := range events {
							if got.WorktreeID == e.WorktreeID && got.GitAfter == strings.Repeat("b", 40) {
								t.Errorf("excluded rewrite applied: %+v", got)
							}
						}
					}
				}
				after, err := c.History.CurrentPosition(ctx)
				if err != nil || !reflect.DeepEqual(position, after) {
					t.Fatalf("replay changed working selection: %+v %v", after, err)
				}
			})
		}
	}
}

type selectedReplayUnavailableHistory struct {
	inbound.ContextHistory
	foreign string
	reads   int
}

func (h *selectedReplayUnavailableHistory) ValidateHistorySource(ctx context.Context, e domain.HistoryEvent) (domain.HistoryEvent, error) {
	if e.BranchID == h.foreign {
		h.reads++
		return domain.HistoryEvent{}, errors.New("synthetic unavailable foreign source")
	}
	return h.ContextHistory.ValidateHistorySource(ctx, e)
}

func TestSelectedLocalReplaySkipsUnresolvedForeignEvidence(t *testing.T) {
	for _, kind := range []string{"capture", "publication", "rewrite"} {
		t.Run(kind, func(t *testing.T) {
			cwd, c, _, repo, target := publicationFixture(t)
			ctx := context.Background()
			e := selectedReplayProof(repo, "foreign-X", 1, target)
			var rel string
			switch kind {
			case "capture":
				p := commitCapturePass{Version: 1, Initial: target, Proof: e, Complete: true, Outcomes: []commitCaptureOutcome{{Provider: domain.ProviderClaude, State: "absent"}}}
				if err := p.validate(); err != nil {
					t.Fatal(err)
				}
				rel = p.relativePath()
				writeSelectedReplayJSON(t, cwd, rel, p)
			case "publication":
				e = domain.CaptureAttempt{Proof: e}.Publication()
				rel = filepath.Join(".cxt", "worktrees", e.WorktreeID, "publication-journal", e.ID+".json")
				writeSelectedReplayJSON(t, cwd, rel, e)
			case "rewrite":
				if err := c.History.RecordHistory(ctx, e); err != nil {
					t.Fatal(err)
				}
				rel = writeSelectedReplayBatch(t, cwd, e)
			}
			before, err := os.ReadFile(filepath.Join(cwd, rel))
			if err != nil {
				t.Fatal(err)
			}
			h := &selectedReplayUnavailableHistory{ContextHistory: c.History, foreign: e.BranchID}
			c.History = h
			if err := replayRewriteHistoryForBranches(ctx, c, cwd, map[string]bool{"selected-R": true}); err != nil {
				t.Fatalf("unrelated valid journal blocked selected replay: %v", err)
			}
			if h.reads != 0 {
				t.Fatal("excluded identity reached source validation")
			}
			after, err := os.ReadFile(filepath.Join(cwd, rel))
			if err != nil || string(after) != string(before) {
				t.Fatal("excluded journal changed")
			}
			// Selecting that identity, and legacy nil/all replay, still report the genuine failure.
			for _, ids := range []map[string]bool{{"foreign-X": true}, nil} {
				if err := replayRewriteHistoryForBranches(ctx, c, cwd, ids); err == nil {
					t.Fatal("selected source failure was suppressed")
				}
			}
		})
	}
}

type selectedReplayReadHistory struct {
	inbound.ContextHistory
	repo   string
	called bool
}

func (h *selectedReplayReadHistory) CurrentPosition(context.Context) (domain.WorkingPosition, error) {
	h.called = true
	return domain.WorkingPosition{RepoID: h.repo, WorktreeID: strings.Repeat("f", 32)}, nil
}
func (h *selectedReplayReadHistory) ListHistory(context.Context, string) ([]domain.HistoryEvent, error) {
	return nil, nil
}

func (h *selectedReplayReadHistory) ValidateHistorySource(_ context.Context, e domain.HistoryEvent) (domain.HistoryEvent, error) {
	return e, domain.ErrHashMismatch
}

func TestSelectedLocalReplayEmptyDoesNotRead(t *testing.T) {
	for _, replay := range []func(context.Context, *Container, string, map[string]bool) error{replayPublicationsForBranches, replayRewriteHistoryForBranches} {
		cwd := t.TempDir()
		h := &selectedReplayReadHistory{repo: string(domain.HashContent([]byte("repo")))}
		if err := replay(context.Background(), &Container{History: h}, cwd, map[string]bool{}); err != nil {
			t.Fatal(err)
		}
		if h.called {
			t.Fatal("empty selection read current position")
		}
		if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !os.IsNotExist(err) {
			t.Fatal("empty selection initialized replay directories")
		}
	}
}

func TestSelectedLocalReplayRejectsUnclassifiableEvidence(t *testing.T) {
	for _, kind := range []string{"capture", "publication", "rewrite"} {
		for _, damage := range []string{"json", "identity", "envelope", "symlink"} {
			t.Run(kind+"/"+damage, func(t *testing.T) {
				cwd := t.TempDir()
				repo := string(domain.HashContent([]byte("repo")))
				e := selectedReplayProof(repo, "foreign-X", 1, domain.HashContent([]byte("target")))
				var rel string
				var v any
				switch kind {
				case "capture":
					p := commitCapturePass{Version: 1, Initial: e.Target, Proof: e, Complete: true, Outcomes: []commitCaptureOutcome{{Provider: domain.ProviderClaude, State: "absent"}}}
					rel = p.relativePath()
					if damage == "identity" {
						p.Proof.BranchID = ""
					}
					if damage == "envelope" {
						p.Proof.RepoID = string(domain.HashContent([]byte("wrong repo")))
					}
					v = p
				case "publication":
					e = domain.CaptureAttempt{Proof: e}.Publication()
					if damage == "identity" {
						e.BranchID = ""
						e.ID = publicationID(e)
					}
					rel = filepath.Join(".cxt", "worktrees", e.WorktreeID, "publication-journal", e.ID+".json")
					if damage == "envelope" {
						e.WorktreeID = strings.Repeat("9", 32)
					}
					v = e
				case "rewrite":
					b := rewriteBatch{RepoID: repo, BranchID: e.BranchID, WorktreeID: e.WorktreeID, Rewrites: map[string]string{e.GitAfter: strings.Repeat("b", 40)}}
					if damage == "identity" {
						b.BranchID = ""
					}
					if damage == "envelope" {
						b.RepoID = string(domain.HashContent([]byte("wrong repo")))
					}
					raw, _ := json.Marshal(b)
					rel = filepath.Join(rewriteJournalDir(e.WorktreeID), fmt.Sprintf("%x.json", sha256.Sum256(raw)))
					v = b
				}
				writeSelectedReplayJSON(t, cwd, rel, v)
				path := filepath.Join(cwd, rel)
				if damage == "json" {
					if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if damage == "symlink" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(cwd, "missing-private-fixture"), path); err != nil {
						t.Fatal(err)
					}
				}
				h := &selectedReplayReadHistory{repo: repo}
				if err := replayRewriteHistoryForBranches(context.Background(), &Container{History: h}, cwd, map[string]bool{"selected-R": true}); err == nil {
					t.Fatal("invalid/unreadable excluded journal silently bypassed")
				}
			})
		}
	}
}
