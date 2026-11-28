//go:build postgres

package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	mcpserver "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

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
	svc := app.NewService(st, st, st)
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
