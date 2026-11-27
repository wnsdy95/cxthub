package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
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
