package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// LoadAgentContext prepares a new session. It never edits AGENTS.md/CLAUDE.md,
// falls back to raw history, or claims that a materialized file was accepted by
// a running provider. Native resume must bypass this method entirely.
func (s *LoadSessionService) LoadAgentContext(ctx context.Context, in inbound.PrepareAgentContextInput) (inbound.LoadOutput, error) {
	_, out, err := s.PrepareAgentDelivery(ctx, in)
	return out, err
}

// PrepareAgentDelivery prepares exactly once. The returned receipt describes
// the same bytes passed to the materializer, even if another generation arrives
// while delivery is in progress. It does not mark provider acceptance.
func (s *LoadSessionService) PrepareAgentDelivery(ctx context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, inbound.LoadOutput, error) {
	if in.ArtifactOnly {
		return domain.AgentContextPackage{}, inbound.LoadOutput{}, fmt.Errorf("%w: artifact-only context cannot be launched", domain.ErrProviderCapabilityUnknown)
	}
	if s.agentContext == nil {
		return domain.AgentContextPackage{}, inbound.LoadOutput{}, domain.ErrAgentContextUnavailable
	}
	p, err := s.agentContext.PrepareAgentContext(ctx, in)
	if err != nil {
		return p, inbound.LoadOutput{}, err
	}
	out, err := s.materializeAgentPackage(ctx, in, p)
	return p, out, err
}

func (s *LoadSessionService) materializeAgentPackage(ctx context.Context, in inbound.PrepareAgentContextInput, p domain.AgentContextPackage) (inbound.LoadOutput, error) {
	if p.ArtifactOnly {
		return inbound.LoadOutput{}, domain.ErrProviderCapabilityUnknown
	}
	if err := p.ValidateIdentity(); err != nil {
		return inbound.LoadOutput{}, err
	}
	if in.Policy.Mode == "history" || p.Policy.Mode == "history" {
		if p.Policy != in.Policy || p.Budget == nil || p.Capability != "verified_for_preparation" {
			return inbound.LoadOutput{}, fmt.Errorf("%w: history delivery requires the original request and verified budget", domain.ErrProviderCapabilityUnknown)
		}
		model := in.Model
		if model == "" {
			model = p.Budget.Model
		}
		if err := p.Budget.Validate(in.Provider, model, in.Policy.BudgetTokens, p.Usage); err != nil {
			return inbound.LoadOutput{}, err
		}
	}
	cir, err := agentPackageCIR(p, in.Provider, in.Cwd)
	if err != nil {
		return inbound.LoadOutput{}, err
	}
	codec, ok := s.codecs[in.Provider]
	mat, materializable := s.materializers[in.Provider]
	if !ok || !materializable {
		return inbound.LoadOutput{}, domain.ErrUnsupportedProvider
	}
	raw, err := codec.Encode(ctx, cir, in.Provider)
	if err != nil {
		return inbound.LoadOutput{}, fmt.Errorf("%w: encode prepared context: %w", domain.ErrDeliveryFailed, err)
	}
	if err = ctx.Err(); err != nil {
		return inbound.LoadOutput{}, err
	}
	if err := checkAgentCode(ctx, s.agentCode, in.Cwd, p.Content.Selection.CodeCommit); err != nil {
		return inbound.LoadOutput{}, err
	}
	path, resume, err := mat.Materialize(ctx, raw, in.Cwd)
	if err != nil {
		return inbound.LoadOutput{}, fmt.Errorf("%w: materialize prepared context: %w", domain.ErrDeliveryFailed, err)
	}
	if path == "" || resume == "" {
		return inbound.LoadOutput{}, fmt.Errorf("%w: materializer returned no resumable session", domain.ErrDeliveryFailed)
	}
	fidelity := domain.FidelityMemory
	if p.Policy.Mode == "history" {
		fidelity = domain.FidelityReconstructed
	}
	return inbound.LoadOutput{WrittenPath: path, ResumeCmd: resume, Fidelity: fidelity}, nil
}

