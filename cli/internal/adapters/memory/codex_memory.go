package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// CodexMemorySource reads Codex native memory from memories_1.sqlite stage1_outputs (compatibility rules).
type CodexMemorySource struct{}

// NewCodexMemorySource creates a CodexMemorySource.
func NewCodexMemorySource() *CodexMemorySource { return &CodexMemorySource{} }

// Provider returns codex.
func (s *CodexMemorySource) Provider() domain.ProviderKind { return domain.ProviderCodex }

// ReadNative reads Codex native memory.
//
// Storage structure (empirically verified, ~/.codex/memories_1.sqlite): stage1_outputs stores raw_memory and rollout_summary by thread_id (the rollout UUID). Because the table has no cwd column, the adapter derives candidate thread IDs from rollout files associated with this working directory.
//
// Reading uses the sqlite3 CLI (-readonly -json), keeping this module free of an embedded SQLite driver.
// Absence of sqlite3, absence of DB, or absence of matching row results in found=false (no error) → CIR self-distillation fallback.
func (s *CodexMemorySource) ReadNative(ctx context.Context, cwd, sessionID string) (domain.NativeMemory, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return domain.NativeMemory{}, false, nil
	}
	db := filepath.Join(home, ".codex", "memories_1.sqlite")
	dbFile, err := providerfs.OpenRegularFile(db)
	if err != nil {
		return domain.NativeMemory{}, false, nil
	}
	_ = dbFile.Close()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return domain.NativeMemory{}, false, nil
	}
	var ids []string
	for _, p := range capture.NewCodexCapture().SessionFilesForCwd(ctx, cwd) {
		if id := threadIDFromRollout(p); id != "" && isSafeThreadID(id) {
			ids = append(ids, "'"+id+"'")
		}
	}
	if len(ids) == 0 {
		return domain.NativeMemory{}, false, nil
	}
	if sessionID != "" {
		if !isSafeThreadID(sessionID) {
			return domain.NativeMemory{}, false, nil
		}
		quoted := "'" + sessionID + "'"
		found := false
		for _, id := range ids {
			if id == quoted {
				found = true
				break
			}
		}
		if !found {
			return domain.NativeMemory{}, false, nil
		}
		ids = []string{quoted}
	}
	query := fmt.Sprintf(
		"SELECT thread_id, raw_memory, rollout_summary FROM stage1_outputs WHERE thread_id IN (%s) AND (TRIM(raw_memory) <> '' OR TRIM(rollout_summary) <> '') ORDER BY source_updated_at DESC, thread_id DESC LIMIT 1;",
		strings.Join(ids, ","))
	out, err := exec.CommandContext(ctx, sqlite, "-readonly", "-json", db, query).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return domain.NativeMemory{}, false, nil
	}
	var rows []struct {
		ThreadID       string `json:"thread_id"`
		RawMemory      string `json:"raw_memory"`
		RolloutSummary string `json:"rollout_summary"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return domain.NativeMemory{}, false, nil
	}
	for _, r := range rows {
		text := strings.TrimSpace(r.RawMemory)
		if text == "" {
			text = strings.TrimSpace(r.RolloutSummary)
		}
		if text == "" {
			continue
		}
		return domain.NativeMemory{
			Provider: domain.ProviderCodex,
			Source:   "codex:memories_1.sqlite",
			Scope:    domain.NativeMemoryScopeSession,
			Text:     text,
		}, true, nil
	}
	return domain.NativeMemory{}, false, nil
}

// threadIDFromRollout extracts the thread uuid from a rollout file name.
// Format: rollout-<2006-01-02T15-04-05>-<uuid>.jsonl (fixed 19-character timestamp).
func threadIDFromRollout(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	base = strings.TrimPrefix(base, "rollout-")
	if len(base) <= 20 {
		return ""
	}
	return base[20:]
}

// isSafeThreadID allows only uuid string sets for SQL IN clause safety.
func isSafeThreadID(id string) bool {
	if len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == '-') {
			return false
		}
	}
	return true
}

// CodexMemorySink writes a MemoryDigest to Codex AGENTS.md (compatibility rules).
type CodexMemorySink struct{}

// NewCodexMemorySink creates a CodexMemorySink.
func NewCodexMemorySink() *CodexMemorySink { return &CodexMemorySink{} }

// Provider returns codex.
func (s *CodexMemorySink) Provider() domain.ProviderKind { return domain.ProviderCodex }

// Inject refreshes the bounded cxt-managed region in cwd/AGENTS.md while
// preserving user-authored content outside the markers.
func (s *CodexMemorySink) Inject(_ context.Context, digest domain.MemoryDigest, cwd string) (string, error) {
	path := filepath.Join(cwd, "AGENTS.md")
	if err := writeManagedMemory(path, digest); err != nil {
		return "", err
	}
	return path, nil
}

// Ensure Codex adapters implement the memory ports.
var (
	_ outbound.MemorySource = (*CodexMemorySource)(nil)
	_ outbound.MemorySink   = (*CodexMemorySink)(nil)
)
