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
	"github.com/wnsdy95/cxthub/backend/internal/adapters/webhookdelivery"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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
	lifecycle := newAPILifecycle(runtime.ProcessContext(ctx), runtime.Close)
	defer lifecycle.close()
	ctx = lifecycle.ctx
	st, svc, idSvc, publicURL := runtime.Store, runtime.Context, runtime.Identity, runtime.PublicURL
	if display, ok := st.(outbound.DocDisplayStore); ok {
		svc.ConfigureDocumentDisplay(display)
	}
	projectionRepos, err := pendingProjectionRepositories(os.Getenv("CXT_PENDING_PROJECTION_REPOS"))
	if err != nil {
		return err
	}
	if err := svc.ConfigurePendingProjections(ctx, projectionRepos); err != nil {
		return fmt.Errorf("configure pending projections: %w", err)
	}
	graphRepos, err := graphProjectionRepositories(os.Getenv("CXT_GRAPH_PROJECTION_REPOS"))
	if err != nil {
		return err
	}
	if err := svc.ConfigureGraphProjections(ctx, graphRepos); err != nil {
		return fmt.Errorf("configure graph projections: %w", err)
	}
	memoryRepos, err := projectionRepositories("CXT_MEMORY_PREPARATION_REPOS", os.Getenv("CXT_MEMORY_PREPARATION_REPOS"))
	if err != nil {
		return err
	}
	if err := svc.ConfigureMemoryReadPreparations(ctx, memoryRepos); err != nil {
		return fmt.Errorf("configure memory preparation: %w", err)
	}
	if err := svc.ConfigurePreparedMemoryDemands(memoryRepos); err != nil {
		return fmt.Errorf("configure memory preparation requests: %w", err)
	}
	shadowRepos, err := projectionRepositories("CXT_GRAPH_SHADOW_REPOS", os.Getenv("CXT_GRAPH_SHADOW_REPOS"))
	if err != nil {
		return err
	}
	if err := svc.ConfigureGraphQualifications(ctx, shadowRepos); err != nil {
		return fmt.Errorf("configure graph qualification: %w", err)
	}
	graphReads, err := graphReadRepositories(os.Getenv("CXT_GRAPH_READ_REPOS"))
	if err != nil {
		return err
	}
	if err := svc.ConfigureGraphReads(graphReads); err != nil {
		return fmt.Errorf("configure graph readers: %w", err)
	}
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
	if err := runtime.ConfigureDocumentQueries(); err != nil {
		return err
	}
	if err := runtime.ConfigureRepositoryQueries(); err != nil {
		return err
	}
	if err := runtime.ConfigurePreparedMemoryQueries(); err != nil {
		return err
	}
	if err := runtime.ConfigureMemorySnapshotQueries(); err != nil {
		return err
	}
	if err := runtime.ConfigureLiveMemoryQueries(); err != nil {
		return err
	}
	if err := runtime.ConfigureRepositoryAssetCommands(); err != nil {
		return err
	}
	if err := runtime.ConfigureDocumentPublicationCommands(); err != nil {
		return err
	}
	if err := runtime.ConfigureRepositoryRefCommands(); err != nil {
		return err
	}
	if err := runtime.ConfigureSnapshotGraftCommands(); err != nil {
		return err
	}
	if err := runtime.ConfigureMemoryDigestCommands(); err != nil {
		return err
	}
	if err := runtime.ConfigureHistoryCommands(); err != nil {
		return err
	}
	if err := runtime.ConfigurePRPromotionCommands(); err != nil {
		return err
	}
	if err := runtime.ConfigureContextBranchOperations(); err != nil {
		return err
	}
	api := delivery.NewServer(svc, idSvc)
	if err := api.SetContextBranchOperations(runtime.ContextBranches); err != nil {
		return err
	}
	if err := api.SetPRPromotionCommands(runtime.PRPromotions); err != nil {
		return err
	}
	if err := api.SetHistoryCommands(runtime.HistoryCommands); err != nil {
		return err
	}
	if err := api.SetRepositoryRefCommands(runtime.RepositoryRefs); err != nil {
		return err
	}
	if err := api.SetSnapshotGraftCommands(runtime.SnapshotGrafts); err != nil {
		return err
	}
	if err := api.SetMemoryDigestCommands(runtime.MemoryDigests); err != nil {
		return err
	}
	if err := api.SetDocumentPublicationCommands(runtime.DocumentPublications); err != nil {
		return err
	}
	if err := api.SetRepositoryAssetCommands(runtime.RepositoryAssets); err != nil {
		return err
	}
	if err := api.SetDocumentQueries(runtime.DocumentQueries, runtime.DocumentQueryProfile); err != nil {
		return err
	}
	if err := api.SetRepositoryStateQueries(runtime.RepositoryStates, runtime.RepositoryStateProfile); err != nil {
		return err
	}
	if err := api.SetRepositoryGraphQueries(runtime.RepositoryGraphs, runtime.RepositoryGraphProfile); err != nil {
		return err
	}
	if err := api.SetContextQuery(runtime.ContextQueries, runtime.ContextQueryProfile); err != nil {
		return err
	}
	api.SetDeviceVerificationOrigin(publicURL)
	if githubConnections != nil {
		returnURL, _ := githubReturnURL(publicURL) // validated during configuration
		api.SetGitHubConnections(githubConnections, os.Getenv("CXT_GITHUB_APP_WEBHOOK_SECRET"), returnURL)
	}
	api.SetGitScans(scans)
	api.SetGitSyncAudit(app.NewGitSyncAudit(svc, gitReader))
	api.SetCodeApplicability(svc)
	if err := api.SetLiveMemoryQueries(runtime.MemoryLive); err != nil {
		return err
	}
	if err := api.SetPreparedMemory(runtime.PreparedMemory); err != nil {
		return err
	}
	if err := api.SetMemorySnapshotQueries(runtime.MemorySnapshots); err != nil {
		return err
	}
	api.SetGitChanges(changes)

	if err := configureInvitationEmail(idSvc, addr, publicURL); err != nil {
		return err
	}
	handler, err := withMemoryPreparation(runtime, api.Handler(), os.Getenv("CXT_MCP_PUBLIC_URL"), memoryRepos)
	if err != nil {
		return fmt.Errorf("configure MCP memory preparation: %w", err)
	}
	handler, err = withConversationFindPreparation(runtime, handler, os.Getenv("CXT_MCP_PUBLIC_URL"))
	if err != nil {
		return fmt.Errorf("configure MCP conversation preparation: %w", err)
	}
	fmt.Fprintf(os.Stderr, "cxtd: listening on %s (auth=%s)\n", addr, runtime.AuthMode)
	if !runtime.ExternalWorkers() {
		projectionWorkers, err := svc.ProjectionWorkersForProfile(projectionworker.Complete)
		if err != nil {
			return fmt.Errorf("configure projection workers: %w", err)
		}
		notificationWorker, err := app.NewNotificationWorker(app.NotificationWorkerConfig{
			Profile: notificationworker.Complete, Metadata: st, Blobs: st, Repositories: st, Sender: webhookdelivery.New(),
		})
		if err != nil {
			return fmt.Errorf("configure notification worker: %w", err)
		}
		maintenanceProfile := systemmaintenance.Compatibility
		if _, production := st.(outbound.ProductionStore); production {
			maintenanceProfile = systemmaintenance.Complete
		}
		maintenanceWorkers, err := app.NewMaintenanceWorkers(app.MaintenanceWorkersConfig{Profile: maintenanceProfile, Repositories: st})
		if err != nil {
			return fmt.Errorf("configure maintenance workers: %w", err)
		}
		gitChangeProfile := gitchangeworker.Compatibility
		if _, production := st.(outbound.ProductionStore); production {
			gitChangeProfile = gitchangeworker.Complete
		}
		var gitSourceAuthorizer outbound.GitSourceAuthorizer
		if githubConnections != nil {
			gitSourceAuthorizer = githubConnections
		}
		gitChangeWorker, err := app.NewGitChangeWorker(app.GitChangeWorkerConfig{
			Profile: gitChangeProfile, Metadata: st, Blobs: st, Repositories: st, Reader: gitReader, Authorizer: gitSourceAuthorizer,
		})
		if err != nil {
			return fmt.Errorf("configure Git change worker: %w", err)
		}
		gitIntegrationProfile := app.GitIntegrationCompatibility
		if _, production := st.(outbound.ProductionStore); production {
			gitIntegrationProfile = app.GitIntegrationComplete
		}
		gitIntegrationWorkers, err := app.NewGitIntegrationWorkers(app.GitIntegrationWorkerConfig{
			Profile: gitIntegrationProfile, Metadata: st, Blobs: st, Repositories: st, Reader: gitReader, Authorizer: gitSourceAuthorizer,
		})
		if err != nil {
			return fmt.Errorf("configure Git integration workers: %w", err)
		}
		memoryPreparationProfile := app.MemoryPreparationCompatibility
		if _, production := st.(outbound.ProductionStore); production {
			memoryPreparationProfile = app.MemoryPreparationComplete
		}
		memoryPreparations, err := app.NewMemoryPreparationWorkers(app.MemoryPreparationWorkerConfig{
			Profile: memoryPreparationProfile, Resources: svc.MemoryPreparationResources(), Repositories: memoryRepos,
		})
		if err != nil {
			return fmt.Errorf("configure memory preparation workers: %w", err)
		}
		prPromotionProfile := app.PRPromotionCompatibility
		if _, production := st.(outbound.ProductionStore); production {
			prPromotionProfile = app.PRPromotionComplete
		}
		prPromotionWorker, err := app.NewPRPromotionWorker(svc.PRPromotionWorkerConfig(prPromotionProfile))
		if err != nil {
			return fmt.Errorf("configure PR promotion worker: %w", err)
		}
		invitationEmailProfile := app.InvitationEmailCompatibility
		if _, production := st.(outbound.ProductionStore); production {
			invitationEmailProfile = app.InvitationEmailComplete
		}
		invitationEmailWorker, err := app.NewInvitationEmailWorker(idSvc.InvitationEmailWorkerConfig(invitationEmailProfile))
		if err != nil {
			return fmt.Errorf("configure invitation email worker: %w", err)
		}
		var githubWorker *app.GitHubWorker
		if githubConnections != nil {
			githubWorkerProfile := app.GitHubWorkerCompatibility
			if _, production := st.(outbound.ProductionStore); production {
				githubWorkerProfile = app.GitHubWorkerComplete
			}
			githubWorker, err = app.NewGitHubWorker(githubConnections.GitHubWorkerConfig(githubWorkerProfile))
			if err != nil {
				return fmt.Errorf("configure GitHub worker: %w", err)
			}
		}
		if githubConnections != nil {
			lifecycle.start(githubWorker.Run)
		}
		lifecycle.start(gitChangeWorker.Run)
		lifecycle.start(gitIntegrationWorkers.Run)
		lifecycle.start(gitIntegrationWorkers.RunReconciler)
		if !runtime.BoundedDocuments() {
			documentWorkers, err := svc.DocumentWorkersForProfile(runtime.DocumentQueryProfile)
			if err != nil {
				return err
			}
			lifecycle.start(documentWorkers.Finalization.Run)
			if documentWorkers.Range != nil {
				lifecycle.start(documentWorkers.Range.Run)
			}
			if documentWorkers.FindFields != nil {
				lifecycle.start(documentWorkers.FindFields.Run)
			}
			if documentWorkers.Display != nil {
				lifecycle.start(documentWorkers.Display.Run)
			}
			if maintenance, ok := st.(outbound.DocDisplayMaintenanceStore); ok {
				worker, err := app.NewDisplayMaintenanceWorker(maintenance)
				if err != nil {
					return err
				}
				lifecycle.start(worker.Run)
			}
		}
		if projectionWorkers.Pending != nil {
			lifecycle.start(projectionWorkers.Pending.Run)
		}
		if projectionWorkers.Graph != nil {
			lifecycle.start(projectionWorkers.Graph.Run)
		}
		lifecycle.start(memoryPreparations.Run)
		if projectionWorkers.Qualification != nil {
			lifecycle.start(projectionWorkers.Qualification.Run)
		}
		lifecycle.start(prPromotionWorker.Run)
		lifecycle.start(notificationWorker.Run)
		lifecycle.start(invitationEmailWorker.Run)
		lifecycle.start(maintenanceWorkers.Runtime.Run)
		lifecycle.start(maintenanceWorkers.Storage.Run)
	}
	return serverruntime.Serve(ctx, addr, runtime.HealthHandler(handler))
}

