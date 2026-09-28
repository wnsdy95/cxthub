// Command cxtd is the central cxt server binary (module boundary: Go, container).
//
// 'serve' starts the HTTP REST server; 'repack' performs offline-compatible FS
// object maintenance. Frontend static assets are not served (CDN/Vercel).
//
// Store: a loopback-bound development build may use FSStore. Every externally
// bound server requires the PostgreSQL build and CXT_POSTGRES_DSN; production
// can additionally set CXT_REQUIRE_POSTGRES=1 as a fail-closed assertion.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	delivery "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/http"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/federation"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitevidence"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

// isLoopback determines if the bind address is limited to loopback (127.0.0.1/localhost/::1).
// Omitting the host (e.g., ":8907") means binding to all interfaces, so it returns false.
func isLoopback(addr string) bool { return serverruntime.IsLoopback(addr) }

func postgresRequired(addr, configured string) bool {
	return envBool(configured) || !isLoopback(addr)
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "index" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := buildReadIndexes(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "cxtd index:", err)
			os.Exit(1)
		}
		return
	}
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
		fmt.Fprintln(os.Stderr, "usage: cxtd serve [--addr :8080] [--data ./cxt-data] | cxtd repack [--data ./cxt-data] | cxtd index [--data ./cxt-data] [--migrations ./schemas/db/migrations]")
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
	if err := loadServerEnv(); err != nil {
		return err
	}
	addr := flagOr(args, "--addr", os.Getenv("CXT_ADDR"), ":8080")
	dataDir := flagOr(args, "--data", os.Getenv("CXT_DATA"), "./cxt-data")
	runtime, err := serverruntime.Open(ctx, addr, dataDir, postgresRequired(addr, os.Getenv("CXT_REQUIRE_POSTGRES")))
	if err != nil {
		return err
	}
	defer runtime.Close()
	st, svc, idSvc, publicURL := runtime.Store, runtime.Context, runtime.Identity, runtime.PublicURL
	vault, err := federation.ConfiguredVault(os.Getenv("CXT_IDENTITY_ENCRYPTION_KEY"), os.Getenv("CXT_IDENTITY_ENCRYPTION_KEYRING"))
	if err != nil {
		return fmt.Errorf("invalid identity encryption key configuration")
	}
	if vault != nil {
		idSvc.WithOIDC(federation.NewOIDC(), vault, publicURL)
		idSvc.WithSAML(federation.NewSAML(), vault, publicURL)
	}
	githubConnections, githubClient, err := configureGitHub(idSvc, svc, st, publicURL)
	if err != nil {
		return err
	}

	gitReader := gitevidence.NewGitHub(func() string { return os.Getenv("CXT_GITHUB_TOKEN") })
	if githubConnections != nil {
		gitReader = gitevidence.NewAuthenticated(func(ctx context.Context, path string) (string, string, string, error) {
			id, resolved, e := githubConnections.EvidenceInstallation(ctx, path)
			if e != nil {
				return "", "", "", e
			}
			if id == 0 {
				return os.Getenv("CXT_GITHUB_TOKEN"), "operator", resolved, nil
			}
			token, e := githubClient.InstallationToken(ctx, id)
			return token, fmt.Sprintf("installation:%d", id), resolved, e
		})
	}

	changes, err := app.NewGitChanges(svc, gitReader)
	if err != nil {
		return err
	}
	scans, err := app.NewGitScans(svc, gitReader)
	if err != nil {
		return err
	}
	if githubConnections != nil {
		changes.SetSourceAuthorizer(githubConnections)
		scans.SetSourceAuthorizer(githubConnections)
	}
	api := delivery.NewServer(svc, idSvc)
	if githubConnections != nil {
		returnURL, _ := githubReturnURL(publicURL) // validated during configuration
		api.SetGitHubConnections(githubConnections, os.Getenv("CXT_GITHUB_APP_WEBHOOK_SECRET"), returnURL)
	}
	api.SetGitScans(scans)
	api.SetGitSyncAudit(app.NewGitSyncAudit(svc, gitReader))
	api.SetCodeApplicability(svc)
	api.SetEffectiveMemory(svc)
	api.SetMemoryPositions(svc)
	api.SetGitChanges(changes)

	if err := configureInvitationEmail(idSvc, addr, publicURL); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cxtd: listening on %s (auth=%s)\n", addr, runtime.AuthMode)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if githubConnections != nil {
		go githubConnections.Run(ctx)
	}
	go changes.Run(ctx)
	go scans.Run(ctx)
	go scans.RunReconciler(ctx)
	go svc.RunDocFinalizationWorker(ctx)
	go svc.RunPRPromotionWorker(ctx)
	go svc.RunNotificationWorker(ctx)
	go idSvc.RunInvitationEmailWorker(ctx)
	go idSvc.RunRuntimeMaintenance(ctx)
	go idSvc.RunStorageMaintenance(ctx)
	return serverruntime.Serve(ctx, addr, runtime.HealthHandler(api.Handler()))
}

func envBool(value string) bool { return serverruntime.EnvBool(value) }

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

// index is a restartable maintenance job for existing archives; new writes index
// themselves. Production uses the same PostgreSQL DSN and migration contract.
func buildReadIndexes(ctx context.Context, args []string) error {
	dsn := os.Getenv("CXT_POSTGRES_DSN")
	st, err := store.Open(flagOr(args, "--data", os.Getenv("CXT_DATA"), "./cxt-data"), dsn, envBool(os.Getenv("CXT_REQUIRE_POSTGRES")))
	if err != nil {
		return err
	}
	if dsn != "" {
		mdir := flagOr(args, "--migrations", os.Getenv("CXT_MIGRATIONS_DIR"), "./schemas/db/migrations")
		if _, err := st.ApplyMigrations(ctx, mdir); err != nil {
			return err
		}
	}
	indexer, ok := st.(interface {
		BackfillReadIndexes(context.Context, func(int)) error
	})
	if !ok {
		return fmt.Errorf("read index maintenance unavailable")
	}
	n := 0
	err = indexer.BackfillReadIndexes(ctx, func(done int) {
		n = done
		if n%25 == 0 {
			log.Printf("read indexes: %d documents checked", n)
		}
	})
	if err == nil {
		log.Printf("read indexes ready: %d documents checked", n)
	}
	return err
}
