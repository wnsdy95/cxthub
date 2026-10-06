package app

import (
	"context"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// StashService implements the StashSession inbound port (similar to git stash).
//
// Stash sequence (corresponding to git stash push):
//  1. Capture active session → CIR → doc/snapshot storage (branch label = StashBranchLabel —
//     a local-only object excluded from branch history and push targets)
//  2. Push to stack (.cxt/stash.json)
//  3. Restore current branch head (commit and context chain) as active session
//     — same as git reverting the working tree to HEAD
//
// StashPop sequence (corresponding to git stash pop):
//  1. Prepare the latest original session without removing its entry.
//  2. Remove the entry only if the observed stack is still unchanged.
type StashService struct {
	capture  outbound.SessionCapture
	gitCtx   outbound.GitContext
	captures map[domain.ProviderKind]outbound.CaptureSource
	codecs   map[domain.ProviderKind]outbound.ProviderCodec
	store    outbound.SessionStore
	load     inbound.LoadSession
}

// NewStashService creates a StashService and injects dependencies.
func NewStashService(
	gitCtx outbound.GitContext,
	captures map[domain.ProviderKind]outbound.CaptureSource,
	codecs map[domain.ProviderKind]outbound.ProviderCodec,
	store outbound.SessionStore,
	load inbound.LoadSession,
	capture outbound.SessionCapture,
) *StashService {
	return &StashService{gitCtx: gitCtx, captures: captures, codecs: codecs, store: store, load: load, capture: capture}
}

// Stash saves the active session to the stack and restores the branch head context.
func (s *StashService) Stash(ctx context.Context, in inbound.StashInput) (inbound.StashOutput, error) {
	var out inbound.StashOutput
	var restoreHead bool
	capture := func(locked context.Context) error {
		var err error
		out, restoreHead, err = s.captureStash(locked, in)
		return err
	}
	var err error
	if gate, ok := s.store.(outbound.CaptureTrackingGate); ok {
		err = gate.WithCaptureTrackingGate(ctx, capture)
	} else {
		err = capture(ctx)
	}
	if err != nil {
		return out, err
	}
	// Capture and stack publication are durable. Release admission exclusion
	// before restoration, which can query server history and materialize a session.
	if restoreHead {
		if lo, lerr := s.load.Load(ctx, inbound.LoadInput{Ref: out.Branch, Cwd: in.Cwd}); lerr == nil {
			out.RestoredHead = true
			out.ResumeCmd = lo.ResumeCmd
		}
	}
	return out, nil
}

func (s *StashService) captureStash(ctx context.Context, in inbound.StashInput) (inbound.StashOutput, bool, error) {
	provider := in.Provider
	if provider == "" {
		provider = domain.ProviderClaude
	}
	capt, ok := s.captures[provider]
	if !ok {
		return inbound.StashOutput{}, false, domain.ErrUnsupportedProvider
	}
	cdc, ok := s.codecs[provider]
	if !ok {
		return inbound.StashOutput{}, false, domain.ErrUnsupportedProvider
	}

	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.StashOutput{}, false, err
	}
	branch, _ := s.gitCtx.CurrentBranch(ctx, in.Cwd)
	if branch == "" {
		branch = repo.DefaultBranch
	}

	// 1) Capture active session ("working tree") — if none, like git, "no changes to save".
	path := in.SessionPath
	if path == "" {
		path, err = capt.LocateActiveSession(ctx, in.Cwd)
		if err != nil {
			return inbound.StashOutput{}, false, err // ErrNoActiveSession included
		}
	} else {
		if !s.capture.Eligible(repo.LocalPath, path) {
			return inbound.StashOutput{}, false, domain.ErrNoActiveSession
		}
	}
	envelope, docHash, _, _, err := s.capture.Project(ctx, repo.LocalPath, path, capt, cdc, false)
	if err != nil {
		return inbound.StashOutput{}, false, err
	}

	msg := in.Message
	if msg == "" {
		msg = fmt.Sprintf("WIP on %s", branch)
	}
	// Parent = current branch head (if any) — maintain chain of stashes (like git).
	var parents []domain.ContentHash
	headTarget := domain.ContentHash("")
	if ref, gerr := s.store.GetRef(ctx, string(repo.ID), domain.RefBranch, branch); gerr == nil && ref.Target != "" {
		headTarget = ref.Target
		if ref.Target != docHash {
			parents = []domain.ContentHash{ref.Target}
		}
	}
	snap := domain.Snapshot{
		ID:        docHash,
		RepoID:    string(repo.ID),
		Branch:    domain.StashBranchLabel, // branch history/push excluded
		Parents:   parents,
		DocHash:   docHash,
		Provider:  provider,
		Fidelity:  envelope.Fidelity,
		Message:   msg,
		Author:    in.Author,
		CreatedAt: time.Now().UTC(),
		SessionID: envelope.SessionOriginID,
		Models:    envelope.OrderedModels(),
	}
	if err := s.store.PutSnapshot(ctx, snap); err != nil {
		return inbound.StashOutput{}, false, err
	}

	// 2) stack push.
	if err := s.store.StashPush(ctx, string(repo.ID), domain.StashEntry{
		Snapshot:  docHash,
		Branch:    branch,
		Message:   msg,
		Provider:  provider,
		CreatedAt: snap.CreatedAt,
	}); err != nil {
		return inbound.StashOutput{}, false, err
	}
	stack, _ := s.store.StashList(ctx, string(repo.ID))

	out := inbound.StashOutput{StashID: docHash, Branch: branch, Depth: len(stack)}

	// Preserve the restore decision made against the captured branch baseline.
	return out, headTarget != "" && headTarget != docHash, nil
}

