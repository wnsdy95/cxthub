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
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Runtime struct {
	Store                  store.Store
	Context                *app.Service
	DocumentEvents         inbound.DocumentEventQueries
	DocumentQueries        inbound.DocumentQueries
	DocumentQueryProfile   inbound.DocumentQueryProfile
	DocumentIdentities     []domain.DocumentIdentity
	RepositoryAssets       inbound.RepositoryAssetCommands
	DocumentPublications   inbound.DocumentPublicationCommands
	RepositoryRefs         inbound.RepositoryRefCommands
	SnapshotGrafts         app.SnapshotGraftWriter
	MemoryDigests          app.MemoryDigestWriter
	HistoryCommands        inbound.HistoryCommands
	PRPromotions           inbound.PRPromotionCommands
	ContextBranches        inbound.ContextBranchOperations
	RepositoryStates       inbound.RepositoryStateQueries
	RepositoryStateProfile inbound.RepositoryStateQueryProfile
	RepositoryGraphs       inbound.RepositoryGraphQueries
	RepositoryGraphProfile inbound.RepositoryGraphQueryProfile
	ContextQueries         inbound.ContextQuery
	ContextQueryProfile    inbound.ContextQueryProfile
	PreparedMemory         inbound.PreparedMemoryReader
	MemorySnapshots        inbound.MemorySnapshotQueries
	MemoryLive             inbound.LiveMemoryQueries
	Archive                inbound.RepositoryArchiveQueries
	GitChanges             inbound.GitChangeQuery
	GitScans               inbound.GitScanQuery
	CodeApplicability      inbound.CodeApplicabilityQuery
	ConversationFind       inbound.ConversationFindQuery
	Identity               *app.IdentityService
	PublicURL              string
	AuthMode               string
	infrastructure         *Infrastructure
	execution              *executionProfile
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
	return openRuntime(ctx, addr, dataDir, requirePostgres, false)
}

func OpenMCP(ctx context.Context, addr, dataDir string, requirePostgres bool) (_ *Runtime, err error) {
	return openRuntime(ctx, addr, dataDir, requirePostgres, true)
}

func openRuntime(ctx context.Context, addr, dataDir string, requirePostgres, queriesOnly bool) (_ *Runtime, err error) {
	preparedRepos, err := preparedMemoryRepositories(os.Getenv("CXT_MEMORY_READ_REPOS"))
	if err != nil {
		return nil, err
	}
	rootPublication := EnvBool(os.Getenv("CXT_CONVERSATION_ROOT_PUBLICATION"))
	if err := app.ValidateConversationRootPublicationConfig(rootPublication); err != nil {
		return nil, err
	}
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
	role := outbound.ProcessAPI
	if queriesOnly {
		role = outbound.ProcessMCP
	}
	execution, err := executionProfileFromEnvironment(role, false)
	if err != nil {
		return nil, err
	}
	infrastructure, err := OpenInfrastructure(ctx, InfrastructureConfig{
		DSN:                          os.Getenv("CXT_POSTGRES_DSN"),
		DataDir:                      dataDir,
		RequirePostgres:              requirePostgres,
		MigrationsDirectory:          os.Getenv("CXT_MIGRATIONS_DIR"),
		LegacyByteRangePublication:   EnvBool(os.Getenv("CXT_LEGACY_BYTE_RANGE_PUBLICATION")),
		LegacyDerivedBytePublication: EnvBool(os.Getenv("CXT_LEGACY_DERIVED_BYTE_PUBLICATION")),
		execution:                    execution,
	})
	if err != nil {
		return nil, err
	}
	st := infrastructure.Store
	profile := infrastructure.DocumentProfile
	r := &Runtime{Store: st, PublicURL: publicURL, AuthMode: mode, DocumentQueryProfile: profile, infrastructure: infrastructure, execution: execution}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	r.Identity = app.NewIdentityService(verifier, st).WithDomainResolver(net.DefaultResolver)
	if queriesOnly {
		if err = r.configureMCPReads(profile, preparedRepos, rootPublication); err != nil {
			return nil, err
		}
		return r, nil
	}
	r.Context = app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	if display, ok := st.(outbound.DocDisplayReader); ok {
		r.Context.ConfigureDocumentDisplayReads(display)
	}
	if fields, ok := st.(outbound.DocFindFieldsStore); ok {
		r.Context.ConfigureDocumentFindFields(fields)
	}
	if err = r.Context.ConfigurePreparedMemoryReads(preparedRepos); err != nil {
		return nil, fmt.Errorf("configure prepared memory reads: %w", err)
	}
	if err = r.Context.ConfigureConversationRootPublication(rootPublication); err != nil {
		return nil, err
	}
	if err = r.ConfigureDocumentQueries(); err != nil {
		return nil, err
	}
	if err = r.ConfigureRepositoryQueries(); err != nil {
		return nil, err
	}
	if err = r.ConfigurePreparedMemoryQueries(); err != nil {
		return nil, err
	}
	if err = r.ConfigureMemorySnapshotQueries(); err != nil {
		return nil, err
	}
	if err = r.ConfigureLiveMemoryQueries(); err != nil {
		return nil, err
	}
	if err = r.ConfigureRepositoryAssetCommands(); err != nil {
		return nil, err
	}
	if err = r.ConfigureDocumentPublicationCommands(); err != nil {
		return nil, err
	}
	if err = r.ConfigureRepositoryRefCommands(); err != nil {
		return nil, err
	}
	if err = r.ConfigureSnapshotGraftCommands(); err != nil {
		return nil, err
	}
	if err = r.ConfigureMemoryDigestCommands(); err != nil {
		return nil, err
	}
	if err = r.ConfigureHistoryCommands(); err != nil {
		return nil, err
	}
	if err = r.ConfigurePRPromotionCommands(); err != nil {
		return nil, err
	}
	if err = r.ConfigureContextBranchOperations(); err != nil {
		return nil, err
	}
	return r, nil
}

