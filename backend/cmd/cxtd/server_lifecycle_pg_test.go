//go:build postgres && !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const serverLifecycleChildEnv = "CXT_TEST_SERVER_LIFECYCLE_CHILD"

type serverLifecyclePGFixture struct {
	admin           *pgx.Conn
	observerConfig  *pgx.ConnConfig
	dsn             string
	database        string
	applicationName string
	migrations      string
}

type serverLifecycleProcess struct {
	command *exec.Cmd
	input   io.WriteCloser
	logPath string
	done    chan struct{}
	waitErr error
}

func TestServerLifecyclePGProcessHelper(test *testing.T) {
	mode := os.Getenv(serverLifecycleChildEnv)
	if mode == "" {
		return
	}
	if mode != "sigterm" && mode != "bind" {
		test.Fatal("unknown lifecycle subprocess mode")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(os.Stdout, "cxtd-process:serve-start")
	serveErr := serve(ctx, []string{"--addr", os.Getenv("CXT_ADDR"), "--data", os.Getenv("CXT_DATA")})
	if mode == "sigterm" {
		if serveErr != nil {
			test.Fatalf("serve after SIGTERM: %v", serveErr)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			test.Fatal("serve returned before signal context cancellation")
		}
	} else {
		if !errors.Is(serveErr, syscall.EADDRINUSE) {
			test.Fatalf("serve bind error = %v, want EADDRINUSE", serveErr)
		}
		if ctx.Err() != nil {
			test.Fatal("bind failure required parent signal cancellation")
		}
	}
	fmt.Fprintln(os.Stdout, "cxtd-process:serve-returned:"+mode)
	var acknowledgement string
	if _, err := fmt.Fscanln(os.Stdin, &acknowledgement); err != nil || acknowledgement != "release" {
		test.Fatalf("parent release acknowledgement = %q, error = %v", acknowledgement, err)
	}
}

func TestServerLifecyclePGProcessSIGTERMReleasesConnections(test *testing.T) {
	fixture := newServerLifecyclePGFixture(test)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		test.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		test.Fatal(err)
	}
	child := startServerLifecycleProcess(test, fixture, address, "sigterm")
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	child.await(test, time.Minute, "healthy HTTP startup", func() bool {
		probe, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(probe, http.MethodGet, "http://"+address+"/healthz", nil)
		if err != nil {
			test.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
		return err == nil && response.StatusCode == http.StatusOK && string(body) == `{"status":"ok"}`
	})
	blockerPID := fixture.lockNotificationWorker(test)
	child.await(test, 10*time.Second, "real notification worker blocked on fixture lock", func() bool {
		probe, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var blocked bool
		if err := fixture.admin.QueryRow(probe, `SELECT EXISTS (
 SELECT 1 FROM pg_stat_activity
 WHERE datname=$1 AND application_name=$2 AND wait_event_type='Lock'
 AND query LIKE '%notification_outbox%' AND $3::integer=ANY(pg_blocking_pids(pid)))`,
			fixture.database, fixture.applicationName, int32(blockerPID)).Scan(&blocked); err != nil {
			test.Fatal(err)
		}
		return blocked
	})
	if err := child.command.Process.Signal(syscall.SIGTERM); err != nil {
		test.Fatal(err)
	}
	child.awaitReturned(test, "sigterm", 15*time.Second)
	fixture.assertChildConnectionsReleased(test, child)
	probe, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(probe, "tcp", address)
	if err == nil {
		connection.Close()
		test.Fatal("HTTP listener still accepts connections after serve returned")
	}
	child.finish(test)
}

func TestServerLifecyclePGProcessBindFailureReleasesConnections(test *testing.T) {
	fixture := newServerLifecyclePGFixture(test)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		test.Fatal(err)
	}
	defer listener.Close()
	child := startServerLifecycleProcess(test, fixture, listener.Addr().String(), "bind")
	child.awaitReturned(test, "bind", time.Minute)
	fixture.assertChildConnectionsReleased(test, child)
	probe, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observer, err := pgx.ConnectConfig(probe, fixture.observerConfig.Copy())
	if err != nil {
		test.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observer.Close(cleanup); err != nil {
			test.Errorf("close lifecycle migration observer: %v", err)
		}
	}()
	var migrated bool
	if err := observer.QueryRow(probe, `SELECT to_regclass('public.notification_outbox') IS NOT NULL`).Scan(&migrated); err != nil {
		test.Fatal(err)
	}
	if !migrated {
		test.Fatal("bind failure did not reach actual PostgreSQL runtime startup")
	}
	child.finish(test)
}

