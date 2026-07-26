package session

import (
	"context"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// CodexMaterializer records encoded Codex rollout bytes as a native session file
// (_RECONCILIATION C.1).
//
// ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<newid>.jsonl writes raw and returns resumeCmd="codex resume <id>".
type CodexMaterializer struct{}

// NewCodexMaterializer creates a CodexMaterializer.
func NewCodexMaterializer() *CodexMaterializer { return &CodexMaterializer{} }

// Provider returns codex.
func (m *CodexMaterializer) Provider() domain.ProviderKind { return domain.ProviderCodex }

// Materialize records raw (encoded Codex rollout JSONL) as a native session file.
func (m *CodexMaterializer) Materialize(_ context.Context, raw []byte, _ string) (string, string, error) {
	root, err := providerfs.CodexSessionsDir()
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	id := providerfs.NewSessionID()
	dir := filepath.Join(root, now.Format("2006"), now.Format("01"), now.Format("02"))
	path := filepath.Join(dir, "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	if err := providerfs.EnsureRealDir(dir, 0o755); err != nil {
		return "", "", err
	}
	// codex resume finds the rollout by session_meta.id — the internal id must be rewritten to a new id
	// so that the materialized version actually resumes (if the original id is unchanged, it will not be found or conflict with the original session).
	if err := providerfs.WriteRegularFileAtomic(path, rewriteCodexSessionID(raw, id), 0o644); err != nil {
		return "", "", err
	}
	return path, "codex resume " + id, nil
}

// Ensure CodexMaterializer implements outbound.SessionMaterializer.
var _ outbound.SessionMaterializer = (*CodexMaterializer)(nil)
