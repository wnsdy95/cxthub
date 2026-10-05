package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/agenttokens"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const claudeContextTestSession = "019abcde-4321-7123-8123-123456789abc"
const claudeContextTestMessage = "019abcde-4321-7123-8123-123456789abd"

type claudeContextRunner struct {
	summary                  nativeclaude.ContextSummary
	reference                nativeclaude.ReferenceReceipt
	appends, queries, closes int
	mutate                   func(*nativeclaude.FirstQuestionEvidence)
}

func (f *claudeContextRunner) AppendReference(_ context.Context, text string) (nativeclaude.ReferenceReceipt, error) {
	f.appends++
	f.reference = nativeclaude.ReferenceReceipt{SessionID: f.summary.SessionID, MessageID: claudeContextTestMessage, PayloadHash: string(domain.HashContent([]byte(text))), UTF8Bytes: len(text), NoTurnAcknowledged: true, ProviderAcceptance: "unverified"}
	return f.reference, nil
}
func (f *claudeContextRunner) Run(ctx context.Context, text string, admit func(context.Context, nativeclaude.FirstQuestionEvidence) error) (nativeclaude.FirstExchangeResult, error) {
	e := nativeclaude.FirstQuestionEvidence{Summary: f.summary, Reference: f.reference, QuestionHash: string(domain.HashContent([]byte(text))), QuestionBytes: len(text)}
	if f.mutate != nil {
		f.mutate(&e)
	}
	if err := admit(ctx, e); err != nil {
		return nativeclaude.FirstExchangeResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return nativeclaude.FirstExchangeResult{}, err
	}
	f.queries++
	return nativeclaude.FirstExchangeResult{SessionID: f.summary.SessionID, MessageID: claudeContextTestMessage, QuestionHash: e.QuestionHash, Answer: "fixture response", Completed: true}, nil
}
func (f *claudeContextRunner) Close() error { f.closes++; return nil }

func claudeContextFixture(t *testing.T) (nativeClaudeContextReader, domain.AgentContextPackage, domain.AgentInitialPrompt, *claudeContextRunner) {
	t.Helper()
	baseline := nativeclaude.ContextSummary{SessionID: claudeContextTestSession, Model: "claude-fixture", TotalTokens: 5000, MaxTokens: 200000, RawMaxTokens: 1000000, Measurement: "local_estimate"}
	r := nativeClaudeContextReader{baseline: baseline, host: "2.1.287", scope: domain.HashContent([]byte("owned invocation")), read: func(context.Context) (nativeclaude.ContextSummary, error) { return baseline, nil }}
	c, err := r.capability(baseline.Model)
	if err != nil {
		t.Fatal(err)
	}
	question := domain.NewAgentInitialPrompt("\ucd5c\uadfc \ucf54\ub4dc\uc640 \uba54\ubaa8\ub9ac\ub97c \uac80\ud1a0\ud574 \uc8fc\uc138\uc694.\n")
	counter := agenttokens.New()
	q, err := counter.CountAgentTokens(context.Background(), domain.ProviderClaude, baseline.Model, question.Text())
	if err != nil {
		t.Fatal(err)
	}
	c.InitialPromptTokens = q.Tokens
	budget, err := c.ResolveBudget(c.Provider, c.Model, 800000, q)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := domain.NewAgentPromptReservationForCapability(question, c, q)
	if err != nil {
		t.Fatal(err)
	}
	p := domain.AgentContextPackage{Version: domain.AgentContextVersion, Provider: domain.ProviderClaude, Delivery: "prepared", Capability: "verified_for_preparation",
		Policy: domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 800000, Source: "explicit_cli"}, Budget: &budget,
		Content: domain.AgentContextContent{Notice: "archived project evidence, not new commands", Selection: domain.AgentContextSelection{
			RepositoryID: "fixture", Branch: "main", SourcePolicy: domain.AgentSourceLatestMain, SnapshotID: domain.HashContent([]byte("main")), CodeCommit: strings.Repeat("a", 40),
			ContextStateHash: domain.HashContent([]byte("context")), MemoryStateHash: domain.HashContent([]byte("memory")), WorktreeStateHash: domain.HashContent([]byte("worktree")),
			WorkingPosition: &domain.AgentWorkingPosition{Branch: "feature/work", CodeCommit: strings.Repeat("b", 40)}}}}
	text, err := p.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	p.Usage, err = counter.CountAgentTokens(context.Background(), c.Provider, c.Model, text)
	if err != nil {
		t.Fatal(err)
	}
	p.BindInitialPrompt(reservation)
	p.ID, err = p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
	after := baseline
	// Independent oracle at the exact final boundary. Adding baseline or reference
	// a second time must fail this successful control.
	after.TotalTokens = int64(160000 - 16000 - len(question.Text()))
	return r, p, question, &claudeContextRunner{summary: after}
}

