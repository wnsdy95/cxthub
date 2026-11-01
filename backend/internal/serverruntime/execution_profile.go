package serverruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const executionBudgetLimit = executionprofile.MaxManifestBytes

type executionProfile struct {
	budget       executionprofile.Budget
	role         executionprofile.RoleBudget
	admission    outbound.ProcessAdmission
	ctx          context.Context
	cancel       context.CancelFunc
	controlDone  func()
	restore      func()
	spoolRestore func() error
	mu           sync.Mutex
	closing      bool
	requests     sync.WaitGroup
	closeOnce    sync.Once
	closeErr     error
}

func executionProfileFromEnvironment(role outbound.ProcessRole, required bool) (*executionProfile, error) {
	name := strings.TrimSpace(os.Getenv("CXT_EXECUTION_PROFILE"))
	if name == "" || name == "embedded" {
		if required {
			return nil, fmt.Errorf("%w: cxt-worker requires CXT_EXECUTION_PROFILE=%s or %s", domain.ErrValidation, outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile)
		}
		return nil, nil
	}
	if name != outbound.BoundedDocumentsProfile && name != outbound.BoundedWorkersProfile {
		return nil, fmt.Errorf("%w: CXT_EXECUTION_PROFILE is unsupported", errors.ErrUnsupported)
	}
	path := strings.TrimSpace(os.Getenv("CXT_EXECUTION_BUDGET_FILE"))
	if path == "" {
		return nil, fmt.Errorf("%w: CXT_EXECUTION_BUDGET_FILE is required", domain.ErrValidation)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read CXT_EXECUTION_BUDGET_FILE", domain.ErrValidation)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, executionBudgetLimit+1))
	if err != nil || len(raw) > executionBudgetLimit {
		return nil, fmt.Errorf("%w: execution budget must be readable and at most 64 KiB", domain.ErrValidation)
	}
	budget, err := executionprofile.Parse(raw)
	if err != nil {
		return nil, err
	}
	if budget.Profile != name {
		return nil, fmt.Errorf("%w: CXT_EXECUTION_PROFILE must match the execution budget profile", domain.ErrValidation)
	}
	roleBudget, err := budget.ForRole(role)
	if err != nil {
		return nil, err
	}
	if err := validateExecutionCPU(roleBudget.CPU); err != nil {
		return nil, err
	}
	return &executionProfile{budget: budget, role: roleBudget}, nil
}

func validateExecutionCPU(cpu int64) error {
	if cpu <= 0 || cpu > int64(^uint(0)>>1) || cpu > int64(runtime.NumCPU()) {
		return fmt.Errorf("%w: execution CPU declaration exceeds this runtime", domain.ErrValidation)
	}
	return nil
}

func executionDataDSN(raw string, role executionprofile.RoleBudget) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%w: bounded execution requires CXT_POSTGRES_DSN", domain.ErrValidation)
	}
	if role.DataPoolMax <= 0 || (role.Role != outbound.ProcessAPI && role.Role != outbound.ProcessMCP && role.Role != outbound.ProcessWorker) {
		return "", domain.ErrValidation
	}
	if _, err := pgxpool.ParseConfig(raw); err != nil {
		return "", fmt.Errorf("%w: invalid PostgreSQL configuration", domain.ErrValidation)
	}
	maximum := strconv.FormatInt(int64(role.DataPoolMax), 10)
	applicationName := "cxthub/" + outbound.BoundedDocumentsProfile + "/" + string(role.Role) + "/data"
	var rewritten string
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("%w: invalid PostgreSQL configuration", domain.ErrValidation)
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return "", fmt.Errorf("%w: invalid PostgreSQL configuration", domain.ErrValidation)
		}
		query.Set("pool_max_conns", maximum)
		query.Set("pool_min_conns", "0")
		query.Set("pool_min_idle_conns", "0")
		query.Set("application_name", applicationName)
		parsed.RawQuery = query.Encode()
		rewritten = parsed.String()
	} else {
		rewritten = raw + " pool_max_conns=" + maximum + " pool_min_conns=0 pool_min_idle_conns=0 application_name=" + applicationName
	}
	verified, err := pgxpool.ParseConfig(rewritten)
	if err != nil || verified.MaxConns != role.DataPoolMax || verified.MinConns != 0 || verified.MinIdleConns != 0 || verified.ConnConfig.RuntimeParams["application_name"] != applicationName {
		return "", fmt.Errorf("%w: PostgreSQL execution pool configuration disagrees with budget", domain.ErrValidation)
	}
	return rewritten, nil
}

