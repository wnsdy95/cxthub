package domain

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"unicode/utf8"
)

func FuzzDocToolInlineKeyMatchesPreparedDigest(fuzzer *testing.F) {
	for _, token := range []string{`""`, `"0"`, `"\u0030"`, `"4294967294"`, `"4294967295"`, `"\ud800"`, `"\ud800\udc00"`, `"a\\b\n\u0000"`} {
		fuzzer.Add([]byte(token))
	}
	fuzzer.Fuzz(func(test *testing.T, token []byte) {
		if len(token) < 2 || len(token) > docToolKeyMapInlineTokenBytes || token[0] != '"' || token[len(token)-1] != '"' || !utf8.Valid(token) || !json.Valid(token) {
			return
		}
		fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":{`+string(token)+`:0}}`)
		if err := fixture.scan(test); err != nil {
			test.Fatal(err)
		}
		source, err := NewDocToolKeyMapSource(fixture.source, fixture.steps[len(fixture.steps)-1].State())
		if err != nil {
			test.Fatal(err)
		}
		state, err := NewDocToolKeyMapState(fixture.source, source)
		if err != nil {
			test.Fatal(err)
		}
		nodes := slices.Clone(fixture.nodes)
		slices.SortFunc(nodes, func(left, right DocToolTreeNode) int { return left.Value.From - right.Value.From })
		step, err := StepDocToolKeyMap(context.Background(), fixture.source, source, state, fixture.read, fixture.load,
			func(context.Context, int, int) ([]DocToolTreeNode, error) { return nodes, nil })
		if err != nil || !step.State().Done() || len(step.Items()) != 1 || len(step.Objects()) != 0 {
			test.Fatal("inline key was not completed without disposable plans", err)
		}
		quoted := prepareDocToolStringFixture(test, string(token), DocToolStringQuoted)
		digestSource, err := NewDocToolKeyDigestSource(quoted.plan)
		if err != nil {
			test.Fatal(err)
		}
		expected := runDocToolKeyDigestFixture(test, digestSource, quoted.objects).result
		actual := step.Items()[0]
		if actual.Digest != expected.Digest || actual.IsArrayIndex != expected.IsArrayIndex || actual.ArrayIndex != expected.ArrayIndex {
			test.Fatal("inline and prepared canonical key identities differ", actual, expected)
		}
	})
}
