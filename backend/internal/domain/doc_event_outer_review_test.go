package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDocEventOuterReviewReasoningIgnoresMessageSummaryFlag(test *testing.T) {
	for _, summary := range []string{`"visible"`, `null`} {
		raw := []byte(`{"kind":"reasoning","compact_summary":true,"redacted_summary":` + summary + `}`)
		entry, err := DecodeDocRangeMetadata(raw, DocEventIndex{Hash: HashContent(raw), Length: len(raw)})
		if err != nil || !entry.CompactSummary || entry.Kind != EventReasoning {
			test.Fatal("fixture is not admitted Reasoning metadata", err)
		}
		if _, err := BuildDocEventDisplay(context.Background(), raw); err != nil {
			test.Fatal("existing display rejects fixture", err)
		}
		state, err := NewDocEventDisplayPreparation(entry)
		if err != nil {
			test.Errorf("admitted Reasoning event rejected because it carries a message-only flag: summary=%s err=%v", summary, err)
			continue
		}
		encoded, err := state.Source.Encode()
		if err != nil || state.Source.Summary || bytes.Contains(encoded, []byte(`"summary"`)) {
			test.Error("message-only summary flag changes legacy Reasoning source bytes", err)
		}
	}
}

func outerReviewMessagePlan(test *testing.T, raw []byte) (*eventMessageChildTestFixture, DocEventDisplayPreparation) {
	test.Helper()
	prepared := buildMessageInputFixture(test, raw)
	inputs := prepared.generation
	encoded := eventMessageChildTestJSON(test, inputs)
	fixture := &eventMessageChildTestFixture{raw: raw, inputsHash: HashContent(encoded), inputs: messageFixtureInputs(test, prepared), objects: prepared.objects}
	fixture.store(encoded)
	source := DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: inputs}
	display, err := NewDocMessageDisplayState(source)
	if err != nil {
		test.Fatal(err)
	}
	active, err := display.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	state := DocEventDisplayPreparation{Version: 1, Source: DocEventDisplaySource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: HashContent(raw), EventBytes: len(raw), Kind: EventMessage}, Phase: "message_plan", Inputs: fixture.inputsHash, Active: fixture.store(active)}
	return fixture, state
}

func TestDocEventOuterReviewRawYieldKeepsOnlyCommittedPrefix(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"` + strings.Repeat("a", 40000) + `","name":"` + strings.Repeat("b", 40000) + `"}]}`)
	fixture, prior := outerReviewMessagePlan(test, raw)
	selectedType, selectedName, err := fixture.inputs[0].PlaceholderSources()
	if err != nil {
		test.Fatal(err)
	}
	readBytes, discardedNameBytes := 0, 0
	step, err := StepDocEventDisplay(context.Background(), prior, func(_ context.Context, from, length int) ([]byte, error) {
		readBytes += length
		if from >= selectedName.Token.From {
			discardedNameBytes += length
		}
		return bytes.Clone(raw[from : from+length]), nil
	}, fixture.load)
	if err != nil || readBytes > 64<<10 || discardedNameBytes == 0 || len(step.objects) > 32 {
		test.Fatal("raw-limit yield did not return a bounded prefix", readBytes, discardedNameBytes, len(step.objects), err)
	}
	builder := docEventDisplayBuilder{objects: step.objects}
	for _, object := range step.objects {
		builder.bytes += len(object.Bytes)
	}
	fixture.persist(test, builder)
	state := step.Preparation()
	child := fixture.task(test, fixture.inputs[0], state.Child)
	if child.Phase != "name" || child.Type == "" || child.Name != "" || child.Text != "" {
		test.Fatal("yield lost completed type or committed unfinished name")
	}
	nameState, err := DecodeDocDecodedTextState(*selectedName, child.Active, fixture.objects[child.Active])
	if err != nil || nameState.Next != selectedName.Token.From || nameState.TextBytes != 0 {
		test.Fatal("yield retained a partially decoded suffix", err)
	}
	decodedType, err := DecodeDocDecodedTextGeneration(child.Type, fixture.objects[child.Type])
	if err != nil || decodedType.Source != *selectedType || decodedType.TextBytes != 40000 {
		test.Fatal("yield discarded completed prefix generation", err)
	}
	for _, object := range step.objects {
		var candidate DocDisplayPage
		if jsonErr := json.Unmarshal(object.Bytes, &candidate); jsonErr == nil && candidate.Version == 3 && candidate.Text != "" && strings.Contains(candidate.Text, "bbbb") {
			test.Fatal("uncommitted name page escaped in the yielded prefix")
		}
	}
	for advances := 0; advances < 8 && state.Phase == "message_plan"; advances++ {
		next, err := StepDocEventDisplay(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
			return bytes.Clone(raw[from : from+length]), nil
		}, fixture.load)
		if err != nil {
			test.Fatal("prefix-only restart failed", err)
		}
		for _, object := range next.objects {
			fixture.store(object.Bytes)
		}
		state = next.Preparation()
	}
	if state.Phase != "assembly" {
		test.Fatal("prefix-only restart did not finish message planning")
	}
}

func TestDocEventOuterReviewFatalSuffixDoesNotReturnPrefix(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"` + strings.Repeat("a", 40000) + `","name":"` + strings.Repeat("b", 40000) + `"}]}`)
	fixture, prior := outerReviewMessagePlan(test, raw)
	_, name, err := fixture.inputs[0].PlaceholderSources()
	if err != nil {
		test.Fatal(err)
	}
	for _, cancelDuringLoad := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		failure := errors.New("fatal suffix read")
		step, err := StepDocEventDisplay(ctx, prior, func(_ context.Context, from, length int) ([]byte, error) {
			if from >= name.Token.From {
				if cancelDuringLoad {
					cancel()
					return bytes.Clone(raw[from : from+length]), nil
				}
				return nil, failure
			}
			return bytes.Clone(raw[from : from+length]), nil
		}, fixture.load)
		cancel()
		if cancelDuringLoad {
			failure = context.Canceled
		}
		if !errors.Is(err, failure) || !reflect.DeepEqual(step, DocEventDisplayStep{}) {
			test.Fatal("fatal suffix returned a successful prefix", err)
		}
	}
}

type outerReviewCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (ctx *outerReviewCancelContext) Err() error {
	ctx.checks++
	if ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestDocEventOuterReviewPublicationFinalCancellationReturnsZero(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{}]}`)
	fixture, state := outerReviewMessagePlan(test, raw)
	for advances := 0; advances < 32 && state.Phase != "completed"; advances++ {
		step, err := StepDocEventDisplay(context.Background(), state, func(_ context.Context, from, length int) ([]byte, error) {
			return bytes.Clone(raw[from : from+length]), nil
		}, fixture.load)
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range step.objects {
			fixture.store(object.Bytes)
		}
		state = step.Preparation()
	}
	probe := &outerReviewCancelContext{Context: context.Background()}
	if _, err := ValidateDocEventDisplayPublication(probe, state, fixture.load); err != nil {
		test.Fatal("baseline publication failed", err)
	}
	for _, cancelAt := range []int{probe.checks - 1, probe.checks} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &outerReviewCancelContext{Context: base, cancel: cancel, cancelAt: cancelAt}
		generation, err := ValidateDocEventDisplayPublication(ctx, state, fixture.load)
		cancel()
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(generation, DocDisplayProjectionGeneration{}) {
			test.Fatalf("final cancellation returned nonzero publication: checks=%d error=%v generation_hash=%s", ctx.checks, err, state.Generation)
		}
	}
}

func TestDocEventOuterReviewChildBudgetRollbackClearsDiscardedReferences(test *testing.T) {
	fixture := newEventMessageChildTestFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"discarded-body"}]}`))
	input := fixture.inputs[0]
	childHash := fixture.start(test, input)
	source, err := input.TextSource()
	if err != nil {
		test.Fatal(err)
	}
	decoded := buildDecodedTextSourceFixture(test, fixture.raw, *source)
	page, err := encodeDocDecodedTextPage(*source, decoded.pages[0])
	if err != nil {
		test.Fatal(err)
	}
	for _, budget := range []string{"objects", "bytes"} {
		test.Run(budget, func(test *testing.T) {
			prefixCount, prefixBytes := MaxDocEventDisplayStepObjects-1, MaxDocEventDisplayStepObjects-1
			if budget == "bytes" {
				prefixCount, prefixBytes = MaxDocEventDisplayStepBytes/MaxDocDisplayPageBytes, MaxDocEventDisplayStepBytes-len(page)
			}
			backing := make([]DocRangeObject, MaxDocEventDisplayStepObjects)
			remaining := prefixBytes
			for index := 0; index < prefixCount; index++ {
				length := min(MaxDocDisplayPageBytes, remaining-(prefixCount-index-1))
				if budget == "objects" {
					length = 1
				}
				raw := bytes.Repeat([]byte{byte(index + 1)}, length)
				backing[index] = DocRangeObject{Hash: HashContent(raw), Bytes: raw}
				remaining -= length
			}
			builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load, objects: backing[:prefixCount], bytes: prefixBytes, yieldBudget: true}
			eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, input, childHash, func(_ context.Context, from, length int) ([]byte, error) {
				return bytes.Clone(fixture.raw[from : from+length]), nil
			}, errDocEventDisplayBatchYield)
			clear(builder.objects[prefixCount:])
			for _, discarded := range backing[prefixCount:] {
				if discarded.Hash != "" || discarded.Bytes != nil {
					test.Fatal("child rollback truncated staged output before batch cleanup could clear the discarded body reference")
				}
			}
		})
	}
}
