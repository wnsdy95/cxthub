package capture

// briefing.go — Team member context injection briefing sidecar (.cxt/briefing.json).
//
// After a git pull (post-merge), if a new team member snapshot summary is recorded from the remote,
// the next prompt's UserPromptSubmit (or SessionStart) hook consumes it once and injects into the live agent session
// (same protocol as Claude Code·Codex CLI — not visible to users, only to the model).
//
// The raw session is never merged (context convergence policy): Briefing is a one-way transmission in the summary layer,
// while each snapshot remains true to its session in the DAG.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// briefingMaxBytes is the maximum length of the injected text (to prevent prompt pollution).
const briefingMaxBytes = 4 << 10

// briefingTTL is the briefing validity period — stale briefings pulled long ago are not consumed.
const briefingTTL = 24 * time.Hour

type briefingFile struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

func briefingPath(cwd string) string { return filepath.Join(cwd, ".cxt", "briefing.json") }

// WriteBriefing records the briefing (overwriting — the latest pull replaces the previous one).
// In an inactive repo (.cxt file absent), it is a no-op (same as opt-in gate).
func WriteBriefing(cwd, text string) error {
	if !cxtEnabled(cwd) || text == "" {
		return nil
	}
	if len(text) > briefingMaxBytes {
		text = text[:briefingMaxBytes] + "\n…(omitted)"
	}
	b, err := json.Marshal(briefingFile{At: time.Now().UTC(), Text: text})
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileAtomic(cwd, filepath.Join(".cxt", "briefing.json"), b, 0o644)
}

// ConsumeBriefing reads and consumes (deletes) the briefing. Returns ("", false) if absent, corrupted, or expired.
// Atomic rename ensures one-time consumption: even if both claude·codex hooks fire simultaneously in separate processes,
// only one will succeed in renaming the file (the other will get ENOENT). Previous read-then-delete could lead to duplicate briefing injection (backlog #1 TOCTOU).
func ConsumeBriefing(cwd string) (string, bool) {
	source, err := providerfs.PrepareRepoFile(cwd, filepath.Join(".cxt", "briefing.json"), 0o755)
	if err != nil {
		return "", false
	}
	claimRel := filepath.Join(".cxt", fmt.Sprintf("briefing.json.claim.%d", os.Getpid()))
	claim, err := providerfs.PrepareRepoFile(cwd, claimRel, 0o755)
	if err != nil {
		return "", false
	}
	if err := os.Rename(source, claim); err != nil {
		return "", false // absent or another hook already claimed it
	}
	defer func() { _ = os.Remove(claim) }()
	data, err := providerfs.ReadRegularFile(claim)
	if err != nil {
		return "", false
	}
	var f briefingFile
	if json.Unmarshal(data, &f) != nil || f.Text == "" {
		return "", false
	}
	if time.Since(f.At) > briefingTTL {
		return "", false
	}
	return f.Text, true
}
