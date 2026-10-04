package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativecodex"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const nativeGenerationTestHost = "cxthub_native_transport/0.157.1 synthetic-fixture"
const nativeGenerationTestTurn = "019abcde-4321-7123-8123-123456789abc"

type nativeGenerationCalibrationFixture struct {
	current  domain.AgentInputCalibration
	writes   int
	fail     error
	discard  bool
	response func(domain.AgentInputCalibration) domain.AgentInputCalibration
}

func (s *nativeGenerationCalibrationFixture) ReadAgentInputCalibration(_ context.Context, scope domain.ContentHash) (domain.AgentInputCalibration, error) {
	if s.current.Scope == "" {
		return domain.AgentInputCalibration{Scope: scope}, nil
	}
	return s.current, nil
}

func (s *nativeGenerationCalibrationFixture) MergeAgentInputCalibration(_ context.Context, incoming domain.AgentInputCalibration) (domain.AgentInputCalibration, error) {
	s.writes++
	if s.fail != nil {
		return domain.AgentInputCalibration{}, s.fail
	}
	if s.current.Scope == "" {
		s.current.Scope = incoming.Scope
	}
	if !s.discard {
		var err error
		s.current, err = s.current.Merge(incoming)
		if err != nil {
			return domain.AgentInputCalibration{}, err
		}
	}
	if s.response != nil {
		return s.response(s.current), nil
	}
	return s.current, nil
}

type nativeGenerationBridgeFixture struct {
	bridge                    nativeCodexGenerationBridge
	host                      string
	capability                domain.AgentHostCapability
	store                     *nativeGenerationCalibrationFixture
	preparations, validations int
	question                  domain.AgentInitialPrompt
	returned                  domain.AgentContextPackage
	prepareErr, validateErr   error
	mutate                    func(*domain.AgentContextPackage)
	mutateValidation          func(*domain.AgentContextPackage)
}

// Extracted composition fixture: no process, socket, model, auth or public route.
// Byte counts are an explicit synthetic exact counter for these fixtures only.
func newNativeGenerationBridgeFixture(t *testing.T, launchArgs ...string) *nativeGenerationBridgeFixture {
	t.Helper()
	bound, err := bindNativeCodexLaunch(nativeLaunchRequest(launchArgs...))
	if err != nil {
		t.Fatal(err)
	}
	f := &nativeGenerationBridgeFixture{host: nativeGenerationTestHost, store: &nativeGenerationCalibrationFixture{}}
	f.capability = domain.AgentHostCapability{
		Provider: domain.ProviderCodex, Model: "fixture-model", HostVersion: f.host,
		Tokenizer: "fixture-exact-bytes", ContextWindow: 200000,
		Verified: true, Evidence: "synthetic authority for unit tests only",
		RuntimeScope:          domain.HashContent([]byte("synthetic provider/account/settings scope")),
		InputAccountingPolicy: domain.MeasuredInputReserveV1,
	}
	scope, err := f.capability.CalibrationScope()
	if err != nil {
		t.Fatal(err)
	}
	f.capability.Calibration.Scope = scope
	f.bridge = nativeCodexGenerationBridge{
		bound: bound, expectedHost: f.host, host: func() string { return f.host }, store: f.store,
		expected: nativecodex.Thread{
			ID: nativeHandoffTestID, Model: f.capability.Model, ModelProvider: "fixture-provider",
			Cwd: bound.process.Cwd, SettingsHash: string(domain.HashContent([]byte("synthetic native settings"))),
		},
	}
	f.bridge.prepare = func(ctx context.Context, thread nativecodex.Thread, question domain.AgentInitialPrompt) (domain.AgentContextPackage, error) {
		f.preparations++
		f.question = question
		if f.prepareErr != nil {
			return domain.AgentContextPackage{}, f.prepareErr
		}
		if thread != f.bridge.expected || !question.Present() {
			t.Fatal("preparation did not receive exact thread and present question")
		}
		p := nativeGenerationPackageFixture(t, f.capability, question)
		if f.mutate != nil {
			f.mutate(&p)
			p.ID, err = p.Digest()
			if err != nil {
				t.Fatal(err)
			}
		}
		f.returned = p
		return p, nil
	}
	f.bridge.validate = func(_ context.Context, p domain.AgentContextPackage) error {
		f.validations++
		if f.mutateValidation != nil {
			f.mutateValidation(&p)
		}
		return f.validateErr
	}
	return f
}

