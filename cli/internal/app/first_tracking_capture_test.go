package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type firstTrackingBlockedCapture struct {
	outbound.SessionCapture
	entered, release chan struct{}
}

func (b firstTrackingBlockedCapture) Project(ctx context.Context, root, path string, source outbound.CaptureSource, codec outbound.ProviderCodec, pending bool) (domain.Envelope, domain.ContentHash, int64, *time.Time, error) {
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
		return domain.Envelope{}, "", 0, nil, ctx.Err()
	}
	return b.SessionCapture.Project(ctx, root, path, source, codec, pending)
}

func TestFirstTrackingAdmissionWaitsForRealCapture(t *testing.T) {
	for _, name := range []string{"Save", "Stage", "Stash"} {
		t.Run(name, func(t *testing.T) {
			f := newStagingFixture(t)
			source := f.source(t, "test-owned-session", "synthetic fixture message")
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			f.svc.save.capture = firstTrackingBlockedCapture{f.svc.save.capture, entered, release}
			go func() {
				if name == "Stage" {
					_, err := f.svc.Stage(context.Background(), inbound.StageInput{Cwd: f.root, Sessions: []inbound.StageSession{source}})
					done <- err
				} else if name == "Stash" {
					stash := NewStashService(f.git, f.svc.save.captures, f.svc.save.codecs, f.store, nil, f.svc.save.capture)
					_, err := stash.Stash(context.Background(), inbound.StashInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path})
					done <- err
				} else {
					_, err := f.svc.save.Save(context.Background(), inbound.SaveInput{Cwd: f.root, Provider: source.Provider, SessionPath: source.Path, Pending: true})
					done <- err
				}
			}()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			_, err := f.store.TrackingPristine(ctx, f.git.repo.ID)
			cancel()
			close(release)
			captureErr := <-done
			if !errors.Is(err, context.DeadlineExceeded) || captureErr != nil {
				t.Fatalf("capture admission ordering: %v, capture=%v", err, captureErr)
			}
			pristine, err := f.store.TrackingPristine(context.Background(), f.git.repo.ID)
			if err != nil || pristine {
				t.Fatalf("durable capture escaped pristine check: %v %v", pristine, err)
			}
		})
	}
}
