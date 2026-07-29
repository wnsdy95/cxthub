package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/session"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const claudeFixtureWithSig = `{"type":"user","cwd":"/p","sessionId":"s","gitBranch":"main","timestamp":"2026-06-30T00:00:00Z","message":{"role":"user","content":"do it"}}
{"type":"assistant","cwd":"/p","sessionId":"s","gitBranch":"main","timestamp":"2026-06-30T00:00:01Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"planning","signature":"SIG1"},{"type":"text","text":"working"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`

// seedClaudeSnapshot decodes the claude session and stores the snapshot + main branch ref.
func seedClaudeSnapshot(t *testing.T, store *storage.FileStore) domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	cir, err := codec.NewClaudeCodec().Decode(ctx, []byte(claudeFixtureWithSig))
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	h, err := store.PutDoc(ctx, domain.SessionDoc{CIR: cir})
	if err != nil {
		t.Fatalf("PutDoc: %v", err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{ID: h, Branch: "main", DocHash: h, Provider: domain.ProviderClaude, Fidelity: cir.Envelope.Fidelity}); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	store.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: h})
	store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "main"})
	return h
}

func newLoadSvc(store *storage.FileStore) *LoadSessionService {
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{
		domain.ProviderClaude: codec.NewClaudeCodec(),
		domain.ProviderCodex:  codec.NewCodexCodec(),
	}
	mats := map[domain.ProviderKind]outbound.SessionMaterializer{
		domain.ProviderClaude: session.NewClaudeMaterializer(),
		domain.ProviderCodex:  session.NewCodexMaterializer(),
	}
	srcs := map[domain.ProviderKind]outbound.MemorySource{
		domain.ProviderClaude: memory.NewClaudeMemorySource(),
		domain.ProviderCodex:  memory.NewCodexMemorySource(),
	}
	sinks := map[domain.ProviderKind]outbound.MemorySink{
		domain.ProviderClaude: memory.NewClaudeMemorySink(),
		domain.ProviderCodex:  memory.NewCodexMemorySink(),
	}
	return NewLoadSessionService(store, codecs, mats, srcs, memory.NewRuleDistiller(), sinks)
}

// Same provider full load: claude → claude, signature preserved.
func TestLoadFullSameProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityFull, Cwd: cwd})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Fidelity != domain.FidelityFull {
		t.Fatalf("expected full, got %s", out.Fidelity)
	}
	if !strings.HasPrefix(out.ResumeCmd, "claude --resume ") {
		t.Fatalf("resume cmd: %q", out.ResumeCmd)
	}
	data, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatalf("written file: %v", err)
	}
	if !strings.Contains(string(data), "SIG1") {
		t.Fatalf("full same-provider must preserve signature")
	}
	restored, err := codec.NewClaudeCodec().Decode(ctx, data)
	if err != nil {
		t.Fatalf("decode restored Claude session: %v", err)
	}
	if restored.Envelope.Cwd != cwd {
		t.Fatalf("restored Claude session cwd = %q, want target %q", restored.Envelope.Cwd, cwd)
	}
}

// Cross-provider full request: claude → codex, reconstructed fallback + claude signature masked.
func TestLoadCrossProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Fidelity != domain.FidelityReconstructed {
		t.Fatalf("cross-provider must downgrade to reconstructed, got %s", out.Fidelity)
	}
	if !strings.HasPrefix(out.ResumeCmd, "codex resume ") {
		t.Fatalf("resume cmd: %q", out.ResumeCmd)
	}
	data, err := os.ReadFile(out.WrittenPath)
	if err != nil {
		t.Fatalf("written file: %v", err)
	}
	if strings.Contains(string(data), "SIG1") {
		t.Fatalf("cross-encode leaked claude signature into codex output:\n%s", data)
	}
	if !strings.Contains(string(data), "session_meta") {
		t.Fatalf("codex output should be a rollout (session_meta)")
	}
	restored, err := codec.NewCodexCodec().Decode(ctx, data)
	if err != nil {
		t.Fatalf("decode restored Codex session: %v", err)
	}
	if restored.Envelope.Cwd != cwd {
		t.Fatalf("restored Codex session cwd = %q, want target %q", restored.Envelope.Cwd, cwd)
	}
}

