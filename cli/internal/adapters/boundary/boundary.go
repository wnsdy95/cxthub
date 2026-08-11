// Package boundary handles context transition boundaries (.cxt/boundary.json) recording, isolation, and execution.
//
// Principle: "Transitioning ends the old session" — enforces three layers without provider API dependency:
//   - Isolation (Supersede): renames old session file + excludes from ledger permanently → record-level certainty
//   - Notification (Notify): OS alert — notifies others using the terminal if someone else is waiting
//   - Enforcement (EnforceKill): kills agent process that holds the session file (POSIX lsof/kill)
//
// Wrapper (cxt claude) automates child restarts by monitoring boundary.json.
package boundary

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Boundary is a single context transition record.
type Boundary struct {
	At         string   `json:"at"`
	PrevBranch string   `json:"prev_branch"`
	Branch     string   `json:"branch"`
	SeedPath   string   `json:"seed_path,omitempty"`
	SeedID     string   `json:"seed_id,omitempty"` // resume target session file ID (file name UUID)
	ResumeCmd  string   `json:"resume_cmd,omitempty"`
	Superseded []string `json:"superseded,omitempty"` // paths of superseded session files (after renaming)
}

func boundaryPath(repoRoot string) string { return filepath.Join(repoRoot, ".cxt", "boundary.json") }

// Record logs the boundary (keeps only the last transition — wrapper/enforcement care only about the latest boundary).
func Record(repoRoot string, b Boundary) error {
	// Preserve sub-second ordering. The wrapper records its child start time with
	// nanosecond precision; RFC3339 second truncation can make a newly written
	// boundary appear older than a child started earlier in the same second.
	b.At = time.Now().UTC().Format(time.RFC3339Nano)
	normalized, ok := validateBoundary(b)
	if !ok || len(normalized.Superseded) != len(b.Superseded) {
		return fmt.Errorf("invalid context boundary")
	}
	b = normalized
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileAtomic(repoRoot, filepath.Join(".cxt", "boundary.json"), data, 0o644)
}

// Load returns the last boundary (ok=false if none).
func Load(repoRoot string) (Boundary, bool) {
	var b Boundary
	data, err := providerfs.ReadRepoFile(repoRoot, filepath.Join(".cxt", "boundary.json"))
	if err != nil || json.Unmarshal(data, &b) != nil {
		return Boundary{}, false
	}
	return validateBoundary(b)
}

func validateBoundary(b Boundary) (Boundary, bool) {
	at, err := time.Parse(time.RFC3339, b.At)
	if err != nil || at.After(time.Now().Add(time.Minute)) {
		return Boundary{}, false
	}
	for _, branch := range []string{b.PrevBranch, b.Branch} {
		if branch != "" && domain.ValidateBranchName(branch) != nil {
			return Boundary{}, false
		}
	}
	if b.SeedID != "" && !providerfs.ValidSessionID(b.SeedID) {
		return Boundary{}, false
	}
	if b.SeedPath != "" && !providerfs.IsProviderSessionPath(b.SeedPath) {
		return Boundary{}, false
	}
	if len(b.ResumeCmd) > 4096 {
		return Boundary{}, false
	}
	for _, r := range b.ResumeCmd {
		if unicode.IsControl(r) {
			return Boundary{}, false
		}
	}
	filtered := b.Superseded[:0]
	for _, path := range b.Superseded {
		if providerfs.IsProviderSessionPath(path) {
			filtered = append(filtered, path)
		}
	}
	b.Superseded = filtered
	return b, true
}

// Supersede isolates a session file: renames to <path>.superseded (exits from *.jsonl glob detection) + records original path permanently in ledger (fd process recreating the path will still be excluded).
// Returns: path after renaming ("" = file not found).
func Supersede(repoRoot, path string) string {
	if !providerfs.IsProviderSessionPath(path) {
		return ""
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	renamed := path + ".superseded"
	if err := os.Rename(path, renamed); err != nil {
		renamed = path // exclusion from ledger remains valid even on rename failure
	}
	_ = providerfs.MarkSuperseded(repoRoot, path)
	return renamed
}

// RestoreSuperseded rolls back Supersede when the transition boundary could
// not be recorded. The bytes remain recoverable even if rename-back fails; in
// that case the ledger marker is intentionally retained.
func RestoreSuperseded(repoRoot, renamed string) bool {
	if !providerfs.IsProviderSessionPath(renamed) {
		return false
	}
	original := strings.TrimSuffix(renamed, ".superseded")
	if original == renamed {
		return providerfs.UnmarkSuperseded(repoRoot, original) == nil
	}
	if _, err := os.Stat(original); err == nil {
		// A provider recreated the path while its old descriptor pointed at the
		// renamed file. Never replace those newly written bytes.
		return false
	}
	if err := os.Rename(renamed, original); err != nil {
		return false
	}
	return providerfs.UnmarkSuperseded(repoRoot, original) == nil
}

// Notify sends an OS alert (best-effort — macOS osascript / Linux notify-send).
func Notify(title, msg string) {
	switch runtime.GOOS {
	case "darwin":
		script := "display notification " + strconv.Quote(msg) + " with title " + strconv.Quote(title)
		_ = exec.Command("osascript", "-e", script).Start()
	case "linux":
		_ = exec.Command("notify-send", title, msg).Start()
	}
}

// EnforceKill kills the process holding the isolated session file (i.e., the agent that wrote that session).
// Uses only POSIX lsof/kill. If the wrapper (cxt claude) is holding the child, it detects the child's death
// and restarts the seed, so the executor and wrapper naturally join.
func EnforceKill(repoRoot string) int {
	b, ok := Load(repoRoot)
	if !ok {
		return 0
	}
	killed := 0
	self := os.Getpid()
	for _, p := range b.Superseded {
		if !providerfs.IsProviderSessionPath(p) {
			continue
		}
		out, err := exec.Command("lsof", "-t", p).Output()
		if err != nil {
			continue // no one is holding it
		}
		for _, line := range strings.Fields(string(out)) {
			pid, cerr := strconv.Atoi(line)
			if cerr != nil || pid == self {
				continue
			}
			if proc, ferr := os.FindProcess(pid); ferr == nil {
				if proc.Signal(syscall.SIGTERM) == nil {
					killed++
				}
			}
		}
	}
	return killed
}