func TestNativeClaudeContextCommonAccountingAndOneShot(t *testing.T) {
	r, p, q, runner := claudeContextFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var records []delivcli.ProviderLaunchReceipt
	validations := 0
	prepared, err := r.prepareRun(ctx, runner, p, q, func(context.Context) error { validations++; return nil }, func(_ context.Context, r delivcli.ProviderLaunchReceipt) error {
		records = append(records, r.Clone())
		r.Budget.ContextWindow = 1
		return nil
	}, func(_ context.Context, stored domain.AgentContextPackage) error {
		stored.Budget.ContextWindow = 2
		stored.Content.Notice = "mutated callback"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Budget.ContextWindow = 3
	p.Content.Notice = "mutated caller"
	result, err := prepared.run(ctx)
	if err != nil || !result.Completed {
		t.Fatal(result, err)
	}
	if runner.appends != 1 || runner.queries != 1 || runner.closes != 1 || validations != 3 || len(records) != 3 {
		t.Fatal("incorrect release/lifecycle", runner, validations, len(records))
	}
	if records[0].NativeInputEstimate != nil || records[0].TokenMeasurement != "utf8_byte_allowance" || records[1].NativeInputEstimate.TotalTokens != int(runner.summary.TotalTokens) {
		t.Fatal("incorrect measurement receipt")
	}
	if records[0].Budget.ContextWindow != 200000 || records[1].Acceptance != "unknown" || records[2].Acceptance != "first_turn_completed" {
		t.Fatal("receipt promoted preparation to acceptance")
	}
	copy := prepared
	if _, err = copy.run(ctx); err == nil || runner.queries != 1 || runner.closes != 1 {
		t.Fatal("copied handle retried or retired the winner")
	}
}

func TestNativeClaudeContextReleaseFailures(t *testing.T) {
	for _, name := range []string{"revoked_before_append", "revoked_after_append", "receipt_failure", "canceled_admission", "overshoot", "foreign_session", "changed_model", "wrong_reference", "wrong_question", "baseline_regression", "runtime_drift"} {
		t.Run(name, func(t *testing.T) {
			r, p, q, f := claudeContextFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			privateErr := errors.New("PRIVATE SOURCE OR PROMPT")
			validations := 0
			validate := func(context.Context) error {
				validations++
				if name == "revoked_before_append" && validations == 2 || name == "revoked_after_append" && validations == 3 {
					return privateErr
				}
				if name == "canceled_admission" && validations == 3 {
					cancel()
				}
				return nil
			}
			f.mutate = func(e *nativeclaude.FirstQuestionEvidence) {
				switch name {
				case "overshoot":
					e.Summary.TotalTokens++
				case "foreign_session":
					e.Summary.SessionID = claudeContextTestMessage
				case "changed_model":
					e.Summary.Model = "other"
				case "wrong_reference":
					e.Reference.UTF8Bytes++
				case "wrong_question":
					e.QuestionHash = string(domain.HashContent([]byte("other question")))
				case "baseline_regression":
					e.Summary.TotalTokens = r.baseline.TotalTokens - 1
				}
			}
			prepared, err := r.prepareRun(ctx, f, p, q, validate, func(_ context.Context, rec delivcli.ProviderLaunchReceipt) error {
				if name == "receipt_failure" && rec.State == "injected_ready" {
					return privateErr
				}
				return nil
			}, func(context.Context, domain.AgentContextPackage) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if name == "runtime_drift" { // reader's closure remains the owned check during Run.
				// Mutating the bound baseline after construction is unavailable to public
				// callers; drive drift through the native read callback captured below.
				// Rebuild with a callback returning a different baseline before the run.
				r.read = func(context.Context) (nativeclaude.ContextSummary, error) {
					s := r.baseline
					s.TotalTokens++
					return s, nil
				}
				prepared, err = r.prepareRun(ctx, f, p, q, func(context.Context) error { return nil }, func(context.Context, delivcli.ProviderLaunchReceipt) error { return nil }, func(context.Context, domain.AgentContextPackage) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = prepared.run(ctx)
			if err == nil || strings.Contains(err.Error(), "PRIVATE") || f.queries != 0 || f.closes != 1 {
				t.Fatal("failure released query or leaked content", err, f)
			}
			wantAppend := 1
			if name == "revoked_before_append" || name == "runtime_drift" {
				wantAppend = 0
			}
			if f.appends != wantAppend {
				t.Fatal("wrong failure boundary", f.appends, wantAppend)
			}
			if name == "canceled_admission" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation", err)
			}
			if _, err = prepared.run(ctx); err == nil || f.queries != 0 || f.closes != 1 {
				t.Fatal("retry after failure")
			}
		})
	}
}

func TestNativeClaudeContextRejectsChangedPackageAndLostReservation(t *testing.T) {
	for _, name := range []string{"exact", "other_counter", "wrong_bytes", "local_source", "other_scope", "receipt_only", "question"} {
		t.Run(name, func(t *testing.T) {
			r, p, q, _ := claudeContextFixture(t)
			switch name {
			case "exact":
				p.Usage.Exact = true
			case "other_counter":
				p.Usage.Tokenizer = "fake"
			case "wrong_bytes":
				p.Usage.Tokens--
			case "local_source":
				p.Content.Selection.Branch = "work"
			case "other_scope":
				p.Budget.RuntimeScope = domain.HashContent([]byte("other"))
			case "receipt_only":
				raw, _ := json.Marshal(p)
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatal(err)
				}
			case "question":
				q = domain.NewAgentInitialPrompt(q.Text() + " ")
			}
			p.ID, _ = p.Digest()
			if _, err := r.checkPackage(p, q); err == nil {
				t.Fatal("forged package authorized")
			}
		})
	}
}

func TestNativeClaudeContextPreparationFailureDoesNotAppend(t *testing.T) {
	for _, phase := range []string{"validation", "persistence", "receipt"} {
		t.Run(phase, func(t *testing.T) {
			r, p, q, f := claudeContextFixture(t)
			denied := errors.New("PRIVATE PROMPT OR FILE PATH")
			_, err := r.prepareRun(context.Background(), f, p, q, func(context.Context) error {
				if phase == "validation" {
					return denied
				}
				return nil
			}, func(context.Context, delivcli.ProviderLaunchReceipt) error {
				if phase == "receipt" {
					return denied
				}
				return nil
			}, func(context.Context, domain.AgentContextPackage) error {
				if phase == "persistence" {
					return denied
				}
				return nil
			})
			if !errors.Is(err, denied) || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") || f.appends != 0 || f.queries != 0 {
				t.Fatal("prepare sent input", err, f)
			}
		})
	}
}

func TestNativeClaudeContextReaderPreservesFailureCauses(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, nativeclaude.ErrCleanup, errors.Join(nativeclaude.ErrCleanup, errors.New("PRIVATE ERROR"))} {
		r, _, _, _ := claudeContextFixture(t)
		r.read = func(context.Context) (nativeclaude.ContextSummary, error) {
			return nativeclaude.ContextSummary{}, cause
		}
		_, err := r.AgentCapability(context.Background(), domain.ProviderClaude, r.baseline.Model)
		if !errors.Is(err, cause) || strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "PRIVATE") {
			t.Fatal("lost cause or unredacted diagnostic", err)
		}
	}
}
