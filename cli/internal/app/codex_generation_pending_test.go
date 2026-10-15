package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type pausedGenerationCapture struct {
	outbound.SessionCapture
	path    string
	paused  chan struct{}
	release chan struct{}
}

func (p pausedGenerationCapture) Project(ctx context.Context, root, path string, source outbound.CaptureSource, codec outbound.ProviderCodec, partial bool, identity domain.DocumentIdentity) (domain.Envelope, domain.DocumentRef, int64, *time.Time, error) {
	envelope, doc, size, activity, err := p.SessionCapture.Project(ctx, root, path, source, codec, partial, identity)
	if path == p.path && err == nil {
		close(p.paused)
		select {
		case <-ctx.Done():
			return envelope, doc, size, activity, ctx.Err()
		case <-p.release:
		}
	}
	return envelope, doc, size, activity, err
}

func TestLateCodexGenerationCannotReplaceNewPending(t *testing.T) {
	f := newStagingFixture(t)
	const session = "11111111-1111-4111-8111-111111111111"
	const instance = "22222222-2222-4222-8222-222222222222"
	write := func(name, timestamp, message string) string {
		t.Helper()
		path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "10", "09", "rollout-"+name+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": session, "cwd": f.root, "timestamp": timestamp}})
		event, _ := json.Marshal(map[string]any{"type": "response_item", "timestamp": timestamp, "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": message}}}})
		if err := os.WriteFile(path, append(append(append(header, '\n'), event...), '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	old := write("old-"+session, "2026-07-31T00:00:00Z", "old-only history")
	paused, release := make(chan struct{}), make(chan struct{})
	f.svc.save.capture = pausedGenerationCapture{SessionCapture: f.svc.save.capture, path: old, paused: paused, release: release}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: domain.ProviderCodex, SessionPath: old, Pending: true})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("old capture failed before publication pause: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-paused:
	}
	current := write("new-"+session+"_"+instance, "2026-10-09T00:00:00Z", "new generation")
	newer, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: domain.ProviderCodex, SessionPath: current, Pending: true})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, capture.ErrSessionIdentityMismatch) {
		t.Fatalf("stale publication accepted: %v", err)
	}
	pendings, err := f.store.ListPendings(ctx, f.git.repo.ID)
	if err != nil || len(pendings) != 1 || pendings[0].Target != newer.SnapshotID {
		t.Fatalf("new pending replaced: %+v %v", pendings, err)
	}
}
