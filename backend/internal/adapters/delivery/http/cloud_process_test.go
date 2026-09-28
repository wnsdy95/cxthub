//go:build postgres

package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	mcpserver "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

type cloudTestServer struct {
	URL   string
	Close func()
}

// Separate OS processes share only PostgreSQL: no process mutex or Go cache can
// accidentally satisfy the distributed-state assertions in the load contract.
func TestCloudLoadProcess(t *testing.T) {
	ready := os.Getenv("CXT_LOAD_PROCESS_READY")
	if ready == "" {
		t.Skip("child only")
	}
	st, err := store.NewPostgresStore(context.Background(), os.Getenv("CXT_LOAD_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := app.NewService(st, st, nil, gitengine.NewEngine(st), st)
	id := app.NewIdentityService(auth.NewDevVerifier(), st)
	var handler http.Handler
	if os.Getenv("CXT_LOAD_PROCESS_KIND") == "mcp" {
		m, err := mcpserver.NewServer(svc, id, st, "https://load.example.test")
		if err != nil {
			t.Fatal(err)
		}
		handler = m.Handler()
	} else {
		handler = NewServer(svc, id).Handler()
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	if err = os.WriteFile(ready, []byte(server.URL), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}
func startCloudTestProcess(t *testing.T, dsn string) cloudTestServer {
	// The local proxy models the external API/MCP routing boundary. Each
	// upstream owns a separate OS process, pool and disposable read caches.
	api := startCloudTestService(t, dsn, "api")
	mcp := startCloudTestService(t, dsn, "mcp")
	apiURL, _ := url.Parse(api.URL)
	mcpURL, _ := url.Parse(mcp.URL)
	apiProxy := httputil.NewSingleHostReverseProxy(apiURL)
	mcpProxy := httputil.NewSingleHostReverseProxy(mcpURL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			mcpProxy.ServeHTTP(w, r)
		} else {
			apiProxy.ServeHTTP(w, r)
		}
	}))
	var once sync.Once
	stop := func() { once.Do(func() { proxy.Close(); api.Close(); mcp.Close() }) }
	t.Cleanup(stop)
	return cloudTestServer{URL: proxy.URL, Close: stop}
}

func startCloudTestService(t *testing.T, dsn, kind string) cloudTestServer {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(exe, "-test.run=^TestCloudLoadProcess$", "-test.timeout=15m")
	command.Env = append(os.Environ(), "CXT_LOAD_DSN="+dsn, "CXT_LOAD_PROCESS_READY="+ready, "CXT_LOAD_PROCESS_KIND="+kind)
	log, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = log, log
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = input.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				<-done
			}
			_ = log.Close()
		})
	}
	t.Cleanup(stop)
	deadline := time.After(20 * time.Second)
	for {
		if raw, err := os.ReadFile(ready); err == nil {
			return cloudTestServer{URL: string(raw), Close: stop}
		}
		select {
		case err := <-done:
			raw, _ := os.ReadFile(filepath.Join(dir, "server.log"))
			once.Do(func() { _ = input.Close(); _ = log.Close() })
			t.Fatal(fmt.Sprintf("server exited: %v %s", err, raw))
		case <-deadline:
			t.Fatal("server readiness timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
