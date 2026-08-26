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
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// briefingMaxBytes is the maximum length of the injected text (to prevent prompt pollution).
const briefingMaxBytes = 4 << 10

// briefingTTL is the briefing validity period — stale briefings pulled long ago are not consumed.
const briefingTTL = 24 * time.Hour

type briefingFile struct {
	At    time.Time `json:"at"`
	Text  string    `json:"text,omitempty"`  // legacy single-entry format
	Texts []string  `json:"texts,omitempty"` // ordered pull queue
}

func briefingPath(cwd string) string { return filepath.Join(cwd, ".cxt", "briefing.json") }

// WriteBriefing records the briefing (overwriting — the latest pull replaces the previous one).
// In an inactive repo (.cxt file absent), it is a no-op (same as opt-in gate).
func WriteBriefing(cwd, text string) error {
	if !cxtEnabled(cwd) || text == "" {
		return nil
	}
	entries := []string{}
	if data, err := providerfs.ReadRepoFile(cwd, filepath.Join(".cxt", "briefing.json")); err == nil {
		var old briefingFile
		if json.Unmarshal(data, &old) == nil && time.Since(old.At) <= briefingTTL {
			entries = briefingEntries(old)
		}
	}
	if len(entries) == 0 || entries[len(entries)-1] != text {
		entries = append(entries, text)
	}
	entries = boundBriefingEntries(entries, briefingMaxBytes)
	b, err := json.Marshal(briefingFile{At: time.Now().UTC(), Texts: entries})
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
	if json.Unmarshal(data, &f) != nil {
		return "", false
	}
	if time.Since(f.At) > briefingTTL {
		return "", false
	}
	entries := briefingEntries(f)
	if len(entries) == 0 {
		return "", false
	}
	return strings.Join(entries, "\n\n"), true
}

func briefingEntries(f briefingFile) []string {
	if len(f.Texts) > 0 {
		return append([]string(nil), f.Texts...)
	}
	if f.Text != "" {
		return []string{f.Text}
	}
	return nil
}

func boundBriefingEntries(entries []string, maxBytes int) []string {
	for len(entries) > 1 && len(strings.Join(entries, "\n\n")) > maxBytes {
		entries = entries[1:]
	}
	if len(entries) == 1 && len(entries[0]) > maxBytes {
		const marker = "…(earlier text omitted)\n"
		runes := []rune(entries[0])
		for len(runes) > 0 && len(string(runes))+len(marker) > maxBytes {
			runes = runes[1:]
		}
		entries[0] = marker + string(runes)
	}
	return entries
}
