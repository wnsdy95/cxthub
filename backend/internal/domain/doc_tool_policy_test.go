package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type docToolPolicyFixture struct {
	raw     []byte
	state   DocEventDisplayPreparation
	objects map[ContentHash][]byte
	tools   DocToolPreparationReader
	steps   []DocEventDisplayPreparation
}

func newDocToolPolicyFixture(test *testing.T, raw string) *docToolPolicyFixture {
	test.Helper()
	fixture := &docToolPolicyFixture{raw: []byte(raw), objects: make(map[ContentHash][]byte)}
	discovery, fields := requireDocToolFields(test, fixture.raw, DocFieldDiscoveryStepBytes)
	if _, err := NewDocToolTreeSource(discovery.source, discovery.state, DocRangeEntry{DocEventIndex: DocEventIndex{Hash: HashContent(fixture.raw), Length: len(fixture.raw)}, Kind: fields.Kind}); err == nil {
		value := newDocToolValueFixtureFromOrder(test, newDocToolOrdersFixtureFromKeys(test, newDocToolKeyMapFixtureRaw(test, raw)))
		fixture.objects = value.order.keys.tree.objects
		fixture.tools = value.order.tools()
		fixture.tools.OrderPlan = value.loadOrder
	} else if !errors.Is(err, ErrDocFindDisplayUnavailable) {
		test.Fatal(err)
	}
	var err error
	fixture.state, err = NewDocEventDisplayPreparation(DocRangeEntry{DocEventIndex: DocEventIndex{Hash: HashContent(fixture.raw), Length: len(fixture.raw)}, Kind: fields.Kind})
	if err != nil {
		test.Fatal(err)
	}
	return fixture
}

func (fixture *docToolPolicyFixture) load(ctx context.Context, hash ContentHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, found := fixture.objects[hash]
	if !found {
		return nil, ErrIntegrity
	}
	return bytes.Clone(raw), nil
}

func (fixture *docToolPolicyFixture) advance(test *testing.T) {
	test.Helper()
	before, err := fixture.state.Encode()
	if err != nil {
		test.Fatal(err)
	}
	fixture.steps = append(fixture.steps, fixture.state)
	readBytes := 0
	step, err := StepDocEventDisplay(context.Background(), fixture.state, func(_ context.Context, from, length int) ([]byte, error) {
		if strings.Contains(fixture.state.Phase, "assembly") {
			test.Fatal("final assembly read original bytes")
		}
		if from < 0 || length <= 0 || from > len(fixture.raw)-length {
			return nil, ErrIntegrity
		}
		readBytes += length
		return bytes.Clone(fixture.raw[from : from+length]), nil
	}, fixture.load, fixture.tools)
	if err != nil {
		test.Fatal("tool event step", len(fixture.steps), fixture.state.Phase, err)
	}
	after, _ := fixture.state.Encode()
	if !bytes.Equal(before, after) || step.Prior() != HashContent(before) || readBytes > DocFieldDiscoveryStepBytes || len(step.Objects()) > MaxDocEventDisplayStepObjects {
		test.Fatal("worker changed predecessor or exceeded its per-step budget", readBytes, len(step.Objects()))
	}
	objectBytes := 0
	for _, object := range step.Objects() {
		objectBytes += len(object.Bytes)
		if HashContent(object.Bytes) != object.Hash || len(object.Bytes) > MaxDocDisplayPageBytes {
			test.Fatal("invalid emitted object")
		}
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	if objectBytes > MaxDocEventDisplayStepBytes {
		test.Fatal("staged output exceeded byte budget")
	}
	encoded, err := step.Preparation().Encode()
	if err != nil || len(encoded) > MaxDocEventDisplayPreparationBytes {
		test.Fatal("outer control exceeded its unchanged byte limit", len(encoded), err)
	}
	fixture.state, err = DecodeDocEventDisplayPreparation(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal(err)
	}
}

func (fixture *docToolPolicyFixture) finish(test *testing.T) DocDisplayProjectionGeneration {
	test.Helper()
	for count := 0; fixture.state.Phase != "completed"; count++ {
		if count > len(fixture.raw)*4+100 {
			test.Fatal("tool policy did not converge")
		}
		fixture.advance(test)
	}
	generation, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.load)
	if err != nil {
		test.Fatal("tool event publication", err)
	}
	return generation
}