func (runtime *Runtime) ConfigureContextBranchOperations() error {
	if runtime == nil {
		return fmt.Errorf("configure context branch operations: %w", domain.ErrCatalogUnsupported)
	}
	runtime.ContextBranches = nil
	if runtime.Context == nil {
		return fmt.Errorf("configure context branch operations: %w", domain.ErrCatalogUnsupported)
	}
	operations, err := runtime.Context.ContextBranchOperations()
	if err != nil {
		return fmt.Errorf("configure context branch operations: %w", err)
	}
	runtime.ContextBranches = operations
	return nil
}

func (runtime *Runtime) ConfigurePRPromotionCommands() error {
	if runtime == nil {
		return fmt.Errorf("configure PR promotion commands: %w", domain.ErrCatalogUnsupported)
	}
	runtime.PRPromotions = nil
	if runtime.Context == nil {
		return fmt.Errorf("configure PR promotion commands: %w", domain.ErrCatalogUnsupported)
	}
	commands, err := runtime.Context.PRPromotionCommands()
	if err != nil {
		return fmt.Errorf("configure PR promotion commands: %w", err)
	}
	runtime.PRPromotions = commands
	return nil
}

func (runtime *Runtime) ConfigureHistoryCommands() error {
	if runtime == nil {
		return fmt.Errorf("configure history commands: %w", domain.ErrCatalogUnsupported)
	}
	runtime.HistoryCommands = nil
	if runtime.Context == nil {
		return fmt.Errorf("configure history commands: %w", domain.ErrCatalogUnsupported)
	}
	commands, err := runtime.Context.HistoryCommands()
	if err != nil {
		return fmt.Errorf("configure history commands: %w", err)
	}
	runtime.HistoryCommands = commands
	return nil
}

func (runtime *Runtime) ConfigureRepositoryRefCommands() error {
	if runtime == nil {
		return fmt.Errorf("configure repository ref commands: %w", domain.ErrCatalogUnsupported)
	}
	runtime.RepositoryRefs = nil
	if runtime.Context == nil {
		return fmt.Errorf("configure repository ref commands: %w", domain.ErrCatalogUnsupported)
	}
	commands, err := runtime.Context.RepositoryRefCommands()
	if err != nil {
		return fmt.Errorf("configure repository ref commands: %w", err)
	}
	runtime.RepositoryRefs = commands
	return nil
}

func (runtime *Runtime) ConfigureSnapshotGraftCommands() error {
	if runtime == nil {
		return fmt.Errorf("configure snapshot graft commands: %w", domain.ErrCatalogUnsupported)
	}
	runtime.SnapshotGrafts = nil
	if runtime.Context == nil {
		return fmt.Errorf("configure snapshot graft commands: %w", domain.ErrCatalogUnsupported)
	}
	commands, err := runtime.Context.SnapshotGraftCommands()
	if err != nil {
		return fmt.Errorf("configure snapshot graft commands: %w", err)
	}
	runtime.SnapshotGrafts = commands
	return nil
}

