package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// settingsTestTeam answers with the team's bundles and keeps settings
// objects the way the local store does: by the hash of their content.
type settingsTestTeam struct {
	bundles map[string]domain.SettingsBundle
	objects map[domain.ContentHash]domain.SettingsBundle
}

func (f *settingsTestTeam) PullSettings(_ context.Context, _ string, kind string) (domain.SettingsBundle, error) {
	bundle, ok := f.bundles[kind]
	if !ok {
		return domain.SettingsBundle{}, domain.ErrNotFound
	}
	return bundle, nil
}

func (f *settingsTestTeam) PutSettingsObject(_ context.Context, bundle domain.SettingsBundle) (domain.ContentHash, error) {
	hash, err := domain.SettingsObjectHash(bundle)
	if err != nil {
		return "", err
	}
	f.objects[hash] = bundle
	return hash, nil
}

func (f *settingsTestTeam) GetSettingsObject(_ context.Context, hash domain.ContentHash) (domain.SettingsBundle, error) {
	bundle, ok := f.objects[hash]
	if !ok {
		return domain.SettingsBundle{}, domain.ErrNotFound
	}
	return bundle, nil
}

type settingsTestConnection struct{}

func (settingsTestConnection) Connect(context.Context, string) (inbound.ConnectOutput, error) {
	return inbound.ConnectOutput{Repo: domain.Repo{ID: "sha256:" + strings.Repeat("a", 64)}}, nil
}

// `cxt settings pull` replaces the local folders with the team's. What a
// folder held before is kept, and `cxt settings restore` brings it back.
func TestSettingsPullKeepsWhatItReplacedAndRestoreBringsItBack(t *testing.T) {
	repo := t.TempDir()
	activationTestGit(t, repo, "init", "-b", "main")
	t.Chdir(repo)
	t.Setenv("CXT_REMOTE", "https://synthetic.invalid/api/v1")
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("cxt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte(`{"mine":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	team := &settingsTestTeam{objects: map[domain.ContentHash]domain.SettingsBundle{}, bundles: map[string]domain.SettingsBundle{
		"claude": {Kind: "claude", Files: []domain.SettingsFile{{Path: "settings.json", ContentB64: base64.StdEncoding.EncodeToString([]byte(`{"team":true}`))}}},
	}}
	c := &Container{Connection: settingsTestConnection{}, Settings: team, SettingsObjects: team}

	// Nothing was replaced yet, so nothing is kept.
	if backups := loadBackups(repo); len(backups) != 0 {
		t.Fatalf("backups before any pull: %+v", backups)
	}
	if err := Run(c, []string{"cxt", "settings", "pull"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(mine); err != nil || string(got) != `{"team":true}` {
		t.Fatalf("after the pull: %q %v", got, err)
	}
	backups := loadBackups(repo)
	if len(backups) != 1 || backups[0].Note != "before settings pull" || backups[0].Claude == "" || backups[0].Agents != "" {
		t.Fatalf("what the pull kept: %+v", backups)
	}

	if err := Run(c, []string{"cxt", "settings", "restore"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(mine); err != nil || string(got) != `{"mine":true}` {
		t.Fatalf("after the restore: %q %v", got, err)
	}
	// Restoring is itself kept, so that it can be taken back.
	if backups := loadBackups(repo); len(backups) != 2 || backups[0].Note != "restore previous state" {
		t.Fatalf("after the restore: %+v", backups)
	}

	// A pull that applies nothing leaves no entry.
	team.bundles = map[string]domain.SettingsBundle{}
	if err := Run(c, []string{"cxt", "settings", "pull"}); err != nil {
		t.Fatal(err)
	}
	if backups := loadBackups(repo); len(backups) != 2 {
		t.Fatalf("a pull with nothing to apply kept something: %+v", backups)
	}
	// A failure other than "the team has none" stops the pull and is reported.
	offline := errors.New("server unreachable")
	c.Settings = settingsTestFailing{offline}
	if err := Run(c, []string{"cxt", "settings", "pull"}); !errors.Is(err, offline) {
		t.Fatalf("an unreachable server: %v", err)
	}
}

type settingsTestFailing struct{ err error }

func (f settingsTestFailing) PullSettings(context.Context, string, string) (domain.SettingsBundle, error) {
	return domain.SettingsBundle{}, f.err
}
