//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPGDocClaimPoolExhaustionReleasesAdmission(t *testing.T) {
	service, storage, _ := collaborationPG(t)
	ctx, gate := admissionTestContext(resourceWaitLimit)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	acquired := make(chan struct{}, 2)
	release := make(chan struct{})
	finished := make(chan error, 2)
	for range 2 {
		go func() {
			finished <- storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				acquired <- struct{}{}
				select {
				case <-release:
					return nil
				case <-bound.Done():
					return bound.Err()
				}
			})
		}()
	}
	defer func() {
		close(release)
		for range 2 {
			if err := <-finished; err != nil {
				t.Error(err)
			}
		}
	}()
	for range 2 {
		select {
		case <-acquired:
		case <-ctx.Done():
			t.Fatal("could not occupy both fixture pool connections")
		}
	}
	started := time.Now()
	err := service.ProcessDocFinalizations(ctx, 1)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || time.Since(started) > 7*time.Second {
		t.Fatalf("pool claim must expire independently: elapsed=%v error=%v parent=%v", time.Since(started), err, ctx.Err())
	}
	assertResourcesReleased(t, gate)
	_, releasePermit, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal("blocked document claim stranded bulk admission", err)
	}
	releasePermit()
}
