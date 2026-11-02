//go:build postgres

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

type migrationTestStore struct {
	ctx          context.Context
	directory    string
	operations   []string
	applied      int
	applyError   error
	verifyError  error
	contextError error
	closed       int
}

func (storage *migrationTestStore) record(ctx context.Context, directory, operation string) {
	storage.operations = append(storage.operations, operation)
	if ctx != storage.ctx || directory != storage.directory {
		storage.contextError = errors.New("migration context or directory changed")
	}
}

func (storage *migrationTestStore) ApplyPublicMigrations(ctx context.Context, directory string) (int, error) {
	storage.record(ctx, directory, "apply-public")
	return storage.applied, storage.applyError
}

func (storage *migrationTestStore) VerifyMigrations(ctx context.Context, directory string) error {
	storage.record(ctx, directory, "verify")
	return storage.verifyError
}

func (storage *migrationTestStore) Close() {
	storage.operations = append(storage.operations, "close")
	storage.closed++
}

type migrationErrorWriter struct {
	err error
}

func (writer migrationErrorWriter) Write(contents []byte) (int, error) {
	return 0, writer.err
}

func TestParseMigrationOptions(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		args      []string
		env       string
		directory string
		apply     bool
	}{
		{name: "default", directory: "schemas/db/migrations"},
		{name: "empty environment", env: " \t\n", directory: "schemas/db/migrations"},
		{name: "environment", env: "/operator/bundle", directory: "/operator/bundle"},
		{name: "trim environment", env: " /operator/bundle \n", directory: "/operator/bundle"},
		{name: "flag overrides environment", args: []string{"--dir", "/explicit/bundle"}, env: "/ignored", directory: "/explicit/bundle"},
		{name: "equals flag", args: []string{"--dir=/explicit/bundle"}, directory: "/explicit/bundle"},
		{name: "apply", args: []string{"--apply"}, directory: "schemas/db/migrations", apply: true},
		{name: "apply with directory", args: []string{"--apply", "--dir", "/explicit/bundle"}, directory: "/explicit/bundle", apply: true},
		{name: "explicit check", args: []string{"--apply=false"}, directory: "schemas/db/migrations"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			options, err := parseMigrationOptions(scenario.args, scenario.env)
			if err != nil || options.directory != scenario.directory || options.apply != scenario.apply {
				test.Fatalf("unexpected options: %+v, %v", options, err)
			}
		})
	}
}

func TestParseMigrationOptionsRejectsInvalidArgumentsWithoutEchoing(test *testing.T) {
	const secret = "private-argument-must-not-be-printed"
	for _, args := range [][]string{
		{"--unknown=" + secret},
		{"--apply=" + secret},
		{"--dir"},
		{"--dir="},
		{"--dir", " \t\n"},
		{secret},
		{"--apply", secret},
		{"--", secret},
		{"--dir", "/bundle", secret},
	} {
		options, err := parseMigrationOptions(args, "")
		if err == nil || options != (migrationOptions{}) || strings.Contains(err.Error(), secret) {
			test.Fatal("invalid migration arguments accepted or disclosed")
		}
	}
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		if _, err := parseMigrationOptions(args, ""); !errors.Is(err, flag.ErrHelp) {
			test.Fatal("migration help not recognized", err)
		}
	}
}

func TestMigrationsRejectInvalidDSNBeforeOpening(test *testing.T) {
	const secret = "private-dsn-must-not-be-printed"
	for _, dsn := range []string{
		"",
		" \t\n",
		"sqlite:///" + secret,
		"https://operator:" + secret + "@database.invalid/storage",
		"postgres://operator:" + secret + "@database.invalid:invalid/storage",
		"host='" + secret,
	} {
		test.Setenv("CXT_POSTGRES_DSN", dsn)
		test.Setenv("CXT_MIGRATIONS_DIR", "")
		var stdout, stderr bytes.Buffer
		opened := false
		err := runMigrationsWithOpener(context.Background(), nil, &stdout, &stderr, func(context.Context, string) (migrationStore, error) {
			opened = true
			return nil, errors.New(secret)
		})
		if err == nil || opened || stdout.Len() != 0 || stderr.Len() != 0 || !strings.Contains(err.Error(), "CXT_POSTGRES_DSN") || strings.Contains(err.Error(), secret) {
			test.Fatal("invalid PostgreSQL configuration accepted, opened storage, or disclosed")
		}
		if err := runMigrations(context.Background(), nil, &stdout, &stderr); err == nil || stdout.Len() != 0 || stderr.Len() != 0 || strings.Contains(err.Error(), secret) {
			test.Fatal("native migration command accepted or disclosed invalid PostgreSQL configuration")
		}
	}
}

