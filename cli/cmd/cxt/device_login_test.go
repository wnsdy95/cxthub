package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
)

func TestDeviceLoginRootManualLoginBeforeRepositoryConnection(test *testing.T) {
	root := test.TempDir()
	test.Chdir(root)
	test.Setenv("HOME", test.TempDir())
	test.Setenv("CXT_TOKEN", "")
	test.Setenv("CXT_REMOTE", "")
	test.Setenv("CXT_NO_BROWSER", "1")
	test.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	test.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/api/v1/me" || request.Header.Get("Authorization") != "Bearer root-fixture-token" {
			test.Error("production login used wrong transport")
		}
		_, _ = writer.Write([]byte(`{"username":"fixture"}`))
	}))
	defer server.Close()
	if err := run([]string{"cxt", "login", "root-fixture-token", "--server", server.URL}); err != nil {
		test.Fatal(err)
	}
	parsed, _ := url.Parse(server.URL)
	if requests.Load() != 1 || authcfg.Token(parsed.Host) != "root-fixture-token" {
		test.Fatal("production login did not verify and store scoped token", requests.Load())
	}
	info, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".cxt", "auth.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		test.Fatal("credential permissions changed", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".cxt")); !os.IsNotExist(err) {
		test.Fatal("standalone login created repository state", err)
	}
}

func TestDeviceLoginRootRejectsUnsafeTransportAndHidesFromReader(test *testing.T) {
	for _, base := range []string{"http://example.test/api/v1", "https://secret@example.test/api/v1", "https://example.test/api/v1?token=secret", "https://example.test/api/v1#fragment"} {
		if client, err := deviceLoginClient(base); client != nil || err == nil {
			test.Fatal("unsafe login transport accepted")
		}
	}
	read := buildReadContainer(config{RepoRoot: test.TempDir()}).clictr
	if read.DeviceLoginClient != nil || read.OpenLoginBrowser != nil {
		test.Fatal("read-only root exposes login effects")
	}
	write := buildContainer(config{RepoRoot: test.TempDir()}).clictr
	if write.DeviceLoginClient == nil || write.OpenLoginBrowser == nil {
		test.Fatal("write root did not compose device login")
	}
}
