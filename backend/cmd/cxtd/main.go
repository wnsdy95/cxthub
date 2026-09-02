// Command cxtd is the central cxt server binary (module boundary: Go, container).
//
// 'serve' starts the HTTP REST server; 'repack' performs offline-compatible FS
// object maintenance. Frontend static assets are not served (CDN/Vercel).
//
// Store: Default build uses FSStore (file-based, --data directory). `go build -tags postgres` + CXT_POSTGRES_DSN
// for PostgreSQL adapter (pgx). store.Open(factory) selects based on build tags.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// isLoopback determines if the bind address is limited to loopback (127.0.0.1/localhost/::1).
// Omitting the host (e.g., ":8907") means binding to all interfaces, so it returns false.
func isLoopback(addr string) bool {
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

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "repack" {
		// Maintenance: repack legacy FS-store transcript and memory objects into
		// their chunk CAS forms (lossless and idempotent).
		// It is safe alongside a live server because replacement is atomic and newly created chunks receive a sweep grace period.
		dataDir := flagOr(os.Args[2:], "--data", os.Getenv("CXT_DATA"), "./cxt-data")
		fs, err := store.OpenFSStore(dataDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cxtd repack:", err)
			os.Exit(1)
		}
		n, saved, err := fs.RepackObjects()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cxtd repack:", err)
			os.Exit(1)
		}
		fmt.Printf("repacked %d object(s), reclaimed %.1f MB\n", n, float64(saved)/1e6)
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: cxtd serve [--addr :8080] [--data ./cxt-data] | cxtd repack [--data ./cxt-data]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "cxtd:", err)
		os.Exit(1)
	}
}

// serve wires up the adapter and starts a REST server with graceful shutdown.
func serve(ctx context.Context, args []string) error {
	addr := flagOr(args, "--addr", os.Getenv("CXT_ADDR"), ":8080")
	dataDir := flagOr(args, "--data", os.Getenv("CXT_DATA"), "./cxt-data")
	dsn := os.Getenv("CXT_POSTGRES_DSN")

	// Resolve and validate authentication before opening either storage backend.
	// In particular, an unsafe dev-auth bind must fail without touching the FS
	// store or running PostgreSQL migrations first.
	var verifier outbound.IdentityVerifier
	authMode := "dev"
	if os.Getenv("CXT_AUTH") == "firebase" && os.Getenv("CXT_FIREBASE_PROJECT") != "" {
		verifier = auth.NewFirebaseVerifier(os.Getenv("CXT_FIREBASE_PROJECT"))
		authMode = "firebase:" + os.Getenv("CXT_FIREBASE_PROJECT")
	} else {
		// Dev validator accepts any token — a safety measure to prevent accidental external exposure:
		// to bind to an address other than loopback, explicitly start with CXT_AUTH=dev.
		if !isLoopback(addr) && os.Getenv("CXT_AUTH") != "dev" {
			log.Fatalf("refusing to bind dev authentication to external address %s — "+
				"set CXT_AUTH=firebase and CXT_FIREBASE_PROJECT, or explicitly opt in with CXT_AUTH=dev", addr)
		}
		verifier = auth.NewDevVerifier()
		log.Printf("warning: dev authentication trusts every token without verification (local development only)")
	}

	// Outbound adapter: store.Open returns FSStore or PostgresStore according to build tags and DSN.
	st, err := store.Open(dataDir, dsn)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	// Automatic PostgreSQL schema migration is idempotent through schema_migrations history (FS is a no-op).
	// Default search: CXT_MIGRATIONS_DIR > ./schemas/db/migrations (if exists).
	if dsn != "" {
		mdir := os.Getenv("CXT_MIGRATIONS_DIR")
		if mdir == "" {
			if _, serr := os.Stat("schemas/db/migrations"); serr == nil {
				mdir = "schemas/db/migrations"
			}
		}
		if mdir != "" {
			n, merr := st.ApplyMigrations(context.Background(), mdir)
			if merr != nil {
				return fmt.Errorf("apply migrations: %w", merr)
			}
			log.Printf("migrations: applied %d (%s)", n, mdir)
		} else {
			log.Printf("warning: migration directory not found — set CXT_MIGRATIONS_DIR")
		}
	}
	engine := gitengine.NewEngine(st) // GitEngine computes DAG reachability from parent metadata.
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), engine, st)

	idSvc := app.NewIdentityService(verifier, st)

	api := delivery.NewServer(svc, idSvc)

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	backend := "fs"
	if dsn != "" {
		backend = "postgres"
	}
	fmt.Fprintf(os.Stderr, "cxtd: listening on %s (store=%s, auth=%s, data=%s)\n", addr, backend, authMode, dataDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// flagOr selects the first non-empty value from args --name, envVal, or def.
func flagOr(args []string, name, envVal, def string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	if envVal != "" {
		return envVal
	}
	return def
}