func TestMigrationsModesUseOnlyExplicitPublicApplyAndVerification(test *testing.T) {
	for _, scenario := range []struct {
		name       string
		args       []string
		env        string
		dsn        string
		directory  string
		mode       string
		applied    int
		operations []string
	}{
		{name: "default check", dsn: "postgres://operator:secret@database.invalid/storage", directory: "schemas/db/migrations", mode: "check", operations: []string{"verify", "close"}},
		{name: "environment check", env: "/environment/bundle", dsn: "postgresql://operator:secret@database.invalid/storage", directory: "/environment/bundle", mode: "check", operations: []string{"verify", "close"}},
		{name: "explicit apply", args: []string{"--dir", "/explicit/bundle", "--apply"}, env: "/ignored", dsn: "host=database.invalid dbname=storage user=operator password=secret", directory: "/explicit/bundle", mode: "apply", applied: 3, operations: []string{"apply-public", "verify", "close"}},
		{name: "apply already current", args: []string{"--apply"}, dsn: "postgres://operator:secret@database.invalid/storage", directory: "schemas/db/migrations", mode: "apply", operations: []string{"apply-public", "verify", "close"}},
		{name: "explicit false", args: []string{"--apply=false"}, dsn: "postgres://operator:secret@database.invalid/storage", directory: "schemas/db/migrations", mode: "check", operations: []string{"verify", "close"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			test.Setenv("CXT_POSTGRES_DSN", scenario.dsn)
			test.Setenv("CXT_MIGRATIONS_DIR", scenario.env)
			test.Setenv("CXT_IDENTITY_ENCRYPTION_KEY", "invalid-unused-identity-secret")
			test.Setenv("CXT_IDENTITY_ENCRYPTION_KEYRING", "invalid-unused-keyring-secret")
			test.Setenv("CXT_EXECUTION_PROFILE", "invalid-unused-profile")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			storage := &migrationTestStore{ctx: ctx, directory: scenario.directory, applied: scenario.applied}
			var stdout, stderr bytes.Buffer
			opened := 0
			err := runMigrationsWithOpener(ctx, scenario.args, &stdout, &stderr, func(openContext context.Context, dsn string) (migrationStore, error) {
				opened++
				if openContext != ctx || dsn != scenario.dsn {
					test.Fatal("migration opening changed context or DSN")
				}
				if _, deadline := openContext.Deadline(); deadline {
					test.Fatal("migration command imposed a deadline")
				}
				return storage, nil
			})
			if err != nil || opened != 1 || storage.contextError != nil || storage.closed != 1 || stderr.Len() != 0 || !reflect.DeepEqual(storage.operations, scenario.operations) {
				test.Fatal("migration lifecycle changed", err, opened, storage.contextError, storage.operations)
			}
			var result map[string]any
			decoder := json.NewDecoder(&stdout)
			if err := decoder.Decode(&result); err != nil {
				test.Fatal(err)
			}
			if len(result) != 3 || result["mode"] != scenario.mode || result["applied"] != float64(scenario.applied) || result["verified"] != true {
				test.Fatal("unexpected migration result", result)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				test.Fatal("unexpected output after migration JSON", err)
			}
		})
	}
}

func TestMigrationsFailuresCloseOwnedStoreAndHideSecrets(test *testing.T) {
	const secret = "private-database-or-manifest-detail"
	privateError := errors.New(secret)
	mismatch := errors.Join(store.ErrSchemaMismatch, privateError)
	for _, scenario := range []struct {
		name        string
		args        []string
		openError   error
		applyError  error
		verifyError error
		outputError error
		operations  []string
		wantError   error
		message     string
	}{
		{name: "open failure", openError: privateError, operations: []string{"close"}, wantError: privateError, message: "connection failed"},
		{name: "manifest failure", verifyError: privateError, operations: []string{"verify", "close"}, wantError: privateError, message: "complete bundle"},
		{name: "check mismatch", verifyError: mismatch, operations: []string{"verify", "close"}, wantError: store.ErrSchemaMismatch, message: "schema mismatch"},
		{name: "apply failure", args: []string{"--apply"}, applyError: privateError, operations: []string{"apply-public", "close"}, wantError: privateError, message: "prior migrations may already be committed"},
		{name: "apply mismatch", args: []string{"--apply"}, applyError: mismatch, operations: []string{"apply-public", "close"}, wantError: store.ErrSchemaMismatch, message: "schema mismatch"},
		{name: "verify after apply fails", args: []string{"--apply"}, verifyError: mismatch, operations: []string{"apply-public", "verify", "close"}, wantError: store.ErrSchemaMismatch, message: "verification failed"},
		{name: "canceled apply", args: []string{"--apply"}, applyError: errors.Join(context.Canceled, privateError), operations: []string{"apply-public", "close"}, wantError: context.Canceled, message: "migrate canceled"},
		{name: "canceled check", verifyError: errors.Join(context.Canceled, privateError), operations: []string{"verify", "close"}, wantError: context.Canceled, message: "migrate canceled"},
		{name: "caller deadline", verifyError: errors.Join(context.DeadlineExceeded, privateError), operations: []string{"verify", "close"}, wantError: context.DeadlineExceeded, message: "context deadline exceeded"},
		{name: "output failure", outputError: privateError, operations: []string{"verify", "close"}, wantError: privateError, message: "output failed"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			test.Setenv("CXT_POSTGRES_DSN", "postgres://operator:"+secret+"@database.invalid/storage")
			test.Setenv("CXT_MIGRATIONS_DIR", "/bundle/"+secret)
			ctx := context.Background()
			storage := &migrationTestStore{ctx: ctx, directory: "/bundle/" + secret, applied: 2, applyError: scenario.applyError, verifyError: scenario.verifyError}
			var stdout, stderr bytes.Buffer
			var output io.Writer = &stdout
			if scenario.outputError != nil {
				output = migrationErrorWriter{err: scenario.outputError}
			}
			err := runMigrationsWithOpener(ctx, scenario.args, output, &stderr, func(context.Context, string) (migrationStore, error) {
				return storage, scenario.openError
			})
			if err == nil || !errors.Is(err, scenario.wantError) || !strings.Contains(err.Error(), scenario.message) || strings.Contains(err.Error()+stdout.String()+stderr.String(), secret) {
				test.Fatal("failure lost its safe diagnostic or error identity")
			}
			if stdout.Len() != 0 || stderr.Len() != 0 || storage.contextError != nil || storage.closed != 1 || !reflect.DeepEqual(storage.operations, scenario.operations) {
				test.Fatal("failure reported success, skipped close, or continued migration", storage.operations)
			}
		})
	}
}

