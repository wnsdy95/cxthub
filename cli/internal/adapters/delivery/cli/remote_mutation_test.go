package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestConfigCoordinationRemoteAddBeforePublication(t *testing.T) {
	for _, kind := range []string{"success", "offline", "reject", "cancel", "concurrent-origin", "concurrent-ABA", "concurrent-setting", "git-changed"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := t.TempDir()
			const a = "https://example.invalid/team/a"
			const b = "https://example.invalid/team/b"
			var connected, validated bool
			container := &Container{PrepareRemoteConnection: func(_ context.Context, cwd, raw, origin string) (PreparedRemoteConnection, error) {
				if cwd != root || raw != a || origin != "" {
					t.Fatalf("wrong frozen input: %s %s %s", cwd, raw, origin)
				}
				return PreparedRemoteConnection{URL: a, Connect: func(ctx context.Context) (inbound.ConnectOutput, error) {
					connected = true
					if _, ok := remotecfg.Origin(root); ok {
						t.Fatal("tentative origin published before Connect")
					}
					// An EX path can run inside this network callback: no capture/config lock
					// is held across Connect. This is real storage's existing setup exclusion.
					if _, err := storage.NewFileStore(root).TrackingPristine(ctx, remotecfg.RepoIDFor(a)); err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "cancel":
						cancel()
						return inbound.ConnectOutput{}, ctx.Err()
					case "offline":
						return inbound.ConnectOutput{}, errors.New("synthetic offline")
					case "reject":
						return inbound.ConnectOutput{}, &backendclient.HTTPError{Status: 409, Code: "git_origin_mismatch"}
					case "concurrent-origin", "concurrent-ABA":
						if err := configFixtureSave(root, remotecfg.Remotes{"origin": b, "mirror": b}); err != nil {
							t.Fatal(err)
						}
						if kind == "concurrent-ABA" {
							if err := configFixtureSave(root, remotecfg.Remotes{"origin": a, "mirror": b}); err != nil {
								t.Fatal(err)
							}
						}
						if err := remotecfg.SetLoadMode(ctx, root, "memory"); err != nil {
							t.Fatal(err)
						}
						return inbound.ConnectOutput{}, &backendclient.HTTPError{Status: 409, Code: "git_origin_mismatch"}
					case "concurrent-setting":
						if err := remotecfg.SetLoadMode(ctx, root, "memory"); err != nil {
							t.Fatal(err)
						}
					}
					return inbound.ConnectOutput{Repo: domain.Repo{ID: remotecfg.RepoIDFor(a)}}, nil
				}, ValidateLocal: func(context.Context) error {
					validated = true
					if kind == "git-changed" {
						return domain.ErrSelectionChanged
					}
					return nil
				}}, nil
			}}
			err := runRemote(ctx, container, root, []string{"add", "origin", a})
			if !connected {
				t.Fatal("missing Connect")
			}
			remotes, _ := remotecfg.Load(root)
			switch kind {
			case "success", "offline":
				if err != nil || remotes["origin"] != a || !validated {
					t.Fatalf("publication failed: %v %v", remotes, err)
				}
			case "concurrent-origin", "concurrent-ABA":
				var denied *backendclient.HTTPError
				if !errors.As(err, &denied) || denied.Code != "git_origin_mismatch" {
					t.Fatalf("wrong error: %v", err)
				}
				want := b
				if kind == "concurrent-ABA" {
					want = a
				}
				if remotes["origin"] != want || remotes["mirror"] != b || remotecfg.LoadMode(root) != "memory" {
					t.Fatalf("rejection erased others: %v", remotes)
				}
			default:
				if err == nil || remotes["origin"] != "" {
					t.Fatalf("failed attempt published origin: %v %v", remotes, err)
				}
				if kind == "concurrent-setting" && (!errors.Is(err, remotecfg.ErrChanged) || remotecfg.LoadMode(root) != "memory") {
					t.Fatal("concurrent setting not preserved")
				}
				if kind == "reject" {
					if _, err := os.Stat(filepath.Join(root, ".cxt/config")); !os.IsNotExist(err) {
						t.Fatal("rejected connection wrote config")
					}
				}
			}
		})
	}
}