func newServerLifecyclePGFixture(test *testing.T) *serverLifecyclePGFixture {
	test.Helper()
	baseDSN := strings.TrimSpace(os.Getenv("CXT_TEST_DSN"))
	if baseDSN == "" {
		test.Skip("CXT_TEST_DSN unset; lifecycle subprocess requires a disposable local PostgreSQL fixture")
	}
	config, err := pgxpool.ParseConfig(baseDSN)
	if err != nil {
		test.Fatal("invalid CXT_TEST_DSN")
	}
	localHost := func(host string) bool {
		if filepath.IsAbs(host) || host == "localhost" {
			return true
		}
		address := net.ParseIP(host)
		return address != nil && address.IsLoopback()
	}
	if !localHost(config.ConnConfig.Host) {
		test.Fatal("lifecycle fixture requires a local CXT_TEST_DSN")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !localHost(fallback.Host) {
			test.Fatal("lifecycle fixture requires local CXT_TEST_DSN fallbacks")
		}
	}
	migrations, err := filepath.Abs("../../../schemas/db/migrations")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(migrations, "0001_init.sql")); err != nil {
		test.Fatal(err)
	}
	setup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig := config.ConnConfig.Copy()
	adminConfig.RuntimeParams["application_name"] = "cxtd_lifecycle_admin"
	admin, err := pgx.ConnectConfig(setup, adminConfig)
	if err != nil {
		test.Fatal(err)
	}
	database := domain.NewID("cxtd_lifecycle_")
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(setup, "CREATE DATABASE "+quotedDatabase+" TEMPLATE template0"); err != nil {
		admin.Close(setup)
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+quotedDatabase); err != nil {
			test.Errorf("drop lifecycle fixture: %v", err)
		}
		if err := admin.Close(cleanup); err != nil {
			test.Errorf("close lifecycle fixture admin: %v", err)
		}
	})
	applicationName := domain.NewID("cxtd_process_")
	childDSN := baseDSN
	if strings.HasPrefix(baseDSN, "postgres://") || strings.HasPrefix(baseDSN, "postgresql://") {
		parsed, err := url.Parse(baseDSN)
		if err != nil {
			test.Fatal("invalid lifecycle fixture URL")
		}
		parsed.Path = "/" + database
		parsed.RawPath = ""
		query := parsed.Query()
		query.Set("dbname", database)
		query.Set("application_name", applicationName)
		parsed.RawQuery = query.Encode()
		childDSN = parsed.String()
	} else {
		childDSN += " dbname=" + database + " application_name=" + applicationName
	}
	childConfig, err := pgxpool.ParseConfig(childDSN)
	if err != nil || childConfig.ConnConfig.Database != database || childConfig.ConnConfig.RuntimeParams["application_name"] != applicationName {
		test.Fatal("lifecycle subprocess DSN isolation failed")
	}
	seed, err := store.NewPostgresStore(setup, childDSN)
	if err != nil {
		test.Fatal("open owned lifecycle fixture seed store", err)
	}
	_, err = seed.ApplyPublicMigrations(setup, migrations)
	seed.Close()
	if err != nil {
		test.Fatal("migrate only the owned lifecycle fixture database", err)
	}
	observerConfig := childConfig.ConnConfig.Copy()
	observerConfig.RuntimeParams["application_name"] = "cxtd_lifecycle_observer"
	return &serverLifecyclePGFixture{
		admin:           admin,
		observerConfig:  observerConfig,
		dsn:             childDSN,
		database:        database,
		applicationName: applicationName,
		migrations:      migrations,
	}
}

func (fixture *serverLifecyclePGFixture) lockNotificationWorker(test *testing.T) uint32 {
	test.Helper()
	setup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	observer, err := pgx.ConnectConfig(setup, fixture.observerConfig.Copy())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observer.Close(cleanup); err != nil {
			test.Errorf("close lifecycle lock observer: %v", err)
		}
	})
	transaction, err := observer.Begin(setup)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := transaction.Rollback(cleanup); err != nil {
			test.Errorf("release lifecycle fixture lock: %v", err)
		}
	})
	if _, err := transaction.Exec(setup, "LOCK TABLE notification_outbox IN ACCESS EXCLUSIVE MODE"); err != nil {
		test.Fatal(err)
	}
	return observer.PgConn().PID()
}