func nativeGenerationPackageFixture(t *testing.T, capability domain.AgentHostCapability, question domain.AgentInitialPrompt) domain.AgentContextPackage {
	t.Helper()
	capability.InitialPromptTokens = len(question.Text())
	questionUsage := domain.AgentTokenUsage{Exact: true, Tokens: len(question.Text()), Tokenizer: capability.Tokenizer}
	budget, err := capability.ResolveBudget(capability.Provider, capability.Model, domain.MaxAgentContextTokens, questionUsage)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := domain.NewAgentPromptReservation(question, capability.Provider, capability.Model, questionUsage)
	if err != nil {
		t.Fatal(err)
	}
	p := domain.AgentContextPackage{
		Version: domain.AgentContextVersion, Provider: domain.ProviderCodex, Delivery: "prepared", Capability: "verified_for_preparation",
		Policy: domain.InputPolicy{Version: domain.AgentContextVersion, Mode: "history", BudgetTokens: domain.MaxAgentContextTokens, Source: "explicit"},
		Budget: &budget,
		Content: domain.AgentContextContent{
			Notice: `PRIVATE_HISTORY with "quoted data" and no native tool calls`,
			Selection: domain.AgentContextSelection{
				RepositoryID: "fixture-repository", Branch: "main", SourcePolicy: domain.AgentSourceLatestMain,
				SnapshotID: domain.HashContent([]byte("latest main")), CodeCommit: strings.Repeat("a", 40),
				ContextStateHash: domain.HashContent([]byte("context")), MemoryStateHash: domain.HashContent([]byte("memory")),
				WorktreeStateHash: domain.HashContent([]byte("working state")),
				WorkingPosition:   &domain.AgentWorkingPosition{Branch: "work", CodeCommit: strings.Repeat("b", 40)},
			},
		},
	}
	text, err := p.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	p.Usage = domain.AgentTokenUsage{Exact: true, Tokens: len(text), Tokenizer: capability.Tokenizer}
	p.BindInitialPrompt(reservation)
	p.ID, err = p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateIdentity(); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *nativeGenerationBridgeFixture) prepared(t *testing.T, question string) nativecodex.PreparedGeneration {
	t.Helper()
	p, err := f.bridge.prepareGeneration(context.Background(), f.bridge.expected, question)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *nativeGenerationBridgeFixture) observation(outcome string) nativecodex.GenerationObservation {
	return nativecodex.GenerationObservation{ThreadID: f.bridge.expected.ID, TurnID: nativeGenerationTestTurn, Outcome: outcome}
}