func (runtime *Runtime) ConfigureDocumentPublicationCommands() error {
	if runtime == nil {
		return fmt.Errorf("configure document publication commands: %w", domain.ErrCatalogUnsupported)
	}
	runtime.DocumentPublications = nil
	if runtime.Context == nil {
		return fmt.Errorf("configure document publication commands: %w", domain.ErrCatalogUnsupported)
	}
	commands, err := runtime.Context.DocumentPublicationCommands()
	if err != nil {
		return fmt.Errorf("configure document publication commands: %w", err)
	}
	runtime.DocumentPublications = commands
	return nil
}

func (runtime *Runtime) ConfigureRepositoryAssetCommands() error {
	if runtime == nil || runtime.Context == nil {
		return fmt.Errorf("configure repository asset commands: %w", domain.ErrCatalogUnsupported)
	}
	commands, err := runtime.Context.RepositoryAssetCommands()
	if err != nil {
		return fmt.Errorf("configure repository asset commands: %w", err)
	}
	runtime.RepositoryAssets = commands
	return nil
}

func (runtime *Runtime) ConfigureLiveMemoryQueries() error {
	if runtime == nil || runtime.Context == nil {
		return fmt.Errorf("configure live memory queries: %w", domain.ErrCatalogUnsupported)
	}
	queries, err := runtime.Context.LiveMemoryQueries()
	if err != nil {
		return fmt.Errorf("configure live memory queries: %w", err)
	}
	runtime.MemoryLive = queries
	return nil
}

func (runtime *Runtime) ConfigureMemorySnapshotQueries() error {
	if runtime == nil || runtime.Context == nil {
		return fmt.Errorf("configure memory snapshot queries: %w", domain.ErrCatalogUnsupported)
	}
	queries, err := runtime.Context.MemorySnapshotQueries()
	if err != nil {
		return fmt.Errorf("configure memory snapshot queries: %w", err)
	}
	runtime.MemorySnapshots = queries
	return nil
}

func (runtime *Runtime) ConfigurePreparedMemoryQueries() error {
	if runtime == nil || runtime.Context == nil {
		return fmt.Errorf("configure prepared memory queries: %w", domain.ErrCatalogUnsupported)
	}
	queries, err := runtime.Context.PreparedMemoryQueries()
	if err != nil {
		return fmt.Errorf("configure prepared memory queries: %w", err)
	}
	runtime.PreparedMemory = queries
	return nil
}

func (runtime *Runtime) ConfigureRepositoryQueries() error {
	profile := inbound.RepositoryStateCompatibility
	if _, pinned := runtime.Store.(outbound.RepositoryTransactions); pinned {
		profile = inbound.RepositoryStatePinnedSnapshots
	}
	queries, err := runtime.Context.RepositoryStateQueriesForProfile(profile)
	if err != nil {
		return fmt.Errorf("configure repository state queries: %w", err)
	}
	graphs, err := runtime.Context.GraphQueriesForProfile(profile)
	if err != nil {
		return fmt.Errorf("configure repository graph queries: %w", err)
	}
	contexts, err := runtime.Context.ContextQueriesForProfile(profile)
	if err != nil {
		return fmt.Errorf("configure repository context queries: %w", err)
	}
	runtime.RepositoryStates = queries
	runtime.RepositoryStateProfile = profile
	runtime.RepositoryGraphs = graphs
	runtime.RepositoryGraphProfile = profile
	runtime.ContextQueries = contexts
	runtime.ContextQueryProfile = profile
	return nil
}

func (runtime *Runtime) ConfigureDocumentQueries() error {
	queries, err := runtime.Context.DocumentQueriesForProfile(runtime.DocumentQueryProfile)
	if err != nil {
		return fmt.Errorf("configure document queries: %w", err)
	}
	runtime.DocumentQueries = queries
	runtime.DocumentEvents = queries
	return nil
}

func (r *Runtime) Close() {
	if r.infrastructure != nil {
		_ = r.infrastructure.Close()
		return
	}
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
	return r.execution.handler(mux)
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
