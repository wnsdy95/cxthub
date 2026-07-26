package capture

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// ClaudeCaptureSource implements a CaptureSource for detecting Claude Code session files (capture path).
//
// Claude Code session file locations:
//
//	~/.claude/projects/<cwd-encoded>/<sessionId>.jsonl
//
// Active session detection: Selects the latest *.jsonl file in the <cwd-encoded> directory by mtime.
type ClaudeCaptureSource struct{}

// NewClaudeCapture creates a new ClaudeCaptureSource.
func NewClaudeCapture() *ClaudeCaptureSource { return &ClaudeCaptureSource{} }

// Provider returns the provider (claude) this adapter is responsible for.
func (c *ClaudeCaptureSource) Provider() domain.ProviderKind { return domain.ProviderClaude }

func claudeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// LocateActiveSession detects the path to the active Claude session file for the given cwd.
// Returns the latest *.jsonl file in ~/.claude/projects/<cwd-encoded>/ by mtime.
// If none found, returns domain.ErrNoActiveSession.
func (c *ClaudeCaptureSource) LocateActiveSession(_ context.Context, cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	root, err := claudeProjectsDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, encodeCwd(abs))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", domain.ErrNoActiveSession
		}
		return "", err
	}
	candidates := map[string]int64{}
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		// Ledger correction: Restored copies and isolated sessions are not active candidates after materialization.
		if providerfs.CaptureExcluded(cwd, p, info.Size()) {
			continue
		}
		candidates[p] = info.ModTime().UnixNano()
	}
	best := pickLatest(candidates)
	if best == "" {
		return "", domain.ErrNoActiveSession
	}
	return best, nil
}

// ReadSession reads the JSONL bytes from the given session file path.
func (c *ClaudeCaptureSource) ReadSession(_ context.Context, path string) ([]byte, error) {
	return providerfs.ReadRegularFile(path)
}

// SessionFilePath calculates the target Claude session file path for loading.
// ~/.claude/projects/<encodeCwd(cwd)>/<newSessionId>.jsonl (new UUID).
func (c *ClaudeCaptureSource) SessionFilePath(_ context.Context, cwd string, _ domain.ProviderKind) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	root, err := claudeProjectsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, encodeCwd(abs), newSessionID()+".jsonl"), nil
}

// Ensure ClaudeCaptureSource implements outbound.CaptureSource.
var _ outbound.CaptureSource = (*ClaudeCaptureSource)(nil)
