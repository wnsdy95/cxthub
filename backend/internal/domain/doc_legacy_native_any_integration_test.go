package domain

import (
	"fmt"
	"strings"
	"testing"
)

const legacyNativeAnyIntegrationDocumentPrefix = `{"ENVE\u004cOPE":{"CIR_VERſION":"2"},"event\u017f":[`

const legacyNativeAnyIntegrationNumbers = `[-0,-0.0,0,-1e-999,1e-999,9007199254740993,9007199254740995,1.234567890123456789,0.1,1e21,1e-7,5e-324,1.7976931348623157e308]`

func TestLegacyNativeAnyIntegrationGiantMixedValues(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		prefix string
		suffix string
	}{
		{
			name:   "input-object-in-array",
			prefix: `{"Kind":"tool_call","ſeq":7,"call_id":"call","INP\u0055T":{"mixed":[false,null,{},[],{"text":`,
			suffix: `,"numbers":` + legacyNativeAnyIntegrationNumbers + `,"empty_key":{"":null},"duplicate":{"old":1},"dupli\u0063ate":{"new":2}}],"tail":true}}`,
		},
		{
			name:   "input-array-in-object",
			prefix: `{"\u006bind":"tool_call","seq":7,"inpu\u0074":{"outer":{"mixed":[{"text":`,
			suffix: `},` + legacyNativeAnyIntegrationNumbers + ",null,[],{},false]},\"\":{},\"\ub05d\":[]}}",
		},
		{
			name:   "output-object-in-array",
			prefix: `{"Kind":"tool_result","ſeq":7,"call_id":"call","OUTP\u0055T":[false,null,[],{}, {"nested":{"text":`,
			suffix: `,"numbers":` + legacyNativeAnyIntegrationNumbers + `,"duplicate":[1,2],"dupli\u0063ate":[null]}}]}`,
		},
		{
			name:   "output-array-in-object",
			prefix: `{"\u006bind":"tool_result","seq":7,"outp\u0075t":{"outer":{"mixed":[`,
			suffix: `,` + legacyNativeAnyIntegrationNumbers + ",null,[],{},false]},\"\":null,\"\ud55c🙂\":{\"empty\":{}}}}",
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_ = legacyNativeOutputReviewString(test, legacyNativeAnyIntegrationDocumentPrefix+fixture.prefix)
		})
	}
}

func TestLegacyNativeAnyIntegrationDeepMixedShapes(test *testing.T) {
	for _, depth := range []int{65, 257} {
		for _, field := range []string{"input", "output"} {
			test.Run(fmt.Sprintf("%s/depth-%d", field, depth), func(test *testing.T) {
				var prefix strings.Builder
				if field == "input" {
					prefix.WriteString(`{"kind":"tool_call","input":{"root":`)
				} else {
					prefix.WriteString(`{"kind":"tool_result","output":`)
				}
				for level := 0; level < depth; level++ {
					if level%2 == 0 {
						prefix.WriteString("{\"\ud55c🙂\":")
					} else {
						prefix.WriteString(`[0.1,null,{},[],`)
					}
				}
				prefix.WriteString(`{"text":`)
				_ = legacyNativeOutputReviewString(test, legacyNativeAnyIntegrationDocumentPrefix+prefix.String())
				var suffix strings.Builder
				suffix.WriteString(`,"numbers":` + legacyNativeAnyIntegrationNumbers + `,"nil":null,"empty":{}}`)
				for level := depth - 1; level >= 0; level-- {
					if level%2 == 0 {
						suffix.WriteByte('}')
					} else {
						suffix.WriteByte(']')
					}
				}
				if field == "input" {
					suffix.WriteByte('}')
				}
				suffix.WriteByte('}')
			})
		}
	}
}
