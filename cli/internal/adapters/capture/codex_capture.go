package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// CodexCaptureSource implements a CaptureSource for detecting Codex rollout session files (capture path).
//
// Codex session file locations:
//
//	~/.codex/sessions/YYYY/MM/DD/rollout-<ISO_ts>-<uuid>.jsonl
//
// Active session detection: Iterates through rollout-*.jsonl files in the sessions tree, selecting the file with the latest mtime where the first session_meta line's payload.cwd matches the current cwd.
type CodexCaptureSource struct{}

// NewCodexCapture creates a CodexCaptureSource.
func NewCodexCapture() *CodexCaptureSource { return &CodexCaptureSource{} }

// Provider returns the provider (codex) this adapter is responsible for.
func (c *CodexCaptureSource) Provider() domain.ProviderKind { return domain.ProviderCodex }

func codexSessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// rolloutCwd reads the payload.cwd from the first line (session_meta) of the rollout file.
func rolloutCwd(path string) (string, bool) {
	f, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	if !sc.Scan() {
		return "", false
	}
	var line struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
		return "", false
	}
	if line.Type != "session_meta" {
		return "", false
	}
	return line.Payload.Cwd, true
}

// LocateActiveSession detects the path to the active Codex session file for the current cwd. Returns domain.ErrNoActiveSession if none found.
func (c *CodexCaptureSource) LocateActiveSession(_ context.Context, cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	root, err := codexSessionsDir()
	if err != nil {
		return "", err
	}
	candidates := map[string]int64{}
	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		name := info.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if c, ok := rolloutCwd(path); ok && c == abs {
			// Ledger correction: Materialized but unprocessed recovery sessions or isolated sessions are not active candidates.
			if providerfs.CaptureExcluded(cwd, path, info.Size()) {
				return nil
			}
			candidates[path] = info.ModTime().UnixNano()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	best := pickLatest(candidates)
	if best == "" {
		return "", domain.ErrNoActiveSession
	}
	return best, nil
}

// SessionFilesForCwd returns all rollout session files in this cwd (for branch switch isolation). Unlike LocateActiveSession, it does not apply CaptureExcluded — isolation is unrelated to activity. Files already isolated are renamed with .jsonl.superseded and thus excluded.
func (c *CodexCaptureSource) SessionFilesForCwd(_ context.Context, cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	root, err := codexSessionsDir()
	if err != nil {
		return nil
	}
	var files []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		name := info.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if sc, ok := rolloutCwd(path); ok && sc == abs {
			files = append(files, path)
		}
		return nil
	})
	return files
}

// ReadSession reads the raw bytes of the given rollout JSONL file.
func (c *CodexCaptureSource) ReadSession(_ context.Context, path string) ([]byte, error) {
	return providerfs.ReadRegularFile(path)
}

// SessionFilePath calculates the target Codex distillation file path on load.
// ~/.codex/sessions/YYYY/MM/DD/rollout-<now ISO>-<newUuid>.jsonl.
func (c *CodexCaptureSource) SessionFilePath(_ context.Context, _ string, _ domain.ProviderKind) (string, error) {
	root, err := codexSessionsDir()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	dateDir := filepath.Join(root, now.Format("2006"), now.Format("01"), now.Format("02"))
	fname := "rollout-" + now.Format("2006-01-02T15-04-05") + "-" + newSessionID() + ".jsonl"
	return filepath.Join(dateDir, fname), nil
}

// Ensure CodexCaptureSource implements outbound.CaptureSource.
var _ outbound.CaptureSource = (*CodexCaptureSource)(nil)
