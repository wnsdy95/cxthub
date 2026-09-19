package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"testing"
	"time"
)

type failingCapture struct {
	outbound.SessionCapture
	err error
}

func (f failingCapture) Eligible(string, string) bool { return true }
func (f failingCapture) Project(context.Context, string, string, outbound.CaptureSource, outbound.ProviderCodec, bool) (domain.Envelope, domain.ContentHash, int64, *time.Time, error) {
	return domain.Envelope{}, "", 0, nil, f.err
}

type portRepo struct{ outbound.GitContext }

func (portRepo) CurrentRepo(context.Context, string) (domain.Repo, error) {
	return domain.Repo{ID: "test", LocalPath: "virtual"}, nil
}

type failingOutbox struct {
	outbound.SyncOutbox
	err error
}

func (f failingOutbox) WithGrafts(context.Context, string, func(outbound.GraftQueueAccess) error) error {
	return f.err
}
func TestCaptureFailureDoesNotFallBackToFilesystemOrStore(t *testing.T) {
	cause := errors.New("projection unavailable")
	service := NewSaveSessionService(portRepo{}, map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: nil}, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: nil}, nil, failingCapture{err: cause}, nil)
	_, err := service.Save(context.Background(), inbound.SaveInput{Cwd: "virtual", SessionPath: "not-a-file"})
	if !errors.Is(err, cause) {
		t.Fatalf("capture port failure: %v", err)
	}
}
func TestGraftOutboxFailurePrecedesAnySnapshotReadOrWrite(t *testing.T) {
	cause := errors.New("queue unavailable")
	service := NewSaveSessionService(nil, nil, nil, nil, nil, failingOutbox{err: cause})
	if err := service.graftLocalAndQueue(context.Background(), "virtual", domain.HashContent([]byte("h")), domain.HashContent([]byte("p"))); !errors.Is(err, cause) {
		t.Fatalf("queue port failure: %v", err)
	}
}
