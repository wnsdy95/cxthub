package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type manualRefQueries struct {
	inbound.LocalRefQueries
	refs func(context.Context) ([]domain.Ref, error)
}

func (q manualRefQueries) Refs(ctx context.Context, _ string) ([]domain.Ref, error) {
	return q.refs(ctx)
}

type manualPushSync struct {
	inbound.SyncRepo
	calls int
	push  func(inbound.SyncInput)
}

func (s *manualPushSync) Push(_ context.Context, in inbound.SyncInput) (inbound.SyncOutput, error) {
	s.calls++
	if s.push != nil {
		s.push(in)
	}
	return inbound.SyncOutput{}, nil
}

// Real FileStore/history and a separate canonical ref; Git HEAD remains main.
// Only the transport is replaced, so replay must durably finish before Push.
func manualPublicationFixture(t *testing.T, branch string) (string, *Container, *storage.FileStore, string, domain.ContentHash, string) {
	t.Helper()
	cwd, c, st, repo, target := publicationFixture(t)
	t.Chdir(cwd)
	id := domain.LegacyContextBranchID(repo, branch)
	if err := st.PutRef(context.Background(), domain.Ref{Kind: domain.RefBranch, Name: branch, BranchID: id, RepoID: repo, Target: target}); err != nil {
		t.Fatal(err)
	}
	c.ResolveRepo = func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: repo}, nil }
	c.Queries = manualRefQueries{refs: func(ctx context.Context) ([]domain.Ref, error) { return st.ListRefs(ctx, repo) }}
	c.WakeHistoricalSync = func(string) {}
	return cwd, c, st, repo, target, id
}

func manualCapture(t *testing.T, cwd, repo, id string, index int, target domain.ContentHash, complete bool) commitCapturePass {
	t.Helper()
	e := selectedReplayProof(repo, id, index, target)
	e.Branch = "feature"
	state := "absent"
	if !complete {
		state = "pending"
	}
	p := commitCapturePass{Version: 1, Initial: target, Proof: e, Complete: complete, Outcomes: []commitCaptureOutcome{{Provider: domain.ProviderClaude, State: state}}}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	writeSelectedReplayJSON(t, cwd, p.relativePath(), p)
	return p
}

func TestManualSelectedPushReplaysOnlyCanonicalIdentity(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonHEAD_crossWorktree", true: "Git_alias_does_not_retarget"}[alias], func(t *testing.T) {
			cwd, c, st, repo, target, id := manualPublicationFixture(t, "feature")
			ctx := context.Background()
			before, err := c.History.CurrentPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			foreignID := domain.LegacyContextBranchID(repo, "other")
			if alias {
				e := selectedReplayProof(repo, foreignID, 9, target)
				e.Kind, e.Branch, e.LocalBranch = "attach", "other", "feature"
				if err := st.BindLocalBranch(ctx, e); err != nil {
					t.Fatal(err)
				}
				b, err := c.History.ResolveLocalBranch(ctx, repo, "feature")
				if err != nil || b.BranchID != foreignID {
					t.Fatalf("alias fixture: %+v %v", b, err)
				}
			}
			var selected []commitCapturePass
			for i := 1; i <= 2; i++ {
				p := manualCapture(t, cwd, repo, id, i, target, true)
				if p.Proof.WorktreeID == before.WorktreeID || p.Proof.BranchID == before.BranchID {
					t.Fatal("not an inactive selected worktree")
				}
				selected = append(selected, p)
			}
			// Both durable capture and rewrite evidence must reach history before upload.
			writeSelectedReplayBatch(t, cwd, selected[0].Proof)
			foreign := manualCapture(t, cwd, repo, foreignID, 3, target, false)
			foreignBytes, err := os.ReadFile(filepath.Join(cwd, foreign.relativePath()))
			if err != nil {
				t.Fatal(err)
			}
			h := &selectedReplayUnavailableHistory{ContextHistory: c.History, foreign: foreignID}
			c.History = h
			syncer := &manualPushSync{push: func(in inbound.SyncInput) {
				events, err := c.History.ListHistory(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				for i, p := range selected {
					wantOID := p.Proof.GitAfter
					if i == 0 {
						wantOID = strings.Repeat("b", 40)
					}
					found := false
					for _, e := range events {
						if e.BranchID == foreignID {
							t.Error("foreign history applied")
						}
						if e.Kind == "publish" && e.BranchID == id && e.WorktreeID == p.Proof.WorktreeID && e.GitAfter == wantOID {
							found = true
						}
					}
					if !found {
						t.Errorf("Push called before selected worktree %d publication/rewrite completed", i)
					}
				}
				want := &domain.PublicationScope{Branches: []domain.PublicationBranch{{Branch: "feature", BranchID: id}}}
				if in.Ref != "" || !reflect.DeepEqual(in.Publication, want) || !in.Append || in.Force {
					t.Errorf("unfrozen or widened authority: %+v", in)
				}
			}}
			c.ResolveSyncDestination = func(context.Context, string, string) (SyncDestination, error) {
				return SyncDestination{Sync: syncer}, nil
			}
			if err := Run(c, []string{"cxt", "push", "origin", "feature", "--append"}); err != nil {
				t.Fatal(err)
			}
			if syncer.calls != 1 || h.reads != 0 {
				t.Fatalf("pushes=%d foreign validations=%d", syncer.calls, h.reads)
			}
			after, err := c.History.CurrentPosition(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("working position changed: %+v %v", after, err)
			}
			raw, err := os.ReadFile(filepath.Join(cwd, foreign.relativePath()))
			if err != nil || string(raw) != string(foreignBytes) {
				t.Fatal("foreign attempt changed")
			}
		})
	}
}

