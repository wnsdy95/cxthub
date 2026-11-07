package cli

import (
	"context"
	"errors"
	"io"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// Bulk chunk uploads from every local cxt process for one repository share a
// single in-flight request slot, so detached publishers cannot multiply load
// on a small server. Unlike configuration/storage locks, these dedicated
// flocks are intentionally held over exactly one bounded HTTP request (the
// "batch" slot) or one foreground push (the "foreground" marker); the kernel
// releases both if a process dies.
const (
	bulkUploadLockNamespace = "bulk-upload"
	// A manual push waits out a slow, shedding server; a stuck batch still ends.
	foregroundUploadStall = 15 * time.Minute
	// pre-push runs under the 60s hook deadline; a deferred request follows.
	prePushUploadStall = 20 * time.Second
	// Detached workers stay bounded and retry on their next wake.
	backgroundUploadStall = 2 * time.Minute
	// Background batches poll the foreground marker at this interval.
	backgroundYieldPoll = time.Second
)

// bulkUploadSlot runs one request while holding the repository's batch slot.
func bulkUploadSlot(root string) func(context.Context, func() error) error {
	return func(ctx context.Context, request func() error) error {
		var err error
		_, lockErr := providerfs.WithCxtLock(ctx, root, bulkUploadLockNamespace, "batch", syscall.LOCK_EX, true, func() error {
			err = request()
			return nil
		})
		if lockErr != nil {
			return lockErr
		}
		return err
	}
}

// foregroundUploadActive reports whether a foreground push holds its marker.
func foregroundUploadActive(ctx context.Context, root string) (bool, error) {
	acquired, err := providerfs.WithCxtLock(ctx, root, bulkUploadLockNamespace, "foreground", syscall.LOCK_EX, false, func() error { return nil })
	return !acquired && err == nil, err
}

// backgroundUploadGate yields every batch to an active foreground push before
// taking the shared slot, so a user's push waits for at most one batch.
func backgroundUploadGate(root string) func(context.Context, func() error) error {
	slot := bulkUploadSlot(root)
	return func(ctx context.Context, request func() error) error {
		for {
			active, err := foregroundUploadActive(ctx, root)
			if err != nil {
				return err
			}
			if !active {
				break
			}
			if err := uploadWait(ctx, backgroundYieldPoll); err != nil {
				return err
			}
		}
		return slot(ctx, request)
	}
}

func uploadWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// withBackgroundUploads marks a detached worker's uploads as bounded,
// paced and yielding. It is a no-op outside an initialized cxt repository.
func withBackgroundUploads(ctx context.Context, cwd string) context.Context {
	root := cxtRepoRoot(ctx, cwd)
	if root == "" {
		return ctx
	}
	return outbound.WithUploadPolicy(ctx, outbound.UploadPolicy{StallBudget: backgroundUploadStall, Gate: backgroundUploadGate(root)})
}

// runForegroundUpload holds the foreground marker (shared, so concurrent
// manual pushes do not block each other) for the whole push and gives its
// batches patient backpressure handling plus chunk progress.
func runForegroundUpload(ctx context.Context, cwd string, stall time.Duration, progress io.Writer, push func(context.Context) error) error {
	root := cxtRepoRoot(ctx, cwd)
	if root == "" {
		return push(ctx)
	}
	policy := outbound.UploadPolicy{StallBudget: stall, Gate: bulkUploadSlot(root)}
	if progress != nil {
		printer := syncProgressPrinter(progress)
		policy.Progress = func(sent, total int) {
			printer(inbound.SyncProgress{Operation: "push", Phase: "upload-chunks", Completed: sent, Total: total})
		}
	}
	var err error
	_, lockErr := providerfs.WithCxtLock(ctx, root, bulkUploadLockNamespace, "foreground", syscall.LOCK_SH, true, func() error {
		err = push(outbound.WithUploadPolicy(ctx, policy))
		return nil
	})
	if lockErr != nil {
		return lockErr
	}
	return err
}

// uploadInterrupted reports a push that stopped on backpressure or a budget,
// where accepted chunk batches remain on the server for the next run.
func uploadInterrupted(err error) bool {
	var rejection *backendclient.HTTPError
	if errors.As(err, &rejection) && rejection.Status == 503 && rejection.Code == "resource_overloaded" {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
