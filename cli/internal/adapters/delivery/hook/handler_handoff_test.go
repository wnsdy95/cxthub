package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type hookHandoffPreparer struct {
	inputs                    []inbound.PrepareAgentContextInput
	selections                []domain.AgentContextSelection
	text                      string
	prepareErr, validationErr error
	mutate                    func(*domain.AgentContextPackage)
	onValidate                func()
}

func (f *hookHandoffPreparer) PrepareAgentContext(_ context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
	f.inputs = append(f.inputs, in)
	p := domain.AgentContextPackage{
		Version: domain.AgentContextVersion, Provider: in.Provider, Policy: in.Policy,
		Delivery: "prepared", ArtifactOnly: in.ArtifactOnly,
		Content: domain.AgentContextContent{Notice: f.text, Selection: domain.AgentContextSelection{
			RepositoryID: string(domain.HashContent([]byte("repo"))), Branch: "main",
			SnapshotID: domain.HashContent([]byte("latest server main")), CodeCommit: strings.Repeat("a", 40),
			ContextStateHash: domain.HashContent([]byte("context")), MemoryStateHash: domain.HashContent([]byte("memory")),
			SourcePolicy: domain.AgentSourceLatestMain, WorktreeStateHash: domain.HashContent([]byte("worktree")),
			WorkingPosition: &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("b", 40)},
		}},
	}
	p.ID, _ = p.Digest()
	if f.mutate != nil {
		f.mutate(&p)
	}
	return p, f.prepareErr
}

func (f *hookHandoffPreparer) ValidateAgentContextDelivery(_ context.Context, _ string, selection domain.AgentContextSelection) error {
	f.selections = append(f.selections, selection)
	if f.onValidate != nil {
		f.onValidate()
	}
	return f.validationErr
}

type hookPrepareOnly struct{ preparer inbound.PrepareAgentContext }

func (f hookPrepareOnly) PrepareAgentContext(ctx context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
	return f.preparer.PrepareAgentContext(ctx, in)
}

type hookHandoffWriter func([]byte) (int, error)

func (f hookHandoffWriter) Write(raw []byte) (int, error) { return f(raw) }

func handoffContext(t *testing.T, raw []byte, event string) string {
	t.Helper()
	var out struct {
		Output struct {
			Event string `json:"hookEventName"`
			Text  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Output.Event != event {
		t.Fatalf("invalid provider JSON: %s (%v)", raw, err)
	}
	return out.Output.Text
}

func TestHandlerHandoffPreparesFreshMainForBothProviders(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
			t.Run(string(provider)+"/"+event, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				cwd := t.TempDir()
				initHookContext(t, cwd)
				const session = "11111111-1111-4111-8111-111111111111"
				path := writeHookSession(t, cwd, provider, session)
				original, _ := os.ReadFile(path)
				if err := capture.WriteSessionHandoff(cwd, []string{session}, "OLD QUEUED PRIVATE BODY"); err != nil {
					t.Fatal(err)
				}
				if err := capture.WriteSessionHandoff(cwd, []string{"another-session"}, "OTHER SESSION BODY"); err != nil {
					t.Fatal(err)
				}
				prepared := &hookHandoffPreparer{text: "FRESH AUTHORIZED SERVER MAIN"}
				var output bytes.Buffer
				h := NewHandler(capture.NewCaptureCoordinator(&recSave{}, domain.TeamIdentity{})).WithAgentContext(prepared)
				payload, _ := json.Marshal(hookPayload{Cwd: cwd, SessionID: session, TranscriptPath: path, Prompt: "continue"})
				h.stdin, h.stdout = bytes.NewReader(payload), &output
				if err := h.Run(provider, event); err != nil {
					t.Fatal(err)
				}
				text := handoffContext(t, output.Bytes(), event)
				if !strings.Contains(text, prepared.text) || strings.Contains(text, "QUEUED") || strings.Contains(text, "OTHER SESSION") || len(text) > appHandoffMaxBytes {
					t.Fatalf("wrong handoff: %s", text)
				}
				want := inbound.PrepareAgentContextInput{Cwd: cwd, Provider: provider, LatestMain: true, ArtifactOnly: true, Policy: domain.MemoryInputPolicy()}
				if len(prepared.inputs) != 1 || !reflect.DeepEqual(prepared.inputs[0], want) || len(prepared.selections) != 1 || prepared.selections[0].SnapshotID != domain.HashContent([]byte("latest server main")) {
					t.Fatalf("wrong preparation/validation: %+v %+v", prepared.inputs, prepared.selections)
				}
				output.Reset()
				h.stdin = bytes.NewReader(payload)
				if err := h.Run(provider, event); err != nil || output.Len() != 0 || len(prepared.inputs) != 1 {
					t.Fatalf("handoff repeated: %v %s", err, output.String())
				}
				if text, ok := capture.ConsumeSessionHandoff(cwd, "another-session"); !ok || text != "OTHER SESSION BODY" {
					t.Fatal("other session queue consumed")
				}
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(original, after) {
					t.Fatal("active conversation changed", err)
				}
			})
		}
	}
}

