//go:build postgres

package store

import (
	"encoding/json"
	"reflect"
	"testing"
)

func docNormalizationPGLeaseImage(before, after string) bool {
	var prior, current map[string]any
	if json.Unmarshal([]byte(before), &prior) != nil || json.Unmarshal([]byte(after), &current) != nil {
		return false
	}
	priorJob, priorOK := prior["job"].([]any)
	currentJob, currentOK := current["job"].([]any)
	if !priorOK || !currentOK || len(priorJob) != 2 || len(currentJob) != 2 {
		return false
	}
	priorFields, priorOK := priorJob[0].(map[string]any)
	currentFields, currentOK := currentJob[0].(map[string]any)
	if !priorOK || !currentOK || priorFields["lease_until"] == nil || currentFields["lease_until"] == nil {
		return false
	}
	currentFields["lease_until"], currentJob[1] = priorFields["lease_until"], priorJob[1]
	return reflect.DeepEqual(prior, current)
}

func TestDocNormalizationFixtureLeaseImage(test *testing.T) {
	before := `{"job":[{"lease_until":"before","version":1},"xmin-before"],"source":"original"}`
	for _, scenario := range []struct {
		name  string
		after string
		valid bool
	}{
		{"renewal", `{"job":[{"lease_until":"after","version":1},"xmin-after"],"source":"original"}`, true},
		{"job fence", `{"job":[{"lease_until":"after","version":2},"xmin-after"],"source":"original"}`, false},
		{"source", `{"job":[{"lease_until":"after","version":1},"xmin-after"],"source":"changed"}`, false},
		{"missing job", `{"job":null,"source":"original"}`, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if docNormalizationPGLeaseImage(before, scenario.after) != scenario.valid {
				test.Fatal("fixture renewal hid an unrelated public mutation", scenario.after)
			}
		})
	}
}