func TestNativeCodexGenerationCompositionCountsActualQuestionAndPackageText(t *testing.T) {
	for _, question := range []string{"", "PRIVATE_INITIAL\n\uD55C\uAE00\n", " --yolo literal task "} {
		for _, supplied := range []bool{false, true} {
			t.Run(fmt.Sprint(len(question), "/supplied=", supplied), func(t *testing.T) {
				var args []string
				if supplied {
					args = []string{"--", question}
				}
				f := newNativeGenerationBridgeFixture(t, args...)
				prepared := f.prepared(t, question)
				if !f.question.Present() || f.question.Text() != question || f.returned.Budget.InitialPromptTokens != len(question) {
					t.Fatal("actual question presence/bytes/count changed")
				}
				want, _ := f.returned.Prompt()
				if len(prepared.History) != 1 || prepared.History[0].Role != "user" || prepared.History[0].Text != want || len(want) != f.returned.Usage.Tokens {
					t.Fatal("injection text differs from exactly counted package text")
				}
				// Explicitly emulate the adapter's before/after injection checks.
				for range 2 {
					if err := prepared.Validate(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				o := f.observation("completed")
				o.ExecutionKnown, o.ExecutionStarted = true, true
				o.UsageKnown, o.UsageBeforeCompaction = true, true
				o.TotalInputTokens = f.returned.Usage.Tokens + len(question) + 8000
				o.ModelContextWindow = f.returned.Budget.ContextWindow
				if err := prepared.Observe(context.Background(), o); err != nil {
					t.Fatal(err)
				}
				if f.store.writes != 1 || f.store.current.OverheadTokens != 8000 || f.store.current.Scope != f.capability.Calibration.Scope || f.preparations != 1 || f.validations != 3 {
					t.Fatal("composition changed scope/count or skipped boundary validation")
				}
			})
		}
	}
}

func TestNativeCodexGenerationCompositionEnforcesBoundQuestion(t *testing.T) {
	for _, args := range [][]string{{"PRIVATE_BOUND"}, {""}, {"PRIVATE\r\nBOUND\r"}} {
		f := newNativeGenerationBridgeFixture(t, args...)
		question := f.bridge.bound.prompt.Text()
		if _, err := f.bridge.prepareGeneration(context.Background(), f.bridge.expected, question+" "); !errors.Is(err, domain.ErrContextBudgetExceeded) || f.preparations != 0 {
			t.Fatal("changed question reached preparation", err)
		}
		f.prepared(t, question)
	}
}

func TestNativeCodexGenerationCompositionRejectsUnboundRuntime(t *testing.T) {
	for name, mutate := range map[string]func(*nativeGenerationBridgeFixture, *nativecodex.Thread){
		"thread": func(_ *nativeGenerationBridgeFixture, thread *nativecodex.Thread) {
			thread.ID = nativeGenerationTestTurn
		},
		"model":          func(_ *nativeGenerationBridgeFixture, thread *nativecodex.Thread) { thread.Model = "other" },
		"provider route": func(_ *nativeGenerationBridgeFixture, thread *nativecodex.Thread) { thread.ModelProvider = "other" },
		"cwd":            func(_ *nativeGenerationBridgeFixture, thread *nativecodex.Thread) { thread.Cwd += "/other" },
		"settings": func(_ *nativeGenerationBridgeFixture, thread *nativecodex.Thread) {
			thread.SettingsHash = string(domain.HashContent([]byte("other")))
		},
		"host":           func(f *nativeGenerationBridgeFixture, _ *nativecodex.Thread) { f.host += " changed" },
		"launch":         func(f *nativeGenerationBridgeFixture, _ *nativecodex.Thread) { f.bridge.bound = nativeCodexLaunch{} },
		"selected model": func(f *nativeGenerationBridgeFixture, _ *nativecodex.Thread) { f.bridge.bound.thread.Model = "other" },
		"selected provider": func(f *nativeGenerationBridgeFixture, _ *nativecodex.Thread) {
			f.bridge.bound.thread.ModelProvider = "other"
		},
		"preparer":  func(f *nativeGenerationBridgeFixture, _ *nativecodex.Thread) { f.bridge.prepare = nil },
		"validator": func(f *nativeGenerationBridgeFixture, _ *nativecodex.Thread) { f.bridge.validate = nil },
		"store":     func(f *nativeGenerationBridgeFixture, _ *nativecodex.Thread) { f.bridge.store = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			thread := f.bridge.expected
			mutate(f, &thread)
			p, err := f.bridge.prepareGeneration(context.Background(), thread, "PRIVATE_INITIAL")
			if err == nil || len(p.History) != 0 || f.preparations != 0 || f.store.writes != 0 {
				t.Fatal("invalid runtime reached preparation")
			}
		})
	}
	f := newNativeGenerationBridgeFixture(t)
	if callback, err := newNativeCodexGenerationPrepare(f.bridge.bound, nil, f.bridge.expected, f.host, f.bridge.prepare, f.bridge.validate, f.store); err == nil || callback != nil {
		t.Fatal("missing session authorized a callback")
	}
	if callback, err := newNativeCodexGenerationPrepare(f.bridge.bound, &nativecodex.Session{}, f.bridge.expected, f.host, f.bridge.prepare, f.bridge.validate, f.store); err == nil || callback != nil {
		t.Fatal("unacknowledged session authorized a callback")
	}
}

func TestNativeCodexGenerationCompositionRejectsInvalidPackages(t *testing.T) {
	for name, mutate := range map[string]func(*domain.AgentContextPackage){
		"artifact":               func(p *domain.AgentContextPackage) { p.ArtifactOnly = true },
		"memory":                 func(p *domain.AgentContextPackage) { p.Policy.Mode = "memory" },
		"capability":             func(p *domain.AgentContextPackage) { p.Capability = "unverified" },
		"no budget":              func(p *domain.AgentContextPackage) { p.Budget = nil },
		"package provider":       func(p *domain.AgentContextPackage) { p.Provider = domain.ProviderClaude },
		"budget provider":        func(p *domain.AgentContextPackage) { p.Budget.Provider = domain.ProviderClaude },
		"model":                  func(p *domain.AgentContextPackage) { p.Budget.Model = "other" },
		"host":                   func(p *domain.AgentContextPackage) { p.Budget.HostVersion = "other" },
		"source":                 func(p *domain.AgentContextPackage) { p.Content.Selection.SourcePolicy = "" },
		"branch":                 func(p *domain.AgentContextPackage) { p.Content.Selection.Branch = "other" },
		"approximate usage":      func(p *domain.AgentContextPackage) { p.Usage.Exact = false },
		"missing reservation":    func(p *domain.AgentContextPackage) { p.BindInitialPrompt(domain.AgentPromptReservation{}) },
		"changed original count": func(p *domain.AgentContextPackage) { p.Budget.InitialPromptTokens++; p.Budget.EffectiveTokens-- },
		"policy":                 func(p *domain.AgentContextPackage) { p.Budget.InputAccountingPolicy = "future" },
		"scope":                  func(p *domain.AgentContextPackage) { p.Budget.RuntimeScope = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			f.mutate = mutate // rehash so these test validation beyond the digest
			p, err := f.bridge.prepareGeneration(context.Background(), f.bridge.expected, "PRIVATE_INITIAL")
			if err == nil || len(p.History) != 0 || f.validations != 0 || f.store.writes != 0 {
				t.Fatal("invalid package reached external validation or delivery")
			}
		})
	}
}

