// Package serverruntime assembles the shared database and identity dependencies
// for independently deployed API and MCP processes. It starts no workers.
package serverruntime

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Runtime struct {
	Store     store.Store
	Context   *app.Service
	Identity  *app.IdentityService
	PublicURL string
	AuthMode  string
}

func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func EnvBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// PublicOrigin is the browser-facing Vercel origin, never a Render service URL.
// Keeping it identical on both services preserves cookies, OAuth issuer and
// resource identity, and consent CSRF validation across the routing boundary.
func PublicOrigin(addr, configured string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(configured), "/")
	if value == "" && IsLoopback(addr) {
		value = "http://" + addr
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("CXT_PUBLIC_URL must be an absolute origin without credentials, path, query or fragment")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = loopback || ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback && IsLoopback(addr)) {
		return "", fmt.Errorf("CXT_PUBLIC_URL requires HTTPS (HTTP is allowed only for loopback development)")
	}
	return u.String(), nil
}

func Open(ctx context.Context, addr, dataDir string, requirePostgres bool) (_ *Runtime, err error) {
	var verifier outbound.IdentityVerifier
	mode := strings.TrimSpace(os.Getenv("CXT_AUTH"))
	switch mode {
	case "firebase":
		project := strings.TrimSpace(os.Getenv("CXT_FIREBASE_PROJECT"))
		if project == "" {
			return nil, fmt.Errorf("CXT_FIREBASE_PROJECT is required for Firebase authentication")
		}
		verifier = auth.NewFirebaseVerifier(project)
	case "", "dev":
		if !IsLoopback(addr) && mode != "dev" {
			return nil, fmt.Errorf("refusing to bind dev authentication to an external address; configure CXT_AUTH=firebase or explicitly opt in with CXT_AUTH=dev")
		}
		verifier = auth.NewDevVerifier()
		mode = "dev"
		log.Print("warning: development authentication trusts unverified tokens")
	default:
		return nil, fmt.Errorf("CXT_AUTH must be firebase or dev")
	}
	publicURL, err := PublicOrigin(addr, os.Getenv("CXT_PUBLIC_URL"))
	if err != nil {
		return nil, err
	}
	dsn := strings.TrimSpace(os.Getenv("CXT_POSTGRES_DSN"))
	st, err := store.Open(dataDir, dsn, requirePostgres)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	r := &Runtime{Store: st, PublicURL: publicURL, AuthMode: mode}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	if requirePostgres || dsn != "" {
		if err = app.ValidateProductionStore(st); err != nil {
			return nil, err
		}
		mdir := os.Getenv("CXT_MIGRATIONS_DIR")
		if mdir == "" {
			mdir = "schemas/db/migrations"
		}
		// PostgreSQL's migration advisory lock serializes concurrent startups.
		var n int
		n, err = st.ApplyMigrations(ctx, mdir)
		if err != nil {
			return nil, fmt.Errorf("apply migrations (check CXT_MIGRATIONS_DIR): %w", err)
		}
		log.Printf("migrations: applied %d", n)
	}
	r.Context = app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	r.Identity = app.NewIdentityService(verifier, st)
	return r, nil
}

func (r *Runtime) Close() {
	if closer, ok := r.Store.(interface{ Close() }); ok {
		closer.Close()
	}
}

// HealthHandler exposes readiness on each independent service. It probes that
// process's pool, rather than declaring a disconnected database healthy.
func (r *Runtime) HealthHandler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if p, ok := r.Store.(interface{ Ping(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
			defer cancel()
			if p.Ping(ctx) != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/", next)
	return mux
}

func Serve(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(shutdown) != nil {
			_ = srv.Close()
		}
	}()
	err := srv.ListenAndServe()
	close(done)
	<-stopped
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
