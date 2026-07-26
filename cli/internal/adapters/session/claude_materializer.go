package session

import (
	"context"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ClaudeMaterializer records encoded Claude session bytes as a native session file (_RECONCILIATION C.1).
//
// Writes raw to ~/.claude/projects/<cwd-encoded>/<newid>.jsonl and returns resumeCmd="claude --resume <id>".
// Rewrites the sessionId in the internal record to the new id (id-rewrite — prevents conflicts with the original session).
type ClaudeMaterializer struct{}

// NewClaudeMaterializer creates a ClaudeMaterializer.
func NewClaudeMaterializer() *ClaudeMaterializer { return &ClaudeMaterializer{} }

// Provider returns claude.
func (m *ClaudeMaterializer) Provider() domain.ProviderKind { return domain.ProviderClaude }

// Materialize records raw (encoded Claude JSONL) as a native session file.
func (m *ClaudeMaterializer) Materialize(_ context.Context, raw []byte, cwd string) (string, string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	root, err := providerfs.ClaudeProjectsDir()
	if err != nil {
		return "", "", err
	}
	id := providerfs.NewSessionID()
	path := filepath.Join(root, providerfs.EncodeCwd(abs), id+".jsonl")
	if err := providerfs.EnsureRealDir(filepath.Dir(path), 0o755); err != nil {
		return "", "", err
	}
	// claude identifies sessions by filename and record sessionId, and reconstructs the conversation content using a UUID chain — id rewrite + UUID/parentUUID chain assignment to make it tangible.
	if err := providerfs.WriteRegularFileAtomic(path, nativizeClaudeSession(raw, id), 0o644); err != nil {
		return "", "", err
	}
	return path, "claude --resume " + id, nil
}

// Ensure ClaudeMaterializer implements outbound.SessionMaterializer.
var _ outbound.SessionMaterializer = (*ClaudeMaterializer)(nil)
