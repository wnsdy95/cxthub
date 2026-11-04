//go:build postgres

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

const migrationUsage = "usage: cxt-admin migrate [--dir DIR] [--apply] (default: check only; requires CXT_POSTGRES_DSN; directory: --dir, CXT_MIGRATIONS_DIR, schemas/db/migrations)"

type migrationOptions struct {
	directory string
	apply     bool
}

type migrationStore interface {
	ApplyPublicMigrations(context.Context, string) (int, error)
	VerifyMigrations(context.Context, string) error
	Close()
}

type migrationResult struct {
	Mode     string `json:"mode"`
	Applied  int    `json:"applied"`
	Verified bool   `json:"verified"`
}

type migrationCommandError struct {
	message string
	cause   error
}

func (failure *migrationCommandError) Error() string { return failure.message }

func (failure *migrationCommandError) Unwrap() error { return failure.cause }

func migrationFailure(message string, cause error) error {
	switch {
	case errors.Is(cause, context.Canceled):
		message = "migrate canceled"
	case errors.Is(cause, context.DeadlineExceeded):
		message = "migrate context deadline exceeded"
	case errors.Is(cause, store.ErrSchemaMismatch):
		message += "; schema mismatch"
	}
	return &migrationCommandError{message: message, cause: cause}
}

func parseMigrationOptions(args []string, directory string) (migrationOptions, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		directory = "schemas/db/migrations"
	}
	var options migrationOptions
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.directory, "dir", directory, "migration bundle directory")
	flags.BoolVar(&options.apply, "apply", false, "explicitly apply migrations before verification")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return migrationOptions{}, flag.ErrHelp
		}
		return migrationOptions{}, errors.New("invalid migrate arguments; " + migrationUsage)
	}
	if flags.NArg() != 0 {
		return migrationOptions{}, errors.New("unexpected migrate arguments; " + migrationUsage)
	}
	if strings.TrimSpace(options.directory) == "" {
		return migrationOptions{}, errors.New("migrate --dir requires a nonempty migration directory")
	}
	return options, nil
}

func runMigrations(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runMigrationsWithOpener(ctx, args, stdout, stderr, func(ctx context.Context, dsn string) (migrationStore, error) {
		storage, err := store.NewPostgresStore(ctx, dsn)
		if err != nil {
			return nil, err
		}
		return storage, nil
	})
}

func runMigrationsWithOpener(ctx context.Context, args []string, stdout, stderr io.Writer, openStorage func(context.Context, string) (migrationStore, error)) error {
	options, err := parseMigrationOptions(args, os.Getenv("CXT_MIGRATIONS_DIR"))
	if errors.Is(err, flag.ErrHelp) {
		if _, err := fmt.Fprintln(stderr, migrationUsage); err != nil {
			return migrationFailure("migrate help output failed", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("CXT_POSTGRES_DSN"))
	if dsn == "" {
		return errors.New("CXT_POSTGRES_DSN is required for migrate; PostgreSQL only")
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return migrationFailure("CXT_POSTGRES_DSN must be a valid PostgreSQL connection string", err)
	}
	if err := ctx.Err(); err != nil {
		return migrationFailure("migrate canceled before opening PostgreSQL", err)
	}
	storage, err := openStorage(ctx, dsn)
	if storage != nil {
		defer storage.Close()
	}
	if err != nil {
		return migrationFailure("migrate PostgreSQL connection failed; check CXT_POSTGRES_DSN", err)
	}
	if storage == nil {
		return errors.New("migrate requires a PostgreSQL store")
	}
	result := migrationResult{Mode: "check"}
	if options.apply {
		result.Mode = "apply"
		result.Applied, err = storage.ApplyPublicMigrations(ctx, options.directory)
		if err != nil {
			return migrationFailure("migrate apply failed; check the migration directory, complete bundle, public schema, history, and permissions; prior migrations may already be committed", err)
		}
	}
	if err := storage.VerifyMigrations(ctx, options.directory); err != nil {
		return migrationFailure("migrate verification failed; check the migration directory, complete bundle, public schema, history, and permissions; review before using --apply", err)
	}
	result.Verified = true
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return migrationFailure("migrate result output failed", err)
	}
	return nil
}