// Memory mode: inject digest into CLAUDE.md.
func TestLoadMemoryMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	out, err := newLoadSvc(store).Load(ctx, inbound.LoadInput{Ref: "main", TargetProvider: domain.ProviderClaude, Mode: domain.FidelityMemory, Cwd: cwd})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Fidelity != domain.FidelityMemory {
		t.Fatalf("expected memory, got %s", out.Fidelity)
	}
	if !strings.HasSuffix(out.WrittenPath, "CLAUDE.md") {
		t.Fatalf("memory sink should write CLAUDE.md, got %s", out.WrittenPath)
	}
	data, _ := os.ReadFile(out.WrittenPath)
	if !strings.Contains(string(data), "cxt memory") {
		t.Fatalf("CLAUDE.md should contain injected memory")
	}
}

// checkout -b: branch creation (new branch ref) + cross-provider restoration.
func TestCheckoutForkAndLoad(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	head := seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	load := newLoadSvc(store)
	checkout := NewCheckoutSessionService(NewForkSessionService(store), load, store)

	co, err := checkout.Checkout(ctx, inbound.CheckoutInput{From: "main", NewBranch: "feat/x", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd})
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if co.Branch != "feat/x" || co.Head != head {
		t.Fatalf("checkout output: %+v (head want %s)", co, head)
	}
	if co.Fidelity != domain.FidelityReconstructed {
		t.Fatalf("cross checkout should be reconstructed, got %s", co.Fidelity)
	}
	// New branch ref must point to the head.
	ref, err := store.GetRef(ctx, "", domain.RefBranch, "feat/x")
	if err != nil || ref.Target != head {
		t.Fatalf("forked branch ref: %v %+v", err, ref)
	}
	symbolic, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || symbolic.Symbolic != "feat/x" {
		t.Fatalf("HEAD must follow forked branch: %v %+v", err, symbolic)
	}
}

func TestCheckoutExistingBranchUpdatesHEAD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	head := seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: "other", Target: head,
	}); err != nil {
		t.Fatalf("put other branch: %v", err)
	}
	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefHEAD, Name: "HEAD", Symbolic: "other",
	}); err != nil {
		t.Fatalf("point HEAD at other: %v", err)
	}

	checkout := NewCheckoutSessionService(
		NewForkSessionService(store),
		newLoadSvc(store),
		store,
	)
	out, err := checkout.Checkout(ctx, inbound.CheckoutInput{
		From: "main", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd,
	})
	if err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if out.Branch != "main" {
		t.Fatalf("checkout branch = %q, want main", out.Branch)
	}
	symbolic, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || symbolic.Symbolic != "main" {
		t.Fatalf("HEAD must follow existing branch: %v %+v", err, symbolic)
	}
}

func TestCheckoutTagKeepsCurrentHEAD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	head := seedClaudeSnapshot(t, store)
	cwd := t.TempDir()

	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefTag, Name: "v1", Target: head,
	}); err != nil {
		t.Fatalf("put tag: %v", err)
	}
	checkout := NewCheckoutSessionService(
		NewForkSessionService(store),
		newLoadSvc(store),
		store,
	)
	out, err := checkout.Checkout(ctx, inbound.CheckoutInput{
		From: "v1", TargetProvider: domain.ProviderCodex, Mode: domain.FidelityFull, Cwd: cwd,
	})
	if err != nil {
		t.Fatalf("checkout tag: %v", err)
	}
	if out.Branch != "" {
		t.Fatalf("tag restore must be detached, got branch %q", out.Branch)
	}
	symbolic, err := store.GetRef(ctx, "", domain.RefHEAD, "HEAD")
	if err != nil || symbolic.Symbolic != "main" {
		t.Fatalf("tag restore must not move HEAD: %v %+v", err, symbolic)
	}
}