func agentPackageCIR(p domain.AgentContextPackage, provider domain.ProviderKind, cwd string) (domain.CIRDocument, error) {
	prompt, err := p.Prompt()
	if err != nil {
		return domain.CIRDocument{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	fidelity := domain.FidelityMemory
	if p.Policy.Mode == "history" {
		fidelity = domain.FidelityReconstructed
	}
	return domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: provider, CapturedAt: now, Cwd: materializationCwd(cwd), GitBranch: p.Content.Selection.Branch, SessionOriginID: domain.NewSessionID(), Fidelity: fidelity}, Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0, Ts: now, Blocks: []domain.ContentBlock{{Type: "text", Text: prompt}}}}}, nil
}

func (s *BranchSeedService) seedAgentContext(ctx context.Context, in inbound.SeedInput, repo domain.Repo, source domain.Snapshot, sourceDoc domain.SessionDoc, provider domain.ProviderKind, cwd string) (inbound.SeedOutput, error) {
	var state outbound.CheckoutState
	tx, transactional := s.store.(outbound.CheckoutTransactionStore)
	if transactional {
		var err error
		state, err = tx.ReadCheckoutState(ctx, repo.ID)
		if err != nil {
			return inbound.SeedOutput{}, err
		}
	}
	p, err := s.agentContext.PrepareAgentContext(ctx, inbound.PrepareAgentContextInput{RepoID: repo.ID, Cwd: cwd, Branch: in.FromBranch, SnapshotID: source.ID, Provider: provider, Policy: domain.MemoryInputPolicy()})
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	if err = p.ValidateIdentity(); err != nil {
		return inbound.SeedOutput{}, err
	}
	selection := p.Content.Selection
	if selection.RepositoryID != repo.ID || selection.SnapshotID != source.ID || selection.Branch != in.FromBranch {
		return inbound.SeedOutput{}, domain.ErrSelectionChanged
	}
	cir, err := agentPackageCIR(p, provider, cwd)
	if err != nil {
		return inbound.SeedOutput{}, err
	}
	cir.Envelope.GitBranch = in.NewBranch
	out := inbound.SeedOutput{SessionID: cir.Envelope.SessionOriginID}
	// Preserve full inherited memory independently of the small provider prompt.
	// Raw history and opaque provider state remain reachable through source.ID.
	digest, found, err := readAgentSeedMemory(ctx, s.store, source, selection.MemoryPin)
	if err != nil {
		return out, err
	}
	if !found {
		if s.distiller == nil {
			return out, domain.ErrAgentContextUnavailable
		}
		digest, err = s.distiller.Distill(ctx, sourceDoc.CIR.EffectiveContext(), nil)
		if err != nil {
			return out, err
		}
		digest.SnapshotID = source.ID
		digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	}
	// Prepare first. An unsupported/failed provider cannot leave an active branch
	// pointer pretending that its seed was delivered. Desktop handoff skips native
	// materialization and retains the vendor-owned active conversation.
	if !in.SkipMaterialize {
		codec, ok := s.codecs[provider]
		mat, matOK := s.materializers[provider]
		if !ok || !matOK {
			return out, domain.ErrUnsupportedProvider
		}
		raw, err := codec.Encode(ctx, cir, provider)
		if err != nil {
			return out, fmt.Errorf("%w: encode prepared seed: %w", domain.ErrDeliveryFailed, err)
		}
		if err = ctx.Err(); err != nil {
			return out, err
		}
		code, _ := s.gitCtx.(outbound.CodePosition)
		if err = checkAgentCode(ctx, code, cwd, p.Content.Selection.CodeCommit); err != nil {
			return out, err
		}
		out.WrittenPath, out.ResumeCmd, err = mat.Materialize(ctx, raw, cwd)
		if err != nil {
			return out, fmt.Errorf("%w: materialize prepared seed: %w", domain.ErrDeliveryFailed, err)
		}
		if out.WrittenPath == "" || out.ResumeCmd == "" {
			return out, fmt.Errorf("%w: materializer returned no resumable session", domain.ErrDeliveryFailed)
		}
		fields := strings.Fields(out.ResumeCmd)
		if len(fields) > 0 && domain.ValidSessionID(fields[len(fields)-1]) {
			out.SessionID = fields[len(fields)-1]
		}
	}
	docHash, err := s.store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		return out, err
	}
	digest.SnapshotID = docHash
	digest.PreviousMemoryHash = ""
	memoryHash, err := s.store.PutMemory(ctx, digest)
	if err != nil {
		return out, err
	}
	snap := domain.Snapshot{ID: docHash, RepoID: repo.ID, Branch: in.NewBranch, Parents: []domain.ContentHash{source.ID}, DocHash: docHash, MemoryHash: memoryHash, Provider: provider, Fidelity: domain.FidelityMemory, Message: fmt.Sprintf("seed: %s → %s", in.FromBranch, in.NewBranch), Author: in.Author, CreatedAt: time.Now().UTC(), SessionID: cir.Envelope.SessionOriginID}
	if err = s.store.PutSnapshot(ctx, snap); err != nil {
		return out, err
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: in.NewBranch, RepoID: repo.ID, Target: docHash}
	head := domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: in.NewBranch}
	if code, ok := s.gitCtx.(outbound.CodePosition); ok {
		current, err := code.CurrentCommit(ctx, cwd)
		if err != nil {
			return out, err
		}
		if current != p.Content.Selection.CodeCommit {
			return out, domain.ErrCodePositionMismatch
		}
	}
	if transactional {
		if err = tx.CommitCheckout(ctx, outbound.CheckoutTransition{RepoID: repo.ID, Expected: state, ExpectedMemoryHash: memoryHash, Head: head, Branch: &branch, CreateBranch: true}); err != nil {
			return out, err
		}
	} else {
		if _, err = s.store.CreateBranchRef(ctx, branch); err != nil {
			return out, err
		}
		if err = s.store.PutRef(ctx, head); err != nil {
			return out, err
		}
	}
	out.SnapshotID = docHash

	return out, nil
}

