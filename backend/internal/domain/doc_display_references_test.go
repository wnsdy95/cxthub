package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDocDisplayHashCandidatesConservativeAndBounded(test *testing.T) {
	first, second := HashContent([]byte("first")), HashContent([]byte("second"))
	raw, err := json.Marshal(map[string]any{string(first): []any{second, map[string]any{"digest": first, "literal": "prefix " + string(second)}}, "number": json.Number("1e9999999999")})
	if err != nil {
		test.Fatal(err)
	}
	got, err := DocDisplayHashCandidates(context.Background(), raw)
	want := []ContentHash{first, second}
	slices.Sort(want)
	if err != nil || !reflect.DeepEqual(got, want) {
		test.Fatal("candidate references lost keys, nested values or deduplication", got, err)
	}
	for _, raw := range []string{`null`, `"not a hash"`, `1e99999999`, `[]`, `{}`} {
		if hashes, err := DocDisplayHashCandidates(context.Background(), []byte(raw)); err != nil || len(hashes) != 0 {
			test.Fatal("valid reference-free JSON was not supported", raw, hashes, err)
		}
	}
	for _, raw := range [][]byte{nil, []byte("not JSON"), []byte(`{} []`), []byte(`{"x":`), {0xff}, []byte(strings.Repeat("[", 513) + "0" + strings.Repeat("]", 513)), []byte(strings.Repeat(" ", MaxDocDisplayPageBytes+1))} {
		if hashes, err := DocDisplayHashCandidates(context.Background(), raw); !errors.Is(err, ErrDocDisplayReferencesUnsupported) || hashes != nil {
			test.Fatal("unsupported input acquired an empty/partial edge witness", len(raw), hashes, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hashes, err := DocDisplayHashCandidates(ctx, raw); !errors.Is(err, context.Canceled) || hashes != nil {
		test.Fatal("cancelled extraction returned candidates", hashes, err)
	}
}

func TestDocDisplayHashCandidatesCoverActualToolObjects(test *testing.T) {
	fixture := newDocToolPolicyFixture(test, `{"kind":"tool_call","tool_name":"Edit","input":{"file_path":"/a/b/c/d","old_string":"before\n","new_string":"after\n"}}`)
	fixture.finish(test)
	for hash, raw := range fixture.objects {
		if _, err := DocDisplayHashCandidates(context.Background(), raw); err != nil {
			test.Fatal("actual immutable tool object is not indexable", hash, err)
		}
	}
	plan, err := decodeDocToolEventPlan(fixture.state.ToolEvent, fixture.objects[fixture.state.ToolEvent])
	if err != nil {
		test.Fatal(err)
	}
	root, err := DocDisplayHashCandidates(context.Background(), fixture.objects[fixture.state.ToolEvent])
	if err != nil || !slices.Contains(root, plan.Evidence) {
		test.Fatal("final policy evidence is not retained", err)
	}
	evidence, _ := decodeDocToolPolicyState(plan.Evidence, fixture.objects[plan.Evidence])
	refs, err := DocDisplayHashCandidates(context.Background(), fixture.objects[plan.Evidence])
	if err != nil || !slices.Contains(refs, evidence.Receipt) || !slices.Contains(refs, evidence.Source.RootPlan) {
		test.Fatal("completed receipt or root plan is not retained", err)
	}
}
