package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
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