func (fixture *serverLifecyclePGFixture) assertChildConnectionsReleased(test *testing.T, child *serverLifecycleProcess) {
	test.Helper()
	child.await(test, 5*time.Second, "child database connections released before process exit", func() bool {
		probe, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var remaining int
		if err := fixture.admin.QueryRow(probe, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND application_name=$2`, fixture.database, fixture.applicationName).Scan(&remaining); err != nil {
			test.Fatal(err)
		}
		return remaining == 0
	})
	child.assertRunning(test)
}

func startServerLifecycleProcess(test *testing.T, fixture *serverLifecyclePGFixture, address, mode string) *serverLifecycleProcess {
	test.Helper()
	executable, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	command := exec.Command(executable, "-test.run=^TestServerLifecyclePGProcessHelper$", "-test.count=1", "-test.timeout=2m")
	command.Dir = directory
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ", "GOMAXPROCS", "SystemRoot", "WINDIR":
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env,
		"HOME="+directory,
		serverLifecycleChildEnv+"="+mode,
		"CXT_AUTH=dev",
		"CXT_ADDR="+address,
		"CXT_DATA="+filepath.Join(directory, "data"),
		"CXT_PUBLIC_URL=http://"+address,
		"CXT_WEB_URL=http://"+address,
		"CXT_REQUIRE_POSTGRES=1",
		"CXT_POSTGRES_DSN="+fixture.dsn,
		"CXT_MIGRATIONS_DIR="+fixture.migrations,
	)
	logPath := filepath.Join(directory, "process.log")
	output, err := os.Create(logPath)
	if err != nil {
		test.Fatal(err)
	}
	defer output.Close()
	command.Stdout, command.Stderr = output, output
	input, err := command.StdinPipe()
	if err != nil {
		test.Fatal(err)
	}
	if err := command.Start(); err != nil {
		input.Close()
		test.Fatal(err)
	}
	child := &serverLifecycleProcess{command: command, input: input, logPath: logPath, done: make(chan struct{})}
	go func() {
		child.waitErr = command.Wait()
		close(child.done)
	}()
	test.Cleanup(func() {
		select {
		case <-child.done:
		default:
			_ = command.Process.Kill()
			select {
			case <-child.done:
			case <-time.After(5 * time.Second):
				test.Error("lifecycle subprocess did not exit after cleanup kill")
			}
		}
		input.Close()
		if test.Failed() {
			output, err := os.ReadFile(logPath)
			if err != nil {
				test.Logf("read lifecycle subprocess output: %v", err)
			} else {
				test.Logf("lifecycle subprocess output:\n%s", output)
			}
		}
	})
	return child
}

func (child *serverLifecycleProcess) assertRunning(test *testing.T) {
	test.Helper()
	select {
	case <-child.done:
		test.Fatalf("lifecycle subprocess exited before parent acknowledgement: %v", child.waitErr)
	default:
	}
}

func (child *serverLifecycleProcess) await(test *testing.T, budget time.Duration, description string, ready func() bool) {
	test.Helper()
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		child.assertRunning(test)
		if ready() {
			return
		}
		select {
		case <-child.done:
			test.Fatalf("lifecycle subprocess exited waiting for %s: %v", description, child.waitErr)
		case <-deadline.C:
			test.Fatalf("lifecycle subprocess exceeded %s waiting for %s", budget, description)
		case <-ticker.C:
		}
	}
}

func (child *serverLifecycleProcess) awaitReturned(test *testing.T, mode string, budget time.Duration) {
	test.Helper()
	child.await(test, budget, "serve return: "+mode, func() bool {
		output, err := os.ReadFile(child.logPath)
		if err != nil {
			test.Fatal(err)
		}
		return strings.Contains(string(output), "cxtd-process:serve-returned:"+mode+"\n")
	})
}

func (child *serverLifecycleProcess) finish(test *testing.T) {
	test.Helper()
	child.assertRunning(test)
	if _, err := io.WriteString(child.input, "release\n"); err != nil {
		test.Fatal(err)
	}
	if err := child.input.Close(); err != nil {
		test.Fatal(err)
	}
	select {
	case <-child.done:
		if child.waitErr != nil {
			test.Fatalf("lifecycle subprocess exit: %v", child.waitErr)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("lifecycle subprocess did not exit after parent acknowledgement")
	}
	output, err := os.ReadFile(child.logPath)
	if err != nil {
		test.Fatal(err)
	}
	if strings.Count(string(output), "cxtd-process:serve-start\n") != 1 || strings.Count(string(output), "cxtd-process:serve-returned:") != 1 {
		test.Fatal("lifecycle subprocess must invoke actual serve exactly once")
	}
}