func TestHandlerHandoffFailuresKeepRequestForFreshRetry(t *testing.T) {
	denied := errors.New("server access denied")
	writeFailure := errors.New("output failed")
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		for _, failure := range []string{"denied", "stale", "bad identity", "historical source", "oversized", "no preparer", "no validator", "short write", "write error", "canceled validation"} {
			t.Run(string(provider)+"/"+failure, func(t *testing.T) {
				cwd := t.TempDir()
				initHookContext(t, cwd)
				if err := capture.WriteSessionHandoff(cwd, []string{"session"}, "OLD QUEUED BODY"); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				prepared := &hookHandoffPreparer{text: "FIRST PREPARATION"}
				var output bytes.Buffer
				h := NewHandler(nil).WithAgentContext(prepared)
				h.stdout = &output
				want := domain.ErrAgentContextUnavailable
				switch failure {
				case "denied":
					prepared.prepareErr, want = denied, denied
				case "stale":
					prepared.validationErr, want = domain.ErrSelectionChanged, domain.ErrSelectionChanged
				case "bad identity":
					prepared.mutate = func(p *domain.AgentContextPackage) { p.Content.Notice = "TAMPERED" }
					want = domain.ErrHashMismatch
				case "historical source":
					prepared.mutate = func(p *domain.AgentContextPackage) {
						p.Content.Selection.SourcePolicy, p.Content.Selection.WorkingPosition = "", nil
						p.ID, _ = p.Digest()
					}
				case "oversized":
					prepared.text = strings.Repeat("x", appHandoffMaxBytes)
					want = domain.ErrContextBudgetExceeded
				case "no preparer":
					h.WithAgentContext(nil)
				case "no validator":
					h.WithAgentContext(hookPrepareOnly{prepared})
				case "short write":
					h.stdout, want = shortHookWriter{}, io.ErrShortWrite
				case "write error":
					h.stdout = hookHandoffWriter(func([]byte) (int, error) { return 0, writeFailure })
					want = writeFailure
				case "canceled validation":
					prepared.onValidate = cancel
					want = context.Canceled
				}
				if err := h.emitBriefing(ctx, provider, "UserPromptSubmit", cwd, "session"); !errors.Is(err, want) || output.Len() != 0 {
					t.Fatalf("failure emitted or wrong error: %v, output=%s", err, output.String())
				}
				retry := &hookHandoffPreparer{text: "NEW MAIN ON RETRY"}
				h.WithAgentContext(retry)
				h.stdout = &output
				if err := h.emitBriefing(context.Background(), provider, "UserPromptSubmit", cwd, "session"); err != nil {
					t.Fatal(err)
				}
				text := handoffContext(t, output.Bytes(), "UserPromptSubmit")
				if len(retry.inputs) != 1 || len(retry.selections) != 1 || !strings.Contains(text, retry.text) || strings.Contains(text, "OLD QUEUED") || strings.Contains(text, prepared.text) {
					t.Fatalf("queue lost or stale retry: %s", text)
				}
				if _, ok := capture.ConsumeSessionHandoff(cwd, "session"); ok {
					t.Fatal("successful retry not acknowledged")
				}
			})
		}
	}
}

