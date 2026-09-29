package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type rejectLiveManualCapture struct{ t *testing.T }

func (r rejectLiveManualCapture) Save(context.Context, inbound.SaveInput) (inbound.SaveOutput, error) {
	r.t.Fatal("manual commit attempted legacy live session capture")
	return inbound.SaveOutput{}, nil
}

// Exercise public argument dispatch, exact source discovery and the real frozen
// staging service together. The original provider file is gone at commit time.
func TestManualCommitUsesFrozenAddForBothProviders(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			for _, name := range []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CXT_WRAPPED", "CXT_WRAPPED_AGENT", "CXT_WRAPPED_SESSION_ID", "CXT_WRAPPER_PID"} {
				t.Setenv(name, "")
			}
			cwd, c, _, repo, baseline := historyFixture(t)
			t.Chdir(cwd)
			git := gitctx.NewGitContextAdapter()
			sha, err := git.CurrentCommit(ctx, cwd)
			if err != nil {
				t.Fatal(err)
			}
			store := storage.NewWorktreeFileStore(cwd, filepath.Join(cwd, ".git"), "main", sha)
			c.Staging = app.NewStagingService(git, git, store, store,
				map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: capture.NewClaudeCapture(), domain.ProviderCodex: capture.NewCodexCapture()},
				map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec(), domain.ProviderCodex: codec.NewCodexCodec()},
				capture.NewSessionCapture(store), storage.NewSyncOutbox())
			c.Save = rejectLiveManualCapture{t}
			const session = "11111111-1111-4111-8111-111111111111"
			var path string
			if provider == domain.ProviderCodex {
				path = writeCodexRollout(t, home, cwd, session, time.Now())
			} else {
				path = filepath.Join(home, ".claude", "projects", providerfs.EncodeCwd(cwd), session+".jsonl")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			appendStagingPrompt(t, path, provider, cwd, session, "frozen at add")
			if err := Run(c, []string{"cxt", "commit", "-m", "must not capture"}); !errors.Is(err, domain.ErrEmptyIndex) {
				t.Fatalf("manual commit without add: %v", err)
			}
			ref, err := store.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || ref.Target != baseline {
				t.Fatalf("empty commit moved the branch: %+v %v", ref, err)
			}
			if err := Run(c, []string{"cxt", "add", string(provider)}); err != nil {
				t.Fatal(err)
			}
			index, err := c.Staging.Inspect(ctx, cwd)
			if err != nil || len(index.Entries) != 1 {
				t.Fatalf("add did not freeze the selected provider: %+v %v", index, err)
			}
			frozen := index.Entries[0]
			appendStagingPrompt(t, path, provider, cwd, session, "not staged after add")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := Run(c, []string{"cxt", "commit", "-m", "frozen only", "--expect", string(index.Revision)}); err != nil {
				t.Fatalf("commit depended on deleted provider file: %v", err)
			}
			ref, err = store.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || ref.Target != frozen.DocHash {
				t.Fatalf("wrong frozen context published: %+v %v", ref, err)
			}
			doc, err := store.GetDoc(ctx, ref.Target)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(doc)
			if err != nil || !strings.Contains(string(raw), "frozen at add") || strings.Contains(string(raw), "not staged after add") {
				t.Fatalf("commit changed add-time content: %s %v", raw, err)
			}
			index, err = c.Staging.Inspect(ctx, cwd)
			if err != nil || len(index.Entries) != 0 {
				t.Fatalf("successful commit did not consume the index: %+v %v", index, err)
			}
			operations, err := store.ListStagingCommits(ctx, repo)
			if err != nil || len(operations) != 1 || !operations[0].LocalFinalized {
				t.Fatalf("missing durable local receipt: %+v %v", operations, err)
			}
			before, err := store.ListHistoryEvents(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := Run(c, []string{"cxt", "commit", "--resume", operations[0].ID}); err != nil {
				t.Fatal(err)
			}
			after, err := store.ListHistoryEvents(ctx, repo)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("retry duplicated publication history: %v", err)
			}
			if err := Run(c, []string{"cxt", "commit"}); !errors.Is(err, domain.ErrEmptyIndex) {
				t.Fatalf("consumed index was reused: %v", err)
			}
		})
	}
}

func appendStagingPrompt(t *testing.T, path string, provider domain.ProviderKind, cwd, session, text string) {
	t.Helper()
	var row any
	if provider == domain.ProviderCodex {
		row = map[string]any{"type": "response_item", "timestamp": "2026-09-30T00:00:00Z", "payload": map[string]any{
			"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": text}},
		}}
	} else {
		row = map[string]any{"type": "user", "sessionId": session, "cwd": cwd, "gitBranch": "main", "timestamp": "2026-09-30T00:00:00Z", "message": map[string]string{"role": "user", "content": text}}
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
