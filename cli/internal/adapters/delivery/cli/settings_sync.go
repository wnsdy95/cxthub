// settings_sync.go — the team's settings folders (.claude, .agents, .codex) and the backups of what they replaced.
//
// `cxt settings pull` and `cxt setup` replace the local folders with the
// team's. Before a folder is replaced, its state is stored as a
// content-addressed object and noted on a stack of backups, so that
// `cxt settings restore` can bring it back. Restoring is itself backed up.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// settingsBackup is a single settings folder state before replacement (hash is the local settingsobjs object).
type settingsBackup struct {
	At     time.Time          `json:"at"`
	Note   string             `json:"note"`
	Claude domain.ContentHash `json:"claude,omitempty"`
	Agents domain.ContentHash `json:"agents,omitempty"`
	Codex  domain.ContentHash `json:"codex,omitempty"`
}

func loadBackups(cwd string) []settingsBackup {
	cwd = cxtRepoRoot(context.Background(), cwd)
	var out []settingsBackup
	if b, err := providerfs.ReadRepoFile(cwd, filepath.Join(".cxt", "settings-backups.json")); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveBackups(cwd string, list []settingsBackup) error {
	cwd = cxtRepoRoot(context.Background(), cwd)
	if len(list) > 50 {
		list = list[:50] // stack limit
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	return providerfs.WriteRepoFileAtomic(cwd, filepath.Join(".cxt", "settings-backups.json"), b, 0o644)
}

// rememberSettings reads the settings folders as they are now and returns
// what stores that state and puts it on the stack of backups. The caller
// calls it only after it replaced something, so that a pull that changed
// nothing writes nothing.
func rememberSettings(ctx context.Context, c *Container, cwd, note string) func() {
	kinds := []string{"claude", "agents", "codex"}
	before := map[string]domain.SettingsBundle{}
	for _, kind := range kinds {
		if bundle, ok := capture.ReadSettingsDir(cwd, kind); ok {
			before[kind] = bundle
		}
	}
	return func() {
		if c == nil || c.SettingsObjects == nil || len(before) == 0 {
			return
		}
		stored := map[string]domain.ContentHash{}
		for kind, bundle := range before {
			if hash, err := c.SettingsObjects.PutSettingsObject(ctx, bundle); err == nil {
				stored[kind] = hash
			}
		}
		if len(stored) == 0 {
			return
		}
		entry := settingsBackup{At: time.Now().UTC(), Note: note, Claude: stored["claude"], Agents: stored["agents"], Codex: stored["codex"]}
		_ = saveBackups(cwd, append([]settingsBackup{entry}, loadBackups(cwd)...))
	}
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

// inspectSettingsHashes computes identities without adding objects to .cxt.
// Backups still use currentSettingsHashes to durably preserve their contents.
func inspectSettingsHashes(cwd string) (claude, agents, codex domain.ContentHash, err error) {
	hashes := make([]domain.ContentHash, 3)
	for i, kind := range []string{"claude", "agents", "codex"} {
		bundle, ok := capture.ReadSettingsDir(cwd, kind)
		if !ok {
			continue
		}
		if err := domain.ValidateSettingsBundle(kind, "", bundle); err != nil {
			return "", "", "", err
		}
		hashes[i], err = domain.SettingsObjectHash(bundle)
		if err != nil {
			return "", "", "", err
		}
	}
	return hashes[0], hashes[1], hashes[2], nil
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