func TestNativeCodexGenerationCompositionRevalidatesAndFreezesPackage(t *testing.T) {
	t.Run("preparer aliases", func(t *testing.T) {
		f := newNativeGenerationBridgeFixture(t)
		prepared := f.prepared(t, "PRIVATE_INITIAL")
		original := prepared.History[0].Text
		f.returned.Budget.InitialPromptTokens++
		f.returned.Content.Selection.WorkingPosition.CodeCommit = strings.Repeat("c", 40)
		if err := prepared.Validate(context.Background()); err != nil || prepared.History[0].Text != original {
			t.Fatal("preparer mutated the privately captured package", err)
		}
	})
	for _, changed := range []string{"runtime", "source", "validator mutation", "history"} {
		t.Run(changed, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			prepared := f.prepared(t, "PRIVATE_INITIAL")
			if err := prepared.Validate(context.Background()); err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "runtime":
				f.host += " changed"
			case "source":
				f.validateErr = domain.ErrSelectionChanged
			case "validator mutation":
				f.mutateValidation = func(p *domain.AgentContextPackage) { p.Budget.InitialPromptTokens++ }
			case "history":
				prepared.History[0].Text = "PRIVATE_CHANGED"
			}
			if err := prepared.Validate(context.Background()); err == nil {
				t.Fatal("post-injection check accepted drift")
			}
			if changed == "runtime" || changed == "history" {
				if err := prepared.Observe(context.Background(), f.observation("unknown")); err == nil || f.store.writes != 0 {
					t.Fatal("observation ignored its binding", err)
				}
			}
		})
	}
}