func envBool(value string) bool { return serverruntime.EnvBool(value) }

func pendingProjectionRepositories(value string) ([]domain.ContentHash, error) {
	return projectionRepositories("CXT_PENDING_PROJECTION_REPOS", value)
}

func graphProjectionRepositories(value string) ([]domain.ContentHash, error) {
	return projectionRepositories("CXT_GRAPH_PROJECTION_REPOS", value)
}

func graphReadRepositories(value string) ([]domain.ContentHash, error) {
	return projectionRepositories("CXT_GRAPH_READ_REPOS", value)
}

func projectionRepositories(variable, value string) ([]domain.ContentHash, error) {
	return serverruntime.WorkerRepositories(variable, value)
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

// index is a restartable maintenance job for existing archives; new writes index
// themselves. Production uses the same PostgreSQL DSN and migration contract.
func buildReadIndexes(ctx context.Context, args []string) error {
	dsn := os.Getenv("CXT_POSTGRES_DSN")
	st, err := store.Open(flagOr(args, "--data", os.Getenv("CXT_DATA"), "./cxt-data"), dsn, envBool(os.Getenv("CXT_REQUIRE_POSTGRES")))
	if err != nil {
		return err
	}
	if closer, ok := st.(interface{ Close() }); ok {
		defer closer.Close()
	}
	if dsn != "" {
		mdir := flagOr(args, "--migrations", os.Getenv("CXT_MIGRATIONS_DIR"), "./schemas/db/migrations")
		if err := st.VerifyMigrations(ctx, mdir); err != nil {
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
