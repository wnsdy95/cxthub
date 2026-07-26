// settings_sync.go — Commit/branch movement sync of .claude/.agents/.codex folders + backup stack.
//
// Policy (user decision): If the target snapshot's settings folder is different from the current one on rollback/branch move, **replace** it. However, before replacement, store the current state as a content-addressed object and push it onto the .cxt/settings-backups.json stack to allow recovery at any time (to prevent losing my previous state while receiving team member settings).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// settingsBackup is a single settings folder state before replacement (hash is the local settingsobjs object).
type settingsBackup struct {
	At     time.Time          `json:"at"`
	Note   string             `json:"note"`
	Claude domain.ContentHash `json:"claude,omitempty"`
	Agents domain.ContentHash `json:"agents,omitempty"`
	Codex  domain.ContentHash `json:"codex,omitempty"`
}

func backupsPath(cwd string) string { return filepath.Join(cwd, ".cxt", "settings-backups.json") }

func loadBackups(cwd string) []settingsBackup {
	var out []settingsBackup
	if b, err := providerfs.ReadRepoFile(cwd, filepath.Join(".cxt", "settings-backups.json")); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveBackups(cwd string, list []settingsBackup) error {
	if len(list) > 50 {
		list = list[:50] // stack limit
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	return providerfs.WriteRepoFileAtomic(cwd, filepath.Join(".cxt", "settings-backups.json"), b, 0o644)
}

// currentSettingsHashes stores the current folder state as an object and returns the hash (returns "" if none).
func currentSettingsHashes(ctx context.Context, c *Container, cwd string) (claude, agents, codex domain.ContentHash) {
	put := func(kind string) domain.ContentHash {
		if b, ok := capture.ReadSettingsDir(cwd, kind); ok {
			if h, err := c.SettingsObjects.PutSettingsObject(ctx, b); err == nil {
				return h
			}
		}
		return ""
	}
	return put("claude"), put("agents"), put("codex")
}

// syncSettingsToSnapshot syncs the local state to the target snapshot's settings folder state. If different: push current to backup stack → replace with target bundle. Preserve kinds not recorded in the snapshot.
func syncSettingsToSnapshot(ctx context.Context, c *Container, cwd string, snapID domain.ContentHash, note string) {
	all, err := c.List.List(ctx, inbound.ListInput{})
	if err != nil {
		return
	}
	var snap *domain.Snapshot
	for i := range all.Snapshots {
		if all.Snapshots[i].ID == snapID {
			snap = &all.Snapshots[i]
			break
		}
	}
	if snap == nil || (snap.ClaudeSettings == "" && snap.AgentsSettings == "" && snap.CodexSettings == "") {
		return // legacy snapshot without settings metadata; preserve current folders
	}
	curClaude, curAgents, curCodex := currentSettingsHashes(ctx, c, cwd)
	type job struct {
		kind   string
		cur    domain.ContentHash
		target domain.ContentHash
	}
	jobs := []job{{"claude", curClaude, snap.ClaudeSettings}, {"agents", curAgents, snap.AgentsSettings}, {"codex", curCodex, snap.CodexSettings}}
	changed := false
	for _, j := range jobs {
		if j.target == "" || j.target == j.cur {
			continue
		}
		changed = true
	}
	if !changed {
		return
	}
	// backup push (entire current state — even partial replacements are stored as a single point in time).
	backups := append([]settingsBackup{{At: time.Now().UTC(), Note: note, Claude: curClaude, Agents: curAgents, Codex: curCodex}}, loadBackups(cwd)...)
	_ = saveBackups(cwd, backups)
	for _, j := range jobs {
		if j.target == "" || j.target == j.cur {
			continue
		}
		bundle, gerr := c.SettingsObjects.GetSettingsObject(ctx, j.target)
		if gerr != nil {
			continue
		}
		if n, werr := capture.WriteSettingsDir(cwd, j.kind, j.target, bundle); werr == nil {
			fmt.Printf("cxt: .%s/ replaced by snapshot state (%d files) — previous state backed up (cxt settings list)\n", j.kind, n)
		}
	}
}

// restoreSettingsBackup restores the folder from the idx item in the backup stack (backing up the current state before restoration — symmetric).
func restoreSettingsBackup(ctx context.Context, c *Container, cwd string, idx int) error {
	backups := loadBackups(cwd)
	if idx < 0 || idx >= len(backups) {
		return fmt.Errorf("backup %d not found (check cxt settings list)", idx)
	}
	target := backups[idx]
	curClaude, curAgents, curCodex := currentSettingsHashes(ctx, c, cwd)
	backups = append([]settingsBackup{{At: time.Now().UTC(), Note: "restore previous state", Claude: curClaude, Agents: curAgents, Codex: curCodex}}, backups...)
	_ = saveBackups(cwd, backups)
	for _, pair := range []struct {
		kind string
		h    domain.ContentHash
	}{{"claude", target.Claude}, {"agents", target.Agents}, {"codex", target.Codex}} {
		if pair.h == "" {
			continue
		}
		bundle, err := c.SettingsObjects.GetSettingsObject(ctx, pair.h)
		if err != nil {
			continue
		}
		if n, werr := capture.WriteSettingsDir(cwd, pair.kind, pair.h, bundle); werr == nil {
			fmt.Printf("restored .%s/ (%d files)\n", pair.kind, n)
		}
	}
	return nil
}