func TestDocToolPolicySharedEventParity(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/tool-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixtures struct {
		Cases []struct{ Name, Source, Text string }
	}
	if json.Unmarshal(raw, &fixtures) != nil || len(fixtures.Cases) < 25 {
		test.Fatal("invalid shared tool fixtures")
	}
	for _, scenario := range fixtures.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			fixture := newDocToolPolicyFixture(test, scenario.Source)
			generation := fixture.finish(test)
			baseline, err := BuildDocToolDisplay(context.Background(), fixture.raw)
			if err != nil {
				test.Fatal(err)
			}
			var output strings.Builder
			for index := 0; index < generation.Pages.Count; index++ {
				page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, fixture.load)
				if err != nil || page.From != output.Len() {
					test.Fatal("noncontiguous tool projection", err)
				}
				for offset, character := range page.Text {
					width := utf8.RuneLen(character)
					got, err := page.SourceRange(offset, width)
					want, baselineError := baseline.SourceRange(page.From+offset, width)
					if err != nil || baselineError != nil || got != want {
						test.Fatal("tool policy changed original-byte anchor", offset, got, want, err, baselineError)
					}
				}
				output.WriteString(page.Text)
			}
			if output.String() != scenario.Text || output.String() != baseline.Text() {
				test.Fatalf("tool event output=%q want=%q", output.String(), scenario.Text)
			}
		})
	}
}

func TestDocToolPolicyReceiptRejectsControlTransplants(test *testing.T) {
	fixture := newDocToolPolicyFixture(test, `{"kind":"tool_call","tool_name":"Edit","input":{"old_string":"old","new_string":"new","file_path":"/a/b/c/d"}}`)
	fixture.finish(test)
	checked := 0
	for _, previous := range fixture.steps {
		if previous.Phase != "tool_policy" {
			continue
		}
		state, err := decodeDocToolPolicyState(previous.Active, fixture.objects[previous.Active])
		if err != nil || state.Receipt == "" {
			continue
		}
		state.Truth[0] = !state.Truth[0]
		raw, err := state.encode()
		if err != nil {
			test.Fatal(err)
		}
		previous.Active = HashContent(raw)
		fixture.objects[previous.Active] = raw
		step, err := StepDocEventDisplay(context.Background(), previous, func(context.Context, int, int) ([]byte, error) {
			test.Fatal("forged control reached original data")
			return nil, ErrIntegrity
		}, fixture.load, fixture.tools)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocEventDisplayStep{}) {
			test.Fatal("forged policy checkpoint was accepted", err)
		}
		checked++
	}
	if checked < 10 {
		test.Fatal("did not inspect enough saved policy phases", checked)
	}
}

func TestDocToolPolicyMissingNullInputAndNames(test *testing.T) {
	for _, scenario := range []struct{ raw, text string }{
		{`{"kind":"tool_call"}`, "tool"},
		{`{"kind":"tool_call","tool_name":"Named"}`, "Named"},
		{`{"kind":"tool_call","provider_tool_name":null,"tool_name":"Named","input":null}`, "Named"},
		{`{"kind":"tool_call","provider_tool_name":"","tool_name":null,"input":{}}`, "tool"},
		{`{"kind":"tool_call","provider_tool_name":"Bash","tool_name":"Edit","input":null}`, "Bash"},
	} {
		test.Run(scenario.raw, func(test *testing.T) {
			fixture := newDocToolPolicyFixture(test, scenario.raw)
			generation := fixture.finish(test)
			var actual strings.Builder
			for index := 0; index < generation.Pages.Count; index++ {
				page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, fixture.load)
				if err != nil {
					test.Fatal(err)
				}
				actual.WriteString(page.Text)
			}
			if actual.String() != scenario.text {
				test.Fatal("missing/null input or name changed the header", actual.String(), scenario.text)
			}
		})
	}
}
