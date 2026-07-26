package app

import (
	"context"

	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
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
//  1. Remove latest stack item → 2. Restore that snapshot as active session
type StashService struct {
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
) *StashService {
	return &StashService{gitCtx: gitCtx, captures: captures, codecs: codecs, store: store, load: load}
}

// Stash saves the active session to the stack and restores the branch head context.
func (s *StashService) Stash(ctx context.Context, in inbound.StashInput) (inbound.StashOutput, error) {
	provider := in.Provider
	if provider == "" {
		provider = domain.ProviderClaude
	}
	capt, ok := s.captures[provider]
	if !ok {
		return inbound.StashOutput{}, domain.ErrUnsupportedProvider
	}
	cdc, ok := s.codecs[provider]
	if !ok {
		return inbound.StashOutput{}, domain.ErrUnsupportedProvider
	}

	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.StashOutput{}, err
	}
	branch, _ := s.gitCtx.CurrentBranch(ctx, in.Cwd)
	if branch == "" {
		branch = repo.DefaultBranch
	}

	// 1) Capture active session ("working tree") — if none, like git, "no changes to save".
	path, err := capt.LocateActiveSession(ctx, in.Cwd)
	if err != nil {
		return inbound.StashOutput{}, err // ErrNoActiveSession included
	}
	raw, err := capt.ReadSession(ctx, path)
	if err != nil {
		return inbound.StashOutput{}, err
	}
	raw, _ = capture.ScrubSecrets(raw, repo.LocalPath) // .cxtsecrets masking (before saving)
	cir, err := cdc.Decode(ctx, raw)
	if err != nil {
		return inbound.StashOutput{}, err
	}
	cir = capture.ScrubDoc(cir, repo.LocalPath) // pattern scrub (same layer as save)
	docHash, err := s.store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		return inbound.StashOutput{}, err
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
		Fidelity:  cir.Envelope.Fidelity,
		Message:   msg,
		Author:    in.Author,
		CreatedAt: time.Now().UTC(),
		SessionID: cir.Envelope.SessionOriginID,
		Models:    cir.Envelope.OrderedModels(),
	}
	if err := s.store.PutSnapshot(ctx, snap); err != nil {
		return inbound.StashOutput{}, err
	}

	// 2) stack push.
	if err := s.store.StashPush(ctx, string(repo.ID), domain.StashEntry{
		Snapshot:  docHash,
		Branch:    branch,
		Message:   msg,
		Provider:  provider,
		CreatedAt: snap.CreatedAt,
	}); err != nil {
		return inbound.StashOutput{}, err
	}
	stack, _ := s.store.StashList(ctx, string(repo.ID))

	out := inbound.StashOutput{StashID: docHash, Branch: branch, Depth: len(stack)}

	// 3) branch head context recovery — skip if head matches stash content or is empty.
	if headTarget != "" && headTarget != docHash {
		if lo, lerr := s.load.Load(ctx, inbound.LoadInput{Ref: branch, Cwd: in.Cwd}); lerr == nil {
			out.RestoredHead = true
			out.ResumeCmd = lo.ResumeCmd
		}
	}
	return out, nil
}

// StashPop restores the latest stash to the active session and removes it from the stack.
func (s *StashService) StashPop(ctx context.Context, cwd string) (inbound.StashPopOutput, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return inbound.StashPopOutput{}, err
	}
	entry, err := s.store.StashPop(ctx, string(repo.ID))
	if err != nil {
		return inbound.StashPopOutput{}, err // ErrNotFound = stack empty
	}
	lo, err := s.load.Load(ctx, inbound.LoadInput{Ref: string(entry.Snapshot), Cwd: cwd})
	if err != nil {
		// On recovery failure, restore the stack (undo pop) — git maintains stash on conflict.
		_ = s.store.StashPush(ctx, string(repo.ID), entry)
		return inbound.StashPopOutput{}, err
	}
	stack, _ := s.store.StashList(ctx, string(repo.ID))
	return inbound.StashPopOutput{Entry: entry, Fidelity: lo.Fidelity, ResumeCmd: lo.ResumeCmd, Depth: len(stack)}, nil
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