func TestNativeCodexGenerationCompositionRecordsSubmittedSnapshotAfterToolWork(t *testing.T) {
	f := newNativeGenerationBridgeFixture(t)
	prepared := f.prepared(t, "PRIVATE_INITIAL")
	for range 2 {
		if err := prepared.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// A completed model turn may legitimately change the working position.
	// Its first-request feedback is still bound to the original delivered input.
	f.validateErr = domain.ErrSelectionChanged
	o := f.observation("completed")
	o.ExecutionKnown, o.ExecutionStarted = true, true
	o.UsageKnown, o.UsageBeforeCompaction = true, true
	o.TotalInputTokens = f.returned.Usage.Tokens + f.returned.Budget.InitialPromptTokens + 7000
	if err := prepared.Observe(context.Background(), o); err != nil || f.store.writes != 1 || f.validations != 3 || f.store.current.OverheadTokens != 7000 {
		t.Fatal("feedback incorrectly reauthorized a different delivery boundary", err)
	}
}

func TestNativeCodexGenerationCompositionRejectsIdentityAndReceiptOnlyAuthorization(t *testing.T) {
	for _, receiptOnly := range []bool{false, true} {
		f := newNativeGenerationBridgeFixture(t)
		prepare := f.bridge.prepare
		f.bridge.prepare = func(ctx context.Context, thread nativecodex.Thread, prompt domain.AgentInitialPrompt) (domain.AgentContextPackage, error) {
			p, err := prepare(ctx, thread, prompt)
			if err != nil {
				return p, err
			}
			if receiptOnly {
				raw, err := json.Marshal(p)
				if err != nil {
					return p, err
				}
				err = json.Unmarshal(raw, &p) // removes the in-memory prompt reservation
				return p, err
			}
			p.ID = domain.HashContent([]byte("not this package"))
			return p, nil
		}
		if p, err := f.bridge.prepareGeneration(context.Background(), f.bridge.expected, "PRIVATE_INITIAL"); err == nil || len(p.History) != 0 || f.validations != 0 {
			t.Fatal("invalid identity/receipt authorized private generation", err)
		}
	}
}

func TestNativeCodexGenerationCompositionObservationMapping(t *testing.T) {
	for _, outcome := range []string{"unknown", "completed", "initial_compaction", "input_rejected"} {
		t.Run(outcome, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			prepared := f.prepared(t, "PRIVATE_INITIAL")
			o := f.observation(outcome)
			// Even if a future schema attests nonexecution and the domain returns
			// eligibility, this bridge does not reprepare, inject or retry.
			if outcome == "input_rejected" {
				o.ExecutionKnown = true
			}
			if err := prepared.Observe(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if f.store.writes != 1 || f.preparations != 1 || f.store.current.OverheadTokens != 0 || f.store.current.Scope != f.capability.Calibration.Scope {
				t.Fatal("unknown usage fabricated overhead or triggered preparation")
			}
			if (outcome == "initial_compaction" || outcome == "input_rejected") != (f.store.current.InputCeilingTokens > 0) {
				t.Fatal("outcome was not mapped through domain feedback")
			}
		})
	}
	for name, mutate := range map[string]func(*nativecodex.GenerationObservation){
		"foreign thread":         func(o *nativecodex.GenerationObservation) { o.ThreadID = nativeGenerationTestTurn },
		"missing turn":           func(o *nativecodex.GenerationObservation) { o.TurnID = "" },
		"window disagreement":    func(o *nativecodex.GenerationObservation) { o.ModelContextWindow = 1000000 },
		"negative window":        func(o *nativecodex.GenerationObservation) { o.ModelContextWindow = -1 },
		"unknown with count":     func(o *nativecodex.GenerationObservation) { o.TotalInputTokens = 1000 },
		"usage after compaction": func(o *nativecodex.GenerationObservation) { o.UsageKnown = true; o.TotalInputTokens = 10000 },
		"usage below submission": func(o *nativecodex.GenerationObservation) {
			o.UsageKnown = true
			o.UsageBeforeCompaction = true
			o.TotalInputTokens = 1
		},
		"execution contradiction": func(o *nativecodex.GenerationObservation) { o.ExecutionStarted = true },
		"outcome":                 func(o *nativecodex.GenerationObservation) { o.Outcome = "not_a_native_outcome" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			prepared := f.prepared(t, "PRIVATE_INITIAL")
			o := f.observation("unknown")
			mutate(&o)
			if err := prepared.Observe(context.Background(), o); err == nil || f.store.writes != 0 {
				t.Fatal("invalid observation wrote feedback", err)
			} else if nativeGenerationIsPersistenceFailure(err) {
				t.Fatal("invalid observation was mislabeled as a cache failure")
			}
		})
	}
	for _, rerouted := range []bool{false, true} {
		f := newNativeGenerationBridgeFixture(t)
		prepared := f.prepared(t, "PRIVATE_INITIAL")
		o := f.observation("unknown")
		o.ModelRerouted, o.Ineligible = rerouted, !rerouted
		if err := prepared.Observe(context.Background(), o); err != nil || f.store.writes != 0 {
			t.Fatal("ineligible/rerouted observation persisted", err)
		}
	}
}