// StashPop restores the latest stash to the active session and removes it from the stack.
func (s *StashService) StashPop(ctx context.Context, cwd string) (inbound.StashPopOutput, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return inbound.StashPopOutput{}, err
	}
	ack, ok := s.store.(outbound.StashRestoreStore)
	if !ok {
		return inbound.StashPopOutput{}, fmt.Errorf("stash restore requires compare-and-drop storage")
	}
	stack, err := s.store.StashList(ctx, string(repo.ID))
	if err != nil {
		return inbound.StashPopOutput{}, err // ErrNotFound = stack empty
	}
	if len(stack) == 0 {
		return inbound.StashPopOutput{}, domain.ErrNotFound
	}
	entry := stack[0]
	// Session stash is an explicit original-session restore, including local
	// unpublished work. It must not become a cloud-only new-session selection.
	lo, err := s.load.Load(ctx, inbound.LoadInput{RepoID: repo.ID, Ref: string(entry.Snapshot), Cwd: cwd, TargetProvider: entry.Provider, Mode: domain.FidelityFull, RequireConversation: true})
	if err != nil {
		return inbound.StashPopOutput{}, err
	}
	if lo.ResumeCmd == "" || (lo.Fidelity != domain.FidelityFull && lo.Fidelity != domain.FidelityReconstructed) {
		return inbound.StashPopOutput{}, fmt.Errorf("session restore did not produce a resumable conversation; stash was retained")
	}
	if err := ack.CompareAndDropStash(ctx, repo.ID, stack); err != nil {
		return inbound.StashPopOutput{}, fmt.Errorf("session prepared but stash changed; no stash was removed: %w", err)
	}
	return inbound.StashPopOutput{Entry: entry, Fidelity: lo.Fidelity, ResumeCmd: lo.ResumeCmd, Depth: len(stack) - 1}, nil
}

// StashList returns the stack in latest order.
func (s *StashService) StashList(ctx context.Context, cwd string) ([]domain.StashEntry, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	return s.store.StashList(ctx, string(repo.ID))
}

// Ensure StashService implements inbound.StashSession.
var _ inbound.StashSession = (*StashService)(nil)
