package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// ProviderCodec is a codec for provider raw JSONL ↔ CIR bidirectional transformation (domain model).
//
// Decode: claude/codex JSONL → CIR (provider-independent).
// Encode: CIR → target provider format.
// Tool name mapping (claude Bash/Edit ↔ codex shell/apply_patch …) is performed here.
type ProviderCodec interface {

	// Encode converts CIR to target provider session file bytes.
	// If target == cir.Envelope.SourceProvider, it's full fidelity (locked original re-injection),
	// otherwise, it's reconstructed (locked reasoning disabled, redacted_summary fallback).
	Encode(ctx context.Context, cir domain.CIRDocument, target domain.ProviderKind) ([]byte, error)
}

// GitContext is a port for querying the current code repo / branch (domain model).
//
// Claude can augment with gitBranch, but Codex is the unique branch source.
type GitContext interface {
	// CurrentRepo identifies the code repo containing the current working directory.
	// Produces a normalized remote URL or cwd fallback key as Repo.ID.
	CurrentRepo(ctx context.Context, cwd string) (domain.Repo, error)
}

// MemorySource is a port that absorbs provider native memory (compatibility rules).
//
// Absorb if available: Claude MEMORY.md, Codex rollout_summary, etc. Input source for native-first strategy.
// NativeMemory.Scope states whether that value is automatically available to
// a new session in the working tree or remains bound to the source session.
type MemorySource interface {

	// ReadNative reads provider native memory for the exact provider session when
	// sessionID is available. An empty sessionID falls back to the newest memory
	// associated with cwd. Implementations must set NativeMemory.Scope. Returns
	// found=false if not found (no error).
	ReadNative(ctx context.Context, cwd, sessionID string) (native domain.NativeMemory, found bool, err error)
}

// MemoryDistiller is a port that generates MemoryDigest (summary memory) (compatibility rules).
//
// Native-first + fallback: Absorb if native exists (non-nil), otherwise self-distill from CIR.
// Its digests are the summaries uploaded for a session's commits. Uses redacted_summary for reasoning.
type MemoryDistiller interface {
	// Distill extracts summaries/key facts/unresolved tasks from native (if available) and CIR.
	// If native==nil, self-distill from CIR (fallback). Deterministic.
	Distill(ctx context.Context, cir domain.CIRDocument, native *domain.NativeMemory) (domain.MemoryDigest, error)
}

// SessionMaterializer is a port that records session bytes (encoded in the target provider format) to a native session file,
// enabling resume (CIR→Provider Byte Encoding is the responsibility of ProviderCodec.Encode, and the materializer is a pure writer that receives the raw result and writes to the file system [Hexagonal Architecture: Adapter→Adapter Dependency Avoidance]). The session fork use case (`cxt fork`) orchestrates codec.Encode → materializer.Materialize.
//
// claude writes to ~/.claude/projects/<cwd-encoded>/<newid>.jsonl and returns resumeCmd="claude --resume <id>", while codex writes to ~/.codex/sessions/.../rollout-*.jsonl and returns "codex resume <id>".
type SessionMaterializer interface {

	// Materialize records the encoded session bytes (raw) to a native session file based on the cwd. It returns the sessionPath (record path) and resumeCmd (native resume command).
	Materialize(ctx context.Context, raw []byte, cwd string) (sessionPath string, resumeCmd string, err error)
}
