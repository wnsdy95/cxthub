package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
	"github.com/wnsdy95/cxthub/backend/internal/workerserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cxt-worker:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "run" {
		return fmt.Errorf("%w: usage: cxt-worker run", domain.ErrValidation)
	}
	if err := serverruntime.LoadEnv(); err != nil {
		return err
	}
	worker, err := workerserver.OpenWorker(ctx)
	if err != nil {
		return err
	}
	runErr := worker.Run(ctx)
	return errors.Join(runErr, worker.Close())
}