func TestNativeCodexGenerationCompositionPropagatesPrivateFailures(t *testing.T) {
	privateErr := errors.New("PRIVATE_PROMPT_AUTH_OR_PATH")
	for _, stage := range []string{"prepare", "validate", "store"} {
		t.Run(stage, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			if stage == "prepare" {
				f.prepareErr = privateErr
			} else if stage == "validate" {
				f.validateErr = privateErr
			}
			prepared, err := f.bridge.prepareGeneration(context.Background(), f.bridge.expected, "PRIVATE_INITIAL")
			if stage == "store" {
				if err != nil {
					t.Fatal(err)
				}
				f.store.fail = privateErr
				err = prepared.Observe(context.Background(), f.observation("unknown"))
			}
			if !errors.Is(err, privateErr) {
				t.Fatal("lost dependency/write error", err)
			}
			if nativeGenerationIsPersistenceFailure(err) != (stage == "store") {
				t.Fatal("callback/store failure classification was incorrect")
			}
			for _, rendered := range []string{fmt.Sprint(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%#v", f.bridge), fmt.Sprintf("%#v", prepared)} {
				if strings.Contains(rendered, "PRIVATE_") {
					t.Fatal("routine formatting leaked private input")
				}
			}
			if raw, err := json.Marshal(prepared); err != nil || strings.Contains(string(raw), "PRIVATE_") {
				t.Fatal("prepared generation receipt exposed private content")
			}
		})
	}
	f := newNativeGenerationBridgeFixture(t)
	prepared := f.prepared(t, "PRIVATE_INITIAL")
	f.store.discard = true
	o := f.observation("completed")
	o.UsageKnown, o.UsageBeforeCompaction = true, true
	o.TotalInputTokens = f.returned.Usage.Tokens + f.returned.Budget.InitialPromptTokens + 5000
	if err := prepared.Observe(context.Background(), o); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatal("store failed to retain calibration without reporting failure", err)
	}
}

func TestNativeCodexGenerationCompositionCancellation(t *testing.T) {
	f := newNativeGenerationBridgeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.bridge.prepareGeneration(ctx, f.bridge.expected, "PRIVATE_INITIAL"); !errors.Is(err, context.Canceled) || f.preparations != 0 {
		t.Fatal("canceled preparation reached dependencies", err)
	}
	prepared := f.prepared(t, "PRIVATE_INITIAL")
	if err := prepared.Validate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("validation ignored cancellation", err)
	}
	if err := prepared.Observe(ctx, f.observation("unknown")); !errors.Is(err, context.Canceled) || f.store.writes != 0 {
		t.Fatal("canceled observation wrote feedback", err)
	}
}

func TestNativeCodexGenerationCompositionTelemetryNeverAuthorizesCapacity(t *testing.T) {
	for _, window := range []int{0, 100000, 200000, 1000000} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			prepared := f.prepared(t, "PRIVATE_INITIAL")
			budget, capability := *f.returned.Budget, f.capability
			o := f.observation("completed")
			o.ExecutionKnown, o.ExecutionStarted = true, true
			o.UsageKnown, o.UsageBeforeCompaction = true, true
			o.TotalInputTokens = f.returned.Usage.Tokens + budget.InitialPromptTokens + 9000
			o.ModelContextWindow = window
			err := prepared.Observe(context.Background(), o)
			if window != 0 && window != budget.ContextWindow {
				if !errors.Is(err, domain.ErrProviderCapabilityUnknown) || f.store.writes != 0 {
					t.Fatal("disagreeing telemetry persisted or established capacity", err)
				}
			} else if err != nil || f.store.writes != 1 || f.store.current.OverheadTokens != 9000 {
				t.Fatal("valid usage did not record only measured overhead", err)
			}
			if *f.returned.Budget != budget || f.capability != capability || f.preparations != 1 {
				t.Fatal("telemetry changed the prepared capacity or started another preparation")
			}
			// Successful completion, a matching native window and even stored
			// calibration cannot turn the public unverified host into authority.
			reader := app.MeasuredAgentCapabilities{Runtime: unverifiedAgentHost{}, Observations: f.store}
			c, err := reader.AgentCapability(context.Background(), domain.ProviderCodex, budget.Model)
			if !errors.Is(err, domain.ErrProviderCapabilityUnknown) || c.Verified {
				t.Fatal("calibration bypassed the public capability rejection", err)
			}
		})
	}
}

