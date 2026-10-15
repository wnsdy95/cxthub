package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

func TestCodexPendingPublicationRevalidatesRolloverAndRetirement(t *testing.T) {
	for _, scenario := range []string{"cwd-rollover", "ledger-retirement"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newStagingFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "init", "-q", "-b", "main", fixture.root)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git init: %s %v", output, err)
			}
			const session = "11111111-1111-4111-8111-111111111111"
			const instance = "22222222-2222-4222-8222-222222222222"
			write := func(name, cwd, timestamp, message string) string {
				t.Helper()
				path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "10", "09", "rollout-"+name+".jsonl")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": session, "cwd": cwd, "timestamp": timestamp}})
				if err != nil {
					t.Fatal(err)
				}
				event, err := json.Marshal(map[string]any{"type": "response_item", "timestamp": timestamp, "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": message}}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(append(append(header, '\n'), event...), '\n'), 0600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			old := write("old-"+session, fixture.root, "2026-07-31T00:00:00Z", "old-only history")
			paused, release := make(chan struct{}), make(chan struct{})
			fixture.svc.save.capture = pausedGenerationCapture{SessionCapture: fixture.svc.save.capture, path: old, paused: paused, release: release}
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				_, err := fixture.svc.save.Save(ctx, inbound.SaveInput{Cwd: fixture.root, Provider: domain.ProviderCodex, SessionPath: old, Pending: true})
				done <- err
			}()
			defer func() {
				cancel()
				<-finished
			}()
			select {
			case <-paused:
			case err := <-done:
				t.Fatalf("old capture did not reach publication pause: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var newer inbound.SaveOutput
			wantErr := capture.ErrSessionIdentityMismatch
			if scenario == "cwd-rollover" {
				subdir := filepath.Join(fixture.root, "subdir")
				if err := os.Mkdir(subdir, 0700); err != nil {
					t.Fatal(err)
				}
				current := write("new-"+session+"_"+instance, subdir, "2026-10-09T00:00:00Z", "new generation")
				var err error
				newer, err = fixture.svc.save.Save(ctx, inbound.SaveInput{Cwd: fixture.root, Provider: domain.ProviderCodex, SessionPath: current, Pending: true})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				wantErr = domain.ErrNoActiveSession
				if err := providerfs.MarkSuperseded(fixture.root, old); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-done; !errors.Is(err, wantErr) {
				t.Fatalf("obsolete capture publication error = %v, want %v", err, wantErr)
			}
			pendings, err := fixture.store.ListPendings(ctx, fixture.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cwd-rollover" {
				if len(pendings) != 1 || pendings[0].Target != newer.SnapshotID {
					t.Fatalf("new pending was replaced: %+v", pendings)
				}
			} else if len(pendings) != 0 {
				t.Fatalf("retired capture became pending: %+v", pendings)
			}
		})
	}
}
