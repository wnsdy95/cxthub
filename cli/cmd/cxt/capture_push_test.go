package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type capturePushRoundTrip func(*http.Request) (*http.Response, error)

func (f capturePushRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func capturePushJSON(t *testing.T, r *http.Request, status int, value any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: r}
}

// Real factory, Sync service and BackendClient; all HTTP stays in this in-memory
// transport. Mutation occurs at request boundaries, without sleeps or servers.
func TestPrepareCapturePushFreezesDestinationCredentialsAndRepository(t *testing.T) {
	for _, tc := range []struct {
		name, path          string
		apiOnly, savedToken bool
	}{
		{"legacy-repository-url", "/team/project", false, false},
		{"repository-url-saved-token", "/team/group/project", false, true},
		{"api-fallback", "/api/v1", true, false},
		{"api-fallback-saved-token", "/api/v1/", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			root := t.TempDir()
			selectionGit(t, root, "init", "--template=", "-q", "-b", "main")
			selectionGit(t, root, "config", "core.hooksPath", os.DevNull)
			ctx := context.Background()
			before, err := gitctx.NewGitContextAdapter().CurrentRepo(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			raw := "https://capture.example.invalid" + tc.path
			repoID := remotecfg.RepoIDFor(raw)
			if tc.apiOnly {
				repoID = before.ID
			}
			destination := delivcli.CapturePushDestination{URL: raw, APIOnly: tc.apiOnly}
			const credential = "synthetic-capture-credential"
			cfg := config{RepoRoot: root, GitDir: filepath.Join(root, ".git"), RemoteEndpoint: "https://capture.example.invalid/api/v1", RemoteToken: credential}
			if tc.savedToken {
				cfg.RemoteToken = ""
				if err := authcfg.Save("capture.example.invalid", credential); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate origin changing after the driver's hash check but before
			// factory invocation. Only the supplied observation has authority.
			if err := configFixtureSave(root, remotecfg.Remotes{"origin": "https://changed.example.invalid/team/other"}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CXT_REMOTE", "https://changed.example.invalid/api/v1")
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			calls, registrations := 0, 0
			http.DefaultTransport = capturePushRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme != "https" || r.URL.Host != "capture.example.invalid" || r.Header.Get("Authorization") != "Bearer "+credential {
					t.Fatal("request escaped frozen endpoint or credential")
				}
				if calls == 1 {
					if err := configFixtureSave(root, remotecfg.Remotes{"origin": "https://later.example.invalid/team/other"}); err != nil {
						t.Fatal(err)
					}
					if err := authcfg.Save("capture.example.invalid", "rotated-synthetic-credential"); err != nil {
						t.Fatal(err)
					}
					selectionGit(t, root, "config", "remote.origin.url", "https://code.example.invalid/replaced.git")
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repository-connections":
					if tc.apiOnly || calls != 1 || r.URL.Query().Get("remote_url") != raw {
						t.Fatal("API fallback interpreted as repository URL or destination re-resolved")
					}
					return capturePushJSON(t, r, 200, domain.RepositoryConnection{RepoID: repoID, RepositoryID: "synthetic-repository", RemoteURL: raw}), nil
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+repoID:
					return capturePushJSON(t, r, 200, domain.Repo{ID: repoID, DefaultBranch: "main", ContextProtocol: 1}), nil
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos":
					var got domain.Repo
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Fatal(err)
					}
					wantRemote := raw
					if tc.apiOnly {
						wantRemote = before.RemoteURL
					}
					if got.ID != repoID || got.RemoteURL != wantRemote || got.LocalPath != before.LocalPath || got.GitRemoteURL != before.GitRemoteURL {
						t.Fatal("registration reselected repository or Git evidence")
					}
					registrations++
					// Stop before object publication, after real bound registration.
					return capturePushJSON(t, r, 403, map[string]string{"code": "forbidden"}), nil
				default:
					t.Fatalf("unexpected bounded fixture route: %s %s", r.Method, r.URL.Path)
					return nil, errors.New("unexpected route")
				}
			})
			store := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "main", "")
			prepared, err := prepareCapturePush(cfg, store)(ctx, root, destination, repoID)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("factory did not perform exactly one identity check")
			}
			for _, appendMode := range []bool{false, true} {
				scope := domain.PublicationScope{Branches: []domain.PublicationBranch{{Branch: "main", BranchID: domain.LegacyContextBranchID(repoID, "main")}}}
				if _, err := prepared.Push(ctx, inbound.SyncInput{Cwd: root, RepoID: repoID, ForegroundOnly: true, Append: appendMode, Publication: &scope}); err == nil {
					t.Fatal("synthetic forbidden registration was ignored")
				}
			}
			if calls != 5 || registrations != 2 {
				t.Fatal("bound service did not reach both registration attempts", calls, registrations)
			}
		})
	}
}

func TestPrepareCapturePushRejectsDifferentRemoteIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	selectionGit(t, root, "init", "--template=", "-q", "-b", "main")
	before, err := gitctx.NewGitContextAdapter().CurrentRepo(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewFileStore(root)
	const raw = "https://capture.example.invalid/team/project"
	for _, apiOnly := range []bool{false, true} {
		original := http.DefaultTransport
		calls := 0
		http.DefaultTransport = capturePushRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != http.MethodGet {
				t.Fatal("identity mismatch allowed mutation")
			}
			if apiOnly {
				return capturePushJSON(t, r, 200, domain.Repo{ID: domain.HashContent([]byte("wrong")), DefaultBranch: "main"}), nil
			}
			return capturePushJSON(t, r, 200, domain.RepositoryConnection{RepoID: remotecfg.RepoIDFor(raw), RepositoryID: "synthetic", RemoteURL: raw}), nil
		})
		func() {
			defer func() { http.DefaultTransport = original }()
			destination := delivcli.CapturePushDestination{URL: raw}
			if apiOnly {
				destination = delivcli.CapturePushDestination{URL: "https://capture.example.invalid/api/v1", APIOnly: true}
			}
			prepared, err := prepareCapturePush(config{RepoRoot: root}, store)(context.Background(), root, destination, before.ID)
			if !errors.Is(err, domain.ErrHashMismatch) || prepared != nil || calls != 1 {
				t.Fatal("different identity accepted", err, calls)
			}
		}()
	}
}
