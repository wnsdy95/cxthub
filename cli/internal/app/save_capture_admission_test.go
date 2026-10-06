package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type saveAdmissionStore struct {
	outbound.SessionStore
	calls        int
	admissionErr error
	inGate       bool
}

func (s *saveAdmissionStore) EnsureCapturePosition(context.Context, string) error {
	s.calls++
	if s.inGate {
		panic("admission attempted while shared capture gate held")
	}
	return s.admissionErr
}
func (s *saveAdmissionStore) WithCaptureTrackingGate(ctx context.Context, fn func(context.Context) error) error {
	s.inGate = true
	defer func() { s.inGate = false }()
	return fn(ctx)
}

type saveAdmissionGit struct {
	outbound.GitContext
	calls   int
	repo    domain.Repo
	changed domain.Repo
}

func (g *saveAdmissionGit) CurrentRepo(context.Context, string) (domain.Repo, error) {
	g.calls++
	if g.calls > 1 {
		return g.changed, nil
	}
	return g.repo, nil
}
func TestSaveAdmissionRejectsBeforeReadingSession(t *testing.T) {
	for _, mode := range []string{"admission-rejected", "connection-changed"} {
		t.Run(mode, func(t *testing.T) {
			a := domain.Repo{ID: string(domain.HashContent([]byte("A"))), LocalPath: t.TempDir()}
			b := a
			b.ID = string(domain.HashContent([]byte("B")))
			git := &saveAdmissionGit{repo: a, changed: b}
			store := &saveAdmissionStore{}
			if mode == "admission-rejected" {
				store.admissionErr = domain.ErrSelectionChanged
			}
			// No capture or codec is available: reaching provider lookup would return
			// ErrUnsupportedProvider. Selection failure must take precedence.
			svc := NewSaveSessionService(git, nil, nil, store, nil, nil)
			_, err := svc.Save(context.Background(), inbound.SaveInput{Cwd: a.LocalPath, Provider: domain.ProviderClaude})
			if !errors.Is(err, domain.ErrSelectionChanged) || store.calls != 1 {
				t.Fatalf("err=%v admissions=%d", err, store.calls)
			}
			if mode == "admission-rejected" && git.calls != 1 {
				t.Fatal("continued after rejected admission")
			}
		})
	}
}