func TestMigrationsRejectNilStoreAndOpenFailureWithoutSuccess(test *testing.T) {
	for _, openError := range []error{nil, errors.New("private-open-error")} {
		test.Setenv("CXT_POSTGRES_DSN", "postgres://operator:secret@database.invalid/storage")
		test.Setenv("CXT_MIGRATIONS_DIR", "")
		var stdout, stderr bytes.Buffer
		err := runMigrationsWithOpener(context.Background(), nil, &stdout, &stderr, func(context.Context, string) (migrationStore, error) {
			return nil, openError
		})
		if err == nil || stdout.Len() != 0 || stderr.Len() != 0 || strings.Contains(err.Error(), "private-open-error") {
			test.Fatal("missing PostgreSQL store reported success or disclosed opening error")
		}
	}
}

func TestMigrationsHelpAndInvalidArgsDoNotOpenStorage(test *testing.T) {
	for _, scenario := range []struct {
		args []string
		help bool
	}{
		{args: []string{"--help"}, help: true},
		{args: []string{"-h"}, help: true},
		{args: []string{"--unknown=private-argument"}},
		{args: []string{"--apply", "private-argument"}},
	} {
		test.Setenv("CXT_POSTGRES_DSN", "")
		var stdout, stderr bytes.Buffer
		opened := false
		err := runMigrationsWithOpener(context.Background(), scenario.args, &stdout, &stderr, func(context.Context, string) (migrationStore, error) {
			opened = true
			return nil, nil
		})
		if opened || stdout.Len() != 0 || (err == nil) != scenario.help {
			test.Fatal("help or invalid arguments reached storage")
		}
		if scenario.help && !strings.Contains(stderr.String(), migrationUsage) {
			test.Fatal("migration help omitted usage")
		}
		if !scenario.help && (stderr.Len() != 0 || strings.Contains(err.Error(), "private-argument")) {
			test.Fatal("invalid migration arguments disclosed input")
		}
	}
}

func TestMigrationsCanceledContextDoesNotOpenStorage(test *testing.T) {
	test.Setenv("CXT_POSTGRES_DSN", "postgres://operator:secret@database.invalid/storage")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	opened := false
	err := runMigrationsWithOpener(ctx, []string{"--apply"}, &stdout, &stderr, func(context.Context, string) (migrationStore, error) {
		opened = true
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || opened || stdout.Len() != 0 || stderr.Len() != 0 {
		test.Fatal("canceled migration opened storage or reported success")
	}
}

func TestMigrationsHelpOutputFailureHidesSecrets(test *testing.T) {
	test.Setenv("CXT_POSTGRES_DSN", "")
	privateError := errors.New("private-help-output-error")
	var stdout bytes.Buffer
	err := runMigrations(context.Background(), []string{"--help"}, &stdout, migrationErrorWriter{err: privateError})
	if !errors.Is(err, privateError) || strings.Contains(err.Error(), privateError.Error()) || stdout.Len() != 0 {
		test.Fatal("migration help output failure lost error identity or disclosed details")
	}
}

func TestMigrateDispatchPrecedesProvisioningAndIdentity(test *testing.T) {
	originalArgs := os.Args
	test.Cleanup(func() { os.Args = originalArgs })
	os.Args = []string{"cxt-admin", "migrate", "--unknown=private-argument"}
	test.Setenv("CXT_POSTGRES_DSN", "")
	test.Setenv("CXT_IDENTITY_ENCRYPTION_KEY", "invalid-unused-identity-secret")
	err := run()
	if err == nil || !strings.Contains(err.Error(), "invalid migrate arguments") || strings.Contains(err.Error(), "private-argument") {
		test.Fatal("migrate dispatch reached another operator command or disclosed input")
	}
}