// A prepared historical pin is authoritative even if the live cursor or the
// source's mutable attachment has since moved. An explicit empty pin is found
// memory, so it must never trigger fresh distillation of the source conversation.
func readAgentSeedMemory(ctx context.Context, store MemoryReader, source domain.Snapshot, pin *domain.AgentMemoryPin) (domain.MemoryDigest, bool, error) {
	if err := pin.Validate(); err != nil {
		return domain.MemoryDigest{}, false, err
	}
	if pin == nil {
		return ReadProjectedMemory(ctx, store, source.ID)
	}
	if pin.MemoryHash == "" {
		return domain.MemoryDigest{}, true, nil
	}
	owner, err := store.GetSnapshot(ctx, pin.SnapshotID)
	if err != nil {
		return domain.MemoryDigest{}, false, err
	}
	if owner.ID != pin.SnapshotID || owner.RepoID != source.RepoID {
		return domain.MemoryDigest{}, false, domain.ErrHashMismatch
	}
	digest, err := store.GetMemory(ctx, pin.MemoryHash)
	if err != nil {
		return domain.MemoryDigest{}, false, err
	}
	actual, err := domain.MemoryDigestHash(digest)
	if err != nil {
		return domain.MemoryDigest{}, false, err
	}
	if digest.SnapshotID != pin.SnapshotID || actual != pin.MemoryHash {
		return domain.MemoryDigest{}, false, domain.ErrHashMismatch
	}
	return digest, true, nil
}

// External Git operations are not locked by CXT. This last check bounds the
// publication race without claiming a cross-process Git/provider transaction.
func checkAgentCode(ctx context.Context, code outbound.CodePosition, cwd, expected string) error {
	if code == nil {
		return fmt.Errorf("%w: actual Git code reader required before delivery", domain.ErrAgentContextUnavailable)
	}
	actual, err := code.CurrentCommit(ctx, cwd)
	if err != nil {
		return err
	}
	if !domain.ValidGitOID(actual) || actual != expected {
		return domain.ErrCodePositionMismatch
	}
	return nil
}
