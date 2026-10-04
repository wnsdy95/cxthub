package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type measuredRuntimeFixture struct{ cap domain.AgentHostCapability }

func (f measuredRuntimeFixture) AgentCapability(context.Context, domain.ProviderKind, string) (domain.AgentHostCapability, error) {
	return f.cap, nil
}

type calibrationFixture struct {
	current         domain.AgentInputCalibration
	calls, writes   int
	fail            error
	changeAfterRead bool
}

func (f *calibrationFixture) ReadAgentInputCalibration(_ context.Context, scope domain.ContentHash) (domain.AgentInputCalibration, error) {
	f.calls++
	if f.fail != nil {
		return domain.AgentInputCalibration{}, f.fail
	}
	if f.current.Scope == "" {
		f.current.Scope = scope
	}
	got := f.current
	if f.changeAfterRead && f.calls == 1 {
		f.current.OverheadTokens = 100000
	}
	return got, nil
}
func (f *calibrationFixture) MergeAgentInputCalibration(_ context.Context, c domain.AgentInputCalibration) (domain.AgentInputCalibration, error) {
	f.writes++
	if f.fail != nil {
		return domain.AgentInputCalibration{}, f.fail
	}
	if f.current.Scope == "" {
		f.current.Scope = c.Scope
	}
	var err error
	f.current, err = f.current.Merge(c)
	return f.current, err
}

func measuredPreparationFixture(t *testing.T) (*AgentContextService, measuredRuntimeFixture, *calibrationFixture) {
	t.Helper()
	s, _, _, _, _ := adaptiveHistoryFixture(t)
	c, err := (agentCapabilityFixture{window: 200000}).AgentCapability(context.Background(), domain.ProviderCodex, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	c.HostInputKnown = false
	c.HostInputTokens = 0
	c.AutoCompactKnown = false
	c.AutoCompactTokens = 0
	c.RuntimeScope = domain.HashContent([]byte("route-account-settings synthetic"))
	runtime := measuredRuntimeFixture{c}
	store := &calibrationFixture{}
	s.capabilities = MeasuredAgentCapabilities{Runtime: runtime, Observations: store}
	return s, runtime, store
}

func TestMeasuredPreparationSelectsCompleteTurnsAndRecordsFeedback(t *testing.T) {
	s, in, _, docs, original := adaptiveHistoryFixture(t)
	_, runtime, store := measuredPreparationFixture(t)
	in.Model = runtime.cap.Model
	s.capabilities = MeasuredAgentCapabilities{Runtime: runtime, Observations: store}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Budget.InputAccountingPolicy != domain.MeasuredInputReserveV1 || !p.Budget.HostInputUnverified || !p.Budget.AutoCompactUnverified || p.Budget.OverheadAllowanceTokens != 16000 || store.writes != 0 {
		t.Fatalf("unlabeled estimate or preparation mutation: %+v", p.Budget)
	}
	if len(p.Content.History) != 1 || p.Content.History[0].Source.StartEvent != 4 || len(p.Content.History[0].Events) != 2 || !reflect.DeepEqual(docs.docs[original.Hash], original) {
		t.Fatal("changed source or split turns")
	}
	scope, _ := runtime.cap.CalibrationScope()
	feedback, retry, err := RecordAgentInputObservation(context.Background(), store, p, domain.AgentInputObservation{Scope: scope, PackageID: p.ID, Outcome: "completed", InitialRequest: true, ExecutionKnown: true, ExecutionStarted: true, UsageKnown: true, UsageBeforeCompaction: true, SubmittedTextExact: true, SubmittedTextTokens: p.Usage.Tokens, TotalInputTokens: p.Usage.Tokens + 10000})
	if err != nil || retry || feedback.OverheadTokens != 10000 || store.writes != 1 {
		t.Fatal(feedback, retry, err)
	}
	next, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if next.Budget.EffectiveTokens >= p.Budget.EffectiveTokens || next.Budget.ObservedOverheadTokens != 10000 {
		t.Fatal("feedback not used by selection")
	}
	// No model execution occurs in either preparation or observation recording.
	unknown := domain.AgentInputObservation{Scope: scope, PackageID: p.ID, Outcome: "unknown", InitialRequest: true}
	if _, retry, err := RecordAgentInputObservation(context.Background(), store, p, unknown); err != nil || retry {
		t.Fatal("unknown completion retried", err)
	}
	before := store.writes
	unknown.PackageID = domain.HashContent([]byte("other package"))
	if _, _, err := RecordAgentInputObservation(context.Background(), store, p, unknown); err == nil || store.writes != before {
		t.Fatal("invalid observation wrote calibration")
	}
}

func TestMeasuredPreparationRevalidatesObservationAndScope(t *testing.T) {
	s, in, _, _, _ := adaptiveHistoryFixture(t)
	_, runtime, store := measuredPreparationFixture(t)
	in.Model = runtime.cap.Model
	// Use 1M so both initial and newly observed overhead leave room for the source.
	runtime.cap.ContextWindow = 1000000
	store.changeAfterRead = true
	s.capabilities = MeasuredAgentCapabilities{Runtime: runtime, Observations: store}
	p, err := s.PrepareAgentContext(context.Background(), in)
	if err != nil || store.calls != 4 || p.Budget.ObservedOverheadTokens != 100000 {
		t.Fatal("published stale calibration", store.calls, err)
	}
	runtime.cap.RuntimeScope = domain.HashContent([]byte("changed account"))
	s.capabilities = MeasuredAgentCapabilities{Runtime: runtime, Observations: store}
	if _, err = s.PrepareAgentContext(context.Background(), in); !errors.Is(err, domain.ErrProviderCapabilityUnknown) {
		t.Fatal("cross-scope calibration accepted", err)
	}
}

func TestMeasuredPreparationUnknownWindowAndCorruptFeedbackStopBeforeReads(t *testing.T) {
	for _, mode := range []string{"window", "scope", "calibration"} {
		t.Run(mode, func(t *testing.T) {
			s, in, h, docs, _ := adaptiveHistoryFixture(t)
			_, runtime, store := measuredPreparationFixture(t)
			in.Model = runtime.cap.Model
			switch mode {
			case "window":
				runtime.cap.Verified = false
			case "scope":
				runtime.cap.RuntimeScope = ""
			case "calibration":
				store.fail = domain.ErrHashMismatch
			}
			s.capabilities = MeasuredAgentCapabilities{Runtime: runtime, Observations: store}
			if _, err := s.PrepareAgentContext(context.Background(), in); err == nil || h.calls != 0 || docs.calls != 0 {
				t.Fatal("invalid budget reached cloud reads", err)
			}
		})
	}
}
