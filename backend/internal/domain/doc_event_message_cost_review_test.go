package domain

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDocEventMessageChildCheckpointCanonicalFieldOrder(test *testing.T) {
	model := reflect.TypeOf(docEventMessageChildTask{})
	previous := ""
	for index := 0; index < model.NumField(); index++ {
		field := model.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name <= previous || field.Type.Kind() != reflect.String && field.Type.Kind() != reflect.Int {
			test.Fatal("direct child checkpoint encoding lost its scalar canonical field order", field.Name, name, previous, field.Type)
		}
		previous = name
	}
}

func TestDocEventMessageChildCheckpointAllocationReduction(test *testing.T) {
	fixture := newEventMessageChildTestFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"content"}]}`))
	input := fixture.inputs[0]
	task := docEventMessageChildTask{Version: 1, Inputs: fixture.inputsHash, Block: input.Index(), Phase: "text", Active: HashContent([]byte("active"))}
	want, err := canonicalJSON(task)
	if err != nil {
		test.Fatal(err)
	}
	var output []byte
	legacy := testing.AllocsPerRun(30, func() {
		if err := task.validate(fixture.inputsHash, input); err != nil {
			test.Fatal(err)
		}
		output, err = canonicalJSON(task)
		if err != nil {
			test.Fatal(err)
		}
	})
	if !bytes.Equal(output, want) {
		test.Fatal("legacy checkpoint oracle changed")
	}
	current := testing.AllocsPerRun(30, func() {
		output, err = task.encode(fixture.inputsHash, input)
		if err != nil {
			test.Fatal(err)
		}
	})
	if !bytes.Equal(output, want) || current >= legacy {
		test.Fatal("direct checkpoint encoding did not preserve bytes with fewer allocations", current, legacy)
	}
	test.Logf("checkpoint_allocations legacy=%.0f current=%.0f", legacy, current)
}

func TestDocEventMessageChildCheckpointCanonicalCostReview(test *testing.T) {
	fixture := newEventMessageChildTestFixture(test, []byte("{\"kind\":\"message\",\"blocks\":[{\"type\":\"text\",\"text\":\"**\ud55c\uae00🙂**\"},{\"type\":\"image\",\"name\":\"asset\"},{\"type\":\"image\"},{\"name\":\"asset\"},{}]}"))
	active := HashContent([]byte("active checkpoint"))
	text := HashContent([]byte("decoded text"))
	blockType := HashContent([]byte("decoded type"))
	name := HashContent([]byte("decoded name"))
	for _, scenario := range []struct {
		name  string
		block int
		phase string
		text  ContentHash
		kind  ContentHash
		label ContentHash
	}{
		{"text", 0, "text", "", "", ""},
		{"markdown", 0, "markdown", text, "", ""},
		{"type-before-name", 1, "type", "", "", ""},
		{"name-after-type", 1, "name", "", blockType, ""},
		{"placeholder-type-name", 1, "placeholder", "", blockType, name},
		{"type-only", 2, "type", "", "", ""},
		{"placeholder-type-only", 2, "placeholder", "", blockType, ""},
		{"name-only", 3, "name", "", "", ""},
		{"placeholder-name-only", 3, "placeholder", "", "", name},
		{"placeholder-empty", 4, "placeholder", "", "", ""},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			input := fixture.inputs[scenario.block]
			task := docEventMessageChildTask{Version: 1, Inputs: fixture.inputsHash, Block: input.Index(), Phase: scenario.phase, Active: active, Text: scenario.text, Type: scenario.kind, Name: scenario.label}
			want, err := canonicalJSON(task)
			if err != nil {
				test.Fatal(err)
			}
			got, err := task.encode(fixture.inputsHash, input)
			if err != nil || !bytes.Equal(got, want) || len(got) > maxDocEventMessageChildBytes {
				test.Fatal("child checkpoint changed its canonical bytes or size", err)
			}
			hash := HashContent(want)
			decoded, err := decodeDocEventMessageChildTask(fixture.inputsHash, input, hash, want)
			if err != nil || !reflect.DeepEqual(decoded, task) || HashContent(got) != hash {
				test.Fatal("existing child checkpoint changed identity or failed restart", err)
			}
			unknown := append(bytes.Clone(want[:len(want)-1]), []byte(`,"unknown":null}`)...)
			for _, malformed := range [][]byte{append(bytes.Clone(want), ' '), unknown, append(bytes.Clone(want[:len(want)-1]), []byte(`,"version":1}`)...)} {
				decoded, err := decodeDocEventMessageChildTask(fixture.inputsHash, input, HashContent(malformed), malformed)
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, docEventMessageChildTask{}) {
					test.Fatal("rehashed noncanonical child checkpoint accepted", err)
				}
			}
			for _, foreign := range []struct {
				inputs ContentHash
				input  DocMessageInput
			}{
				{HashContent([]byte("foreign inputs")), input},
				{fixture.inputsHash, fixture.inputs[(scenario.block+1)%len(fixture.inputs)]},
			} {
				decoded, err := decodeDocEventMessageChildTask(foreign.inputs, foreign.input, hash, want)
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, docEventMessageChildTask{}) {
					test.Fatal("child checkpoint crossed its input generation or block", err)
				}
			}
		})
	}
}