func (profile *executionProfile) configure(ctx context.Context, storage any) (failure error) {
	if err := validateExecutionCPU(profile.role.CPU); err != nil {
		return err
	}
	admissionStore, ok := storage.(outbound.ProcessAdmissionStore)
	if !ok {
		return fmt.Errorf("%w: bounded execution requires PostgreSQL process admission", errors.ErrUnsupported)
	}
	spools, ok := storage.(outbound.TransientSpoolBudgetStore)
	if !ok {
		return fmt.Errorf("%w: bounded execution requires transient spool accounting", errors.ErrUnsupported)
	}
	restoreSpools, err := spools.ConfigureTransientSpoolBudget(profile.role.SpoolBytes)
	if err != nil {
		return err
	}
	configured := false
	defer func() {
		if !configured {
			failure = errors.Join(failure, restoreSpools())
		}
	}()
	config, err := profile.budget.AdmissionConfig(profile.role.Role)
	if err != nil {
		return err
	}
	control, cancelControl := context.WithCancel(context.WithoutCancel(ctx))
	stopBootstrap := context.AfterFunc(ctx, cancelControl)
	admission, err := admissionStore.OpenProcessAdmission(control, config)
	stopBootstrap()
	if err != nil {
		cancelControl()
		if err := ctx.Err(); err != nil {
			return err
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		cancelControl()
		_ = admission.Close()
		return err
	}
	profile.admission = admission
	profile.ctx, profile.cancel = context.WithCancel(ctx)
	stopControl := context.AfterFunc(admission.Context(), profile.cancel)
	profile.controlDone = func() {
		stopControl()
		cancelControl()
	}
	restoreAdmission, err := app.ConfigureProcessAdmission(admission, app.ProcessAdmissionLimits{
		Active: int(profile.role.Active), Queue: int(profile.role.Queue),
	})
	if err != nil {
		profile.cancel()
		profile.controlDone()
		_ = admission.Close()
		return err
	}
	previousCPU := runtime.GOMAXPROCS(int(profile.role.CPU))
	previousMemory := debug.SetMemoryLimit(profile.role.MemoryBytes)
	profile.restore = func() {
		restoreAdmission()
		debug.SetMemoryLimit(previousMemory)
		runtime.GOMAXPROCS(previousCPU)
	}
	profile.spoolRestore = restoreSpools
	configured = true
	return nil
}

func (profile *executionProfile) processContext(ctx context.Context) context.Context {
	if profile == nil || profile.ctx == nil {
		return ctx
	}
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(profile.ctx, cancel)
	context.AfterFunc(bound, func() { stop() })
	if profile.ctx.Err() != nil {
		cancel()
	}
	return bound
}

func (profile *executionProfile) ready() bool {
	return profile == nil || (profile.admission != nil && profile.ctx.Err() == nil && profile.admission.Ready())
}

func (profile *executionProfile) handler(next http.Handler) http.Handler {
	if profile == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		profile.mu.Lock()
		if profile.closing || !profile.ready() {
			profile.mu.Unlock()
			writer.Header().Set("Cache-Control", "no-store")
			http.Error(writer, "execution profile unavailable", http.StatusServiceUnavailable)
			return
		}
		profile.requests.Add(1)
		profile.mu.Unlock()
		defer profile.requests.Done()
		ctx, cancel := context.WithCancel(request.Context())
		stop := context.AfterFunc(profile.ctx, cancel)
		defer stop()
		defer cancel()
		if profile.ctx.Err() != nil {
			cancel()
		}
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (profile *executionProfile) close() error {
	if profile == nil {
		return nil
	}
	profile.closeOnce.Do(func() {
		profile.mu.Lock()
		profile.closing = true
		if profile.cancel != nil {
			profile.cancel()
		}
		profile.mu.Unlock()
		profile.requests.Wait()
		if profile.spoolRestore != nil {
			profile.closeErr = profile.spoolRestore()
		}
		if profile.admission != nil {
			profile.closeErr = errors.Join(profile.closeErr, profile.admission.Close())
		}
		if profile.controlDone != nil {
			profile.controlDone()
		}
		if profile.restore != nil {
			profile.restore()
		}
	})
	return profile.closeErr
}

func (runtime *Runtime) ProcessContext(ctx context.Context) context.Context {
	return runtime.execution.processContext(ctx)
}

func (runtime *Runtime) BoundedDocuments() bool {
	return runtime.execution != nil
}

func (runtime *Runtime) ExternalWorkers() bool {
	return runtime.execution != nil && runtime.execution.budget.Profile == outbound.BoundedWorkersProfile
}