type manualRecordCallback struct {
	inbound.ContextHistory
	after func() error
}

func (h *manualRecordCallback) RecordHistory(ctx context.Context, e domain.HistoryEvent) error {
	if err := h.ContextHistory.RecordHistory(ctx, e); err != nil {
		return err
	}
	if h.after != nil {
		f := h.after
		h.after = nil
		return f()
	}
	return nil
}

func TestManualSelectedPushStopsBeforeTransport(t *testing.T) {
	for _, mode := range []string{"selected_incomplete", "corrupt_foreign", "binding_changed", "missing_ref", "missing_replay_locator", "invalid_ref"} {
		t.Run(mode, func(t *testing.T) {
			cwd, c, st, repo, target, id := manualPublicationFixture(t, "feature")
			ctx := context.Background()
			p := manualCapture(t, cwd, repo, id, 1, target, mode != "selected_incomplete")
			switch mode {
			case "corrupt_foreign":
				other := manualCapture(t, cwd, repo, "foreign-X", 2, target, false)
				if err := os.WriteFile(filepath.Join(cwd, other.relativePath()), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "binding_changed":
				c.History = &manualRecordCallback{ContextHistory: c.History, after: func() error {
					return st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "feature", BranchID: "replacement-identity", RepoID: repo, Target: target})
				}}
			case "missing_ref":
				c.Queries = manualRefQueries{refs: func(context.Context) ([]domain.Ref, error) { return nil, nil }}
			case "missing_replay_locator":
				c.History = &manualMissingPosition{ContextHistory: c.History}
			case "invalid_ref":
				c.Queries = manualRefQueries{refs: func(context.Context) ([]domain.Ref, error) {
					return []domain.Ref{{Kind: domain.RefBranch, Name: "feature", BranchID: id, RepoID: "wrong", Target: target}}, nil
				}}
			}
			syncer := &manualPushSync{}
			wakes := 0
			c.WakeHistoricalSync = func(string) { wakes++ }
			c.ResolveSyncDestination = func(context.Context, string, string) (SyncDestination, error) {
				return SyncDestination{Sync: syncer}, nil
			}
			err := Run(c, []string{"cxt", "push", "origin", "feature"})
			if err == nil || syncer.calls != 0 || wakes != 0 {
				t.Fatalf("unsafe selected push: err=%v transport calls=%d wakes=%d", err, syncer.calls, wakes)
			}
			if mode == "binding_changed" && !errors.Is(err, domain.ErrSelectionChanged) {
				t.Errorf("lost binding-change cause: %v", err)
			}
			if mode == "selected_incomplete" {
				// Original all-replay still warns and retains the unresolved attempt.
				if err := replayPublications(ctx, c, cwd); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(cwd, p.relativePath()))
				if err != nil || !strings.Contains(string(raw), `"complete":false`) {
					t.Fatal("pending capture was acknowledged")
				}
			}
		})
	}
}

type manualMissingPosition struct{ inbound.ContextHistory }

func (manualMissingPosition) CurrentPosition(context.Context) (domain.WorkingPosition, error) {
	return domain.WorkingPosition{}, domain.ErrNotFound
}
