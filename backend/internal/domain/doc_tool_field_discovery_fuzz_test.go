package domain

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzDocToolFieldDiscoverySelection(fuzz *testing.F) {
	fuzz.Add("hello", true, uint8(0))
	fuzz.Add("한글🙂\x00\n", false, uint8(1))
	fuzz.Add("provider_tool_name", true, uint8(7))
	fuzz.Fuzz(func(test *testing.T, text string, call bool, selector uint8) {
		if len(text) > 2048 {
			test.Skip()
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			test.Fatal(err)
		}
		kind := EventToolResult
		if call {
			kind = EventToolCall
		}
		output := []byte(`"shadow"`)
		valueType := DocToolValueString
		switch selector % 8 {
		case 0:
			output = encoded
		case 1:
			output = append(append([]byte(`{"nested":`), encoded...), '}')
			valueType = DocToolValueObject
		case 2:
			output = append(append([]byte{'['}, encoded...), []byte(`,null]`)...)
			valueType = DocToolValueArray
		case 3:
			output, valueType = []byte("0"), DocToolValueNumber
		case 4:
			output, valueType = []byte("null"), DocToolValueNull
		case 5:
			output, valueType = []byte("true"), DocToolValueTrue
		case 6:
			output, valueType = []byte("false"), DocToolValueFalse
		case 7:
			output, valueType = []byte("1e1000"), DocToolValueNumber
		}
		input := append(append([]byte(`{"command":`), encoded...), '}')
		raw := []byte(`{"opaque":{"kind":"tool_result","output":"HIDDEN"},"output":"shadow","input":{"stale":true},"provider_tool_name":"old","tool_name":"fallback","kind":"unknown","k\u0069nd":"` + string(kind) + `","\u006futput":`)
		raw = append(raw, output...)
		raw = append(raw, []byte(`,"input":`)...)
		raw = append(raw, input...)
		raw = append(raw, []byte(`,"provider_tool_\u006eame":`)...)
		raw = append(raw, encoded...)
		raw = append(raw, '}')
		fixture, err := discoverDocToolFieldFixture(test, raw, 256)
		if err != nil {
			test.Fatal(err)
		}
		fields, err := fixture.state.Fields(fixture.source)
		if err != nil || fields.Kind != kind || fields.Output == nil || fields.OutputType != valueType || !bytes.Equal(raw[fields.Output.From:fields.Output.To], output) {
			test.Fatal("root output selection/type changed", err)
		}
		if call {
			if fields.Input == nil || fields.InputType != DocToolValueObject || !bytes.Equal(raw[fields.Input.From:fields.Input.To], input) || fields.ProviderToolName == nil || !bytes.Equal(raw[fields.ProviderToolName.From:fields.ProviderToolName.To], encoded) {
				test.Fatal("call public fields selected nested or stale values")
			}
		} else if fields.Input != nil || fields.ProviderToolName != nil || fields.ToolName != nil {
			test.Fatal("result exposed irrelevant call fields")
		}
	})
}