func TestNativeCodexGenerationCompositionRejectsBrokenCalibrationAcknowledgements(t *testing.T) {
	for name, response := range map[string]func(domain.AgentInputCalibration) domain.AgentInputCalibration{
		"scope mismatch": func(c domain.AgentInputCalibration) domain.AgentInputCalibration {
			c.Scope = domain.HashContent([]byte("different runtime scope"))
			return c
		},
		"missing scope":     func(c domain.AgentInputCalibration) domain.AgentInputCalibration { c.Scope = ""; return c },
		"negative overhead": func(c domain.AgentInputCalibration) domain.AgentInputCalibration { c.OverheadTokens = -1; return c },
		"negative ceiling":  func(c domain.AgentInputCalibration) domain.AgentInputCalibration { c.InputCeilingTokens = -1; return c },
		"lost overhead":     func(c domain.AgentInputCalibration) domain.AgentInputCalibration { c.OverheadTokens = 0; return c },
		"lost ceiling":      func(c domain.AgentInputCalibration) domain.AgentInputCalibration { c.InputCeilingTokens = 0; return c },
		"raised ceiling":    func(c domain.AgentInputCalibration) domain.AgentInputCalibration { c.InputCeilingTokens++; return c },
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			prepared := f.prepared(t, "PRIVATE_INITIAL")
			budget := *f.returned.Budget
			f.store.response = response
			o := f.observation("initial_compaction")
			o.UsageKnown, o.UsageBeforeCompaction = true, true
			o.TotalInputTokens = f.returned.Usage.Tokens + budget.InitialPromptTokens + 8000
			if err := prepared.Observe(context.Background(), o); !errors.Is(err, domain.ErrHashMismatch) || !nativeGenerationIsPersistenceFailure(err) {
				t.Fatal("broken storage acknowledgement was accepted", err)
			}
			if f.store.writes != 1 || f.preparations != 1 || *f.returned.Budget != budget {
				t.Fatal("calibration failure retried or changed capacity")
			}
		})
	}
}

func TestNativeCodexGenerationCompositionUnknownAndExcludedObservationsCannotRelaxFeedback(t *testing.T) {
	for _, outcome := range []string{"unknown", "completed", "rerouted", "ineligible"} {
		t.Run(outcome, func(t *testing.T) {
			f := newNativeGenerationBridgeFixture(t)
			prepared := f.prepared(t, "PRIVATE_INITIAL")
			previous := domain.AgentInputCalibration{Scope: f.capability.Calibration.Scope, OverheadTokens: 30000, InputCeilingTokens: 100000}
			f.store.current = previous
			o := f.observation("unknown")
			switch outcome {
			case "completed":
				o.Outcome = "completed"
			case "rerouted":
				o.ModelRerouted = true
			case "ineligible":
				o.Ineligible = true
			}
			if err := prepared.Observe(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if f.store.current != previous || f.preparations != 1 {
				t.Fatal("absent evidence relaxed feedback or triggered preparation")
			}
			if (outcome == "rerouted" || outcome == "ineligible") && f.store.writes != 0 {
				t.Fatal("excluded observation reached storage")
			}
		})
	}
}

func TestNativeCodexGenerationCompositionReportsCancellationDuringPersistence(t *testing.T) {
	f := newNativeGenerationBridgeFixture(t)
	prepared := f.prepared(t, "PRIVATE_INITIAL")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.store.response = func(c domain.AgentInputCalibration) domain.AgentInputCalibration {
		cancel()
		return c
	}
	if err := prepared.Observe(ctx, f.observation("unknown")); !errors.Is(err, context.Canceled) || !nativeGenerationIsPersistenceFailure(err) {
		t.Fatal("cancellation after a possible write became success", err)
	}
	if f.store.writes != 1 || f.preparations != 1 {
		t.Fatal("uncertain persistence was retried")
	}
}

func nativeGenerationIsPersistenceFailure(err error) bool {
	var classified interface{ CalibrationPersistenceFailure() bool }
	return errors.As(err, &classified) && classified.CalibrationPersistenceFailure()
}