type hookHandoffNotices struct {
	notice domain.SessionNotice
	before func()
	acked  bool
}

func (n *hookHandoffNotices) DeliverSessionNotice(_ context.Context, _ inbound.SessionNoticeInput, deliver func(domain.SessionNotice) error) error {
	if n.acked {
		return nil
	}
	if n.before != nil {
		n.before()
	}
	if err := deliver(n.notice); err != nil {
		return err
	}
	n.acked = true
	return nil
}

func TestHandlerHandoffFailurePreservesIndependentNotices(t *testing.T) {
	for _, failure := range []string{"preparation", "revalidation", "write"} {
		t.Run(failure, func(t *testing.T) {
			cwd := t.TempDir()
			initHookContext(t, cwd)
			if err := capture.WriteSessionHandoff(cwd, []string{"session"}, "OLD QUEUED BODY"); err != nil {
				t.Fatal(err)
			}
			pullID := domain.HashContent([]byte("pull source"))
			if err := capture.WritePullBriefing(cwd, "main", []domain.ContentHash{pullID}); err != nil {
				t.Fatal(err)
			}
			prepared := &hookHandoffPreparer{text: "FRESH PROJECT MEMORY"}
			notices := &hookHandoffNotices{notice: domain.SessionNotice{ID: domain.HashContent([]byte("local selection"))}}
			h := NewHandler(nil).WithAgentContext(prepared).WithSessionNotices(notices)
			var output bytes.Buffer
			h.stdout = &output
			want := domain.ErrSelectionChanged
			switch failure {
			case "preparation":
				prepared.prepareErr = want
			case "revalidation":
				// A source move during notice preparation must be caught before output.
				notices.before = func() { prepared.validationErr = want }
			case "write":
				h.stdout, want = shortHookWriter{}, io.ErrShortWrite
			}
			if err := h.emitBriefing(context.Background(), domain.ProviderClaude, "SessionStart", cwd, "session"); !errors.Is(err, want) {
				t.Fatal("wrong failure", err)
			}
			if failure == "write" {
				if notices.acked {
					t.Fatal("failed notice output acknowledged")
				}
				h.stdout = &output
				if err := h.emitBriefing(context.Background(), domain.ProviderClaude, "SessionStart", cwd, "session"); err != nil || !notices.acked {
					t.Fatal("notice/handoff retry failed", err)
				}
				text := handoffContext(t, output.Bytes(), "SessionStart")
				if !strings.Contains(text, prepared.text) || !strings.Contains(text, string(notices.notice.ID)) {
					t.Fatal("retry lost notice or fresh handoff", text)
				}
				return
			}
			text := handoffContext(t, output.Bytes(), "SessionStart")
			if !notices.acked || !strings.Contains(text, "identifiers only") || !strings.Contains(text, string(pullID)) || !strings.Contains(text, string(notices.notice.ID)) || strings.Contains(text, prepared.text) || strings.Contains(text, "OLD QUEUED") {
				t.Fatalf("independent notices lost or memory leaked: %s", text)
			}
			if text, ok := capture.ConsumeSessionHandoff(cwd, "session"); !ok || text != "OLD QUEUED BODY" {
				t.Fatal("failed handoff was acknowledged")
			}
		})
	}
}

func TestHandlerHandoffAcknowledgementKeepsReplacement(t *testing.T) {
	cwd := t.TempDir()
	initHookContext(t, cwd)
	if err := capture.WriteSessionHandoff(cwd, []string{"session"}, "original request"); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil).WithAgentContext(&hookHandoffPreparer{text: "fresh main"})
	h.stdout = hookHandoffWriter(func(raw []byte) (int, error) {
		if err := capture.WriteSessionHandoff(cwd, []string{"session"}, "replacement request"); err != nil {
			return 0, err
		}
		return len(raw), nil
	})
	if err := h.emitBriefing(context.Background(), domain.ProviderCodex, "UserPromptSubmit", cwd, "session"); err != nil {
		t.Fatal(err)
	}
	if text, ok := capture.ConsumeSessionHandoff(cwd, "session"); !ok || text != "replacement request" {
		t.Fatal("replacement was acknowledged by previous delivery")
	}
}
