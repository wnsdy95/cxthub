//go:build postgres

// Offline predeployment rehearsal. This tool cannot target a cloud database.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

func run() error {
	root := flag.String("source", "", "frozen FS backup directory (never a running store)")
	migrations := flag.String("migrations", "schemas/db/migrations", "schema migration directory")
	apply := flag.Bool("apply", false, "retain the verified rehearsal in an empty local database; default rolls back")
	flag.Parse()
	if *root == "" {
		return fmt.Errorf("--source frozen backup directory is required")
	}
	dsn := os.Getenv("CXT_REHEARSAL_DSN")
	if dsn == "" {
		return fmt.Errorf("CXT_REHEARSAL_DSN is required (disposable local PostgreSQL)")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("invalid rehearsal DSN")
	}
	host := config.ConnConfig.Host
	checkHost := func(host string) error {
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("rehearsal requires a loopback PostgreSQL host")
			}
		}
		return nil
	}
	if err = checkHost(host); err != nil {
		return err
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if err = checkHost(fallback.Host); err != nil {
			return err
		}
	}
	if config.ConnConfig.Database == "" || config.ConnConfig.Database == "postgres" {
		return fmt.Errorf("use a named disposable database")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		return fmt.Errorf("cannot connect to local PostgreSQL")
	}
	defer st.Close()
	if err = st.CheckImportTargetEmpty(ctx); err != nil {
		return err
	}
	if _, err = st.ApplyMigrations(ctx, *migrations); err != nil {
		return fmt.Errorf("schema migration: %w", err)
	}
	report, err := st.ImportFrozenFS(ctx, *root, *apply)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FS rehearsal:", err)
		os.Exit(1)
	}
}
