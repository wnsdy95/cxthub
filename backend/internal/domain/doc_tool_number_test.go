package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func docToolNumberTestSource(test *testing.T, token string) (DocToolNumberSource, []byte) {
	test.Helper()
	const prefix = `{"kind":"tool_result","output":`
	raw := []byte(prefix + token + `,"private":"not selected"}`)
	span := DocFindSpan{From: len(prefix), To: len(prefix) + len(token)}
	tree := DocToolTreeSource{Version: 1, Profile: DocToolTreeProfile, EventHash: HashContent(raw), EventBytes: len(raw), Evidence: HashContent([]byte("verified tree evidence")), Root: span, Type: DocToolValueNumber}
	source, err := NewDocToolNumberSource(tree, DocToolTreeNode{Value: span, Type: DocToolValueNumber, Parent: -1})
	if err != nil {
		test.Fatal(err)
	}
	return source, raw
}

func docToolNumberTestPrepare(test *testing.T, token string) (DocToolNumberPlan, []DocToolNumberState) {
	test.Helper()
	source, raw := docToolNumberTestSource(test, token)
	encodedSource, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	source, err = DecodeDocToolNumberSource(HashContent(encodedSource), encodedSource)
	if err != nil {
		test.Fatal(err)
	}
	state, err := NewDocToolNumberState(source)
	if err != nil {
		test.Fatal(err)
	}
	var states []DocToolNumberState
	totalRead := 0
	for !state.Done() {
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		states = append(states, state)
		calls, readBytes := 0, 0
		step, err := StepDocToolNumber(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) {
			calls++
			readBytes += length
			if from != state.Next || length <= 0 || length > DocToolNumberStepSourceBytes || from < source.Node.Value.From || from > source.Node.Value.To-length {
				test.Fatal("read escaped exact token or budget", from, length)
			}
			return bytes.Clone(raw[from : from+length]), nil
		})
		if err != nil {
			test.Fatalf("valid token rejected (length %d, offset %d): %v", len(token), state.Next-source.Node.Value.From, err)
		}
		if calls != 1 || readBytes > DocToolNumberStepSourceBytes || step.ReadBytes() != readBytes || step.Prior() != HashContent(before) || step.State().Next-state.Next != readBytes {
			test.Fatal("invalid sealed step receipt")
		}
		totalRead += readBytes
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("prior checkpoint mutated", err)
		}
		copyState := step.State()
		copyState.Coefficient[0] ^= 1
		copyState.HashState[4] ^= 1
		if copyState == step.State() {
			test.Fatal("state getter aliases sealed step")
		}
		encoded, err := step.State().Encode(source)
		if err != nil || len(encoded) > MaxDocToolNumberStateBytes || len(encoded) > 5000 {
			test.Fatal("unbounded or invalid checkpoint", len(encoded), err)
		}
		state, err = DecodeDocToolNumberState(source, HashContent(encoded), encoded)
		if err != nil {
			test.Fatal("restart failed", err)
		}
	}
	states = append(states, state)
	if totalRead != len(token) || len(states)-1 != (len(token)+DocToolNumberStepSourceBytes-1)/DocToolNumberStepSourceBytes || state.Digest != HashContent([]byte(token)) {
		test.Fatal("scanning cost or complete-token digest changed", totalRead, len(token))
	}
	plan, err := FinishDocToolNumber(context.Background(), source, state)
	if err != nil || len(plan.Text) > 32 || plan.Validate() != nil {
		test.Fatal("finish failed", err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		test.Fatal(err)
	}
	plan, err = DecodeDocToolNumberPlan(HashContent(encoded), encoded)
	if err != nil {
		test.Fatal("plan restart failed", err)
	}
	for offset := range plan.Text {
		span, err := plan.SourceRange(offset, 1)
		want := source.Node.Value
		if token == plan.Text {
			want.From += offset
			want.To = want.From + 1
		}
		if err != nil || span != want || plan.Linear != (token == plan.Text) {
			test.Fatal("original-source provenance changed", span, want, err)
		}
	}
	return plan, states
}

func TestDocToolNumberFrozenFormatting(test *testing.T) {
	for token, want := range map[string]string{
		"0": "0", "-0": "0", "-0.000e9999": "0", "0e-9999": "0", "1.0": "1", "1E+000": "1", "-12.50": "-12.5",
		"0.000001": "0.000001", "0.0000009999999999999999": "0.000001", "1e-7": "1e-7", "1e+09": "1000000000",
		"1e20": "100000000000000000000", "1e21": "1e+21", "1000000000000000100": "1000000000000000100",
		"1e309": "null", "-1e309": "null", "1e-9999": "0", "-1e-9999": "0", "5e-324": "5e-324",
		"2.4703282292062327e-324": "0", "2.4703282292062328e-324": "5e-324", "9007199254740993": "9007199254740992",
		"1.7976931348623157e308": "1.7976931348623157e+308", "1.7976931348623159e308": "null",
	} {
		test.Run(token, func(test *testing.T) {
			plan, _ := docToolNumberTestPrepare(test, token)
			if plan.Text != want {
				test.Fatalf("got %s, want %s", plan.Text, want)
			}
		})
	}
}

func TestDocToolNumberLongZerosExponentsAndBudgets(test *testing.T) {
	for _, size := range []int{2047, 2048, 2049, 65534, 65535, 65536, 65537, 200003} {
		for index, pair := range [][2]string{
			{"1" + strings.Repeat("0", size) + fmt.Sprintf("e-%d", size), "1"},
			{"-0." + strings.Repeat("0", size) + "e99999", "0"},
			{"0." + strings.Repeat("0", size) + "1" + fmt.Sprintf("e%d", size+1), "1"},
			{"1e+" + strings.Repeat("0", size) + "1", "10"},
			{"1e" + strings.Repeat("9", size), "null"},
			{"-1e-" + strings.Repeat("9", size), "0"},
		} {
			test.Run(fmt.Sprintf("%d/%d", size, index), func(test *testing.T) {
				plan, _ := docToolNumberTestPrepare(test, pair[0])
				if plan.Text != pair[1] {
					test.Fatal("long exponent or zero run changed rounding", plan.Text, pair[1])
				}
			})
		}
	}
}

func TestDocToolNumberRejectsMalformedTokens(test *testing.T) {
	tokens := []string{"-", "+0", "+1", "00", "01", "-00", ".1", "1.", "0.", "1.e2", "1e", "1e+", "1e-", "1ee2", "1e1e1", "1e--2", "1e+-2", "1e2.0", " 1", "1 ", "1\n", "1,", "1]", "1x", "1\x00", "NaN", "Infinity", "0x1", "1_2", "١", "−1", "1/2", "--1", "true", "null", "{}"}
	for _, prefix := range []string{"1" + strings.Repeat("0", 65535), "0." + strings.Repeat("0", 65534), "1e" + strings.Repeat("0", 65534)} {
		for _, suffix := range []string{" ", "x", ".", "e", "+", "\x00"} {
			tokens = append(tokens, prefix+suffix)
		}
	}
	for index, token := range tokens {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			source, raw := docToolNumberTestSource(test, token)
			state, _ := NewDocToolNumberState(source)
			failed := false
			for !state.Done() {
				before := state
				step, err := StepDocToolNumber(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length], nil })
				if state != before {
					test.Fatal("failure mutated input")
				}
				if err != nil {
					if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolNumberStep{}) {
						test.Fatal("malformed token leaked result", err)
					}
					failed = true
					break
				}
				state = step.State()
			}
			if !failed {
				test.Fatal("malformed token accepted", len(token))
			}
		})
	}
}

func TestDocToolNumberCounterAndCodecIntegrity(test *testing.T) {
	token := "0." + strings.Repeat("0", 31) + "123456789" + strings.Repeat("0", 150000) + "7e-42"
	plan, states := docToolNumberTestPrepare(test, token)
	source := plan.Source
	for index, state := range states {
		for name, mutate := range map[string]func(*DocToolNumberState){
			"version":           func(value *DocToolNumberState) { value.Version++ },
			"source":            func(value *DocToolNumberState) { value.Source = HashContent([]byte("different")) },
			"offset":            func(value *DocToolNumberState) { value.Next++ },
			"integer":           func(value *DocToolNumberState) { value.IntegerDigits++ },
			"fraction":          func(value *DocToolNumberState) { value.FractionDigits++ },
			"significant":       func(value *DocToolNumberState) { value.SignificantDigits++ },
			"leading":           func(value *DocToolNumberState) { value.LeadingZeros++ },
			"retained":          func(value *DocToolNumberState) { value.RetainedDigits++ },
			"negative_count":    func(value *DocToolNumberState) { value.Negative = !value.Negative },
			"dot":               func(value *DocToolNumberState) { value.Dot = !value.Dot },
			"integer_zero":      func(value *DocToolNumberState) { value.IntegerZero = !value.IntegerZero },
			"exponent_digits":   func(value *DocToolNumberState) { value.ExponentDigits++ },
			"exponent":          func(value *DocToolNumberState) { value.Exponent = math.MaxInt64 },
			"exponent_sign":     func(value *DocToolNumberState) { value.ExponentSign = 'x' },
			"phase":             func(value *DocToolNumberState) { value.Phase = "unknown" },
			"complete":          func(value *DocToolNumberState) { value.Complete = !value.Complete },
			"negative_counter":  func(value *DocToolNumberState) { value.LeadingZeros = -1 },
			"oversized_counter": func(value *DocToolNumberState) { value.SignificantDigits = math.MaxInt },
			"coefficient":       func(value *DocToolNumberState) { value.Coefficient[127] = math.MaxUint64 },
		} {
			test.Run(fmt.Sprintf("%d/%s", index, name), func(test *testing.T) {
				bad := state
				mutate(&bad)
				raw, _ := canonicalJSON(bad)
				if bad.Validate(source) == nil {
					test.Fatal("inconsistent counter accepted")
				}
				if _, err := DecodeDocToolNumberState(source, HashContent(raw), raw); err == nil {
					test.Fatal("inconsistent counter restarted")
				}
			})
		}
		encoded, _ := state.Encode(source)
		for _, bad := range [][]byte{append(bytes.Clone(encoded), ' '), bytes.Replace(encoded, []byte(`"version":1`), []byte(`"unknown":0,"version":1`), 1), append([]byte(`{"version":1,`), encoded[1:]...)} {
			if _, err := DecodeDocToolNumberState(source, HashContent(bad), bad); err == nil {
				test.Fatal("noncanonical checkpoint accepted")
			}
		}
		if _, err := DecodeDocToolNumberState(source, HashContent([]byte("other")), encoded); err == nil {
			test.Fatal("wrong checkpoint hash accepted")
		}
		other := source
		other.Tree.EventHash = HashContent([]byte("changed event"))
		if _, err := DecodeDocToolNumberState(other, HashContent(encoded), encoded); err == nil {
			test.Fatal("checkpoint transplanted across sources")
		}
	}
	for _, text := range []string{"", "-0", "1.0", "1e+09", "1e21", "NaN", "Infinity", strings.Repeat("1", 33)} {
		bad := plan
		bad.Text = text
		if bad.Validate() == nil {
			test.Fatal("noncanonical plan text accepted", text)
		}
	}
}

func TestDocToolNumberCancellationAndLoadFailureZeroEffects(test *testing.T) {
	source, raw := docToolNumberTestSource(test, "1"+strings.Repeat("0", 140000)+"e-140000")
	state, _ := NewDocToolNumberState(source)
	boom := errors.New("load failed")
	for _, variant := range []string{"pre_cancel", "load_cancel", "load_error", "short", "long", "nil"} {
		test.Run(variant, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if variant == "pre_cancel" {
				cancel()
			}
			calls := 0
			load := func(_ context.Context, from, length int) ([]byte, error) {
				calls++
				switch variant {
				case "load_cancel":
					cancel()
				case "load_error":
					return nil, boom
				case "short":
					length--
				case "long":
					length++
				}
				return bytes.Clone(raw[from : from+length]), nil
			}
			if variant == "nil" {
				load = nil
			}
			before := state
			step, err := StepDocToolNumber(ctx, source, state, load)
			if err == nil || !reflect.DeepEqual(step, DocToolNumberStep{}) || before != state || variant == "pre_cancel" && calls != 0 || variant == "load_error" && !errors.Is(err, boom) || strings.Contains(variant, "cancel") && !errors.Is(err, context.Canceled) {
				test.Fatal("failure returned effects", variant, err)
			}
			plan, err := FinishDocToolNumber(ctx, source, state)
			if err == nil || !reflect.DeepEqual(plan, DocToolNumberPlan{}) {
				test.Fatal("incomplete/canceled finish returned effects")
			}
		})
	}
	plan, states := docToolNumberTestPrepare(test, "1")
	if _, err := StepDocToolNumber(context.Background(), plan.Source, states[len(states)-1], func(context.Context, int, int) ([]byte, error) {
		test.Fatal("done state performed load")
		return nil, nil
	}); err == nil {
		test.Fatal("done state advanced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := FinishDocToolNumber(ctx, plan.Source, states[len(states)-1]); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, DocToolNumberPlan{}) {
		test.Fatal("canceled complete finish returned effects", err)
	}
}

func TestDocToolNumberSourceAndGetterIsolation(test *testing.T) {
	source, _ := docToolNumberTestSource(test, "12")
	for _, mutate := range []func(*DocToolNumberSource){
		func(value *DocToolNumberSource) { value.Node.Type = DocToolValueString },
		func(value *DocToolNumberSource) { value.Node.Value.To++ },
		func(value *DocToolNumberSource) { value.Node.Value.From++ },
		func(value *DocToolNumberSource) { value.Tree.EventHash = "invalid" },
		func(value *DocToolNumberSource) { value.Tree.Evidence = "invalid" },
		func(value *DocToolNumberSource) { value.Tree.EventBytes = 0 },
		func(value *DocToolNumberSource) { value.Node.Children = 1 },
		func(value *DocToolNumberSource) { value.Node.Key = &DocFindSpan{From: 0, To: 2} },
	} {
		bad := source
		mutate(&bad)
		if bad.Validate() == nil {
			test.Fatal("bad number source accepted")
		}
		if _, err := NewDocToolNumberSource(bad.Tree, bad.Node); err == nil {
			test.Fatal("bad factory input accepted")
		}
	}
	const nested = `{"a":12}`
	tree := source.Tree
	tree.Root = DocFindSpan{From: 0, To: len(nested)}
	tree.Type, tree.EventBytes, tree.EventHash = DocToolValueObject, len(nested), HashContent([]byte(nested))
	key := DocFindSpan{From: 1, To: 4}
	node := DocToolTreeNode{Value: DocFindSpan{From: 5, To: 7}, Type: DocToolValueNumber, Depth: 1, Parent: 0, Key: &key}
	nestedSource, err := NewDocToolNumberSource(tree, node)
	if err != nil {
		test.Fatal(err)
	}
	key.From = 2
	if nestedSource.Node.Key.From != 1 {
		test.Fatal("factory retained caller key pointer")
	}
	state, _ := NewDocToolNumberState(nestedSource)
	step, err := StepDocToolNumber(context.Background(), nestedSource, state, func(_ context.Context, from, length int) ([]byte, error) {
		return []byte(nested[from : from+length]), nil
	})
	if err != nil {
		test.Fatal(err)
	}
	getter := step.Source()
	getter.Node.Key.From = 2
	nestedSource.Node.Key.From = 3
	if step.Source().Node.Key.From != 1 {
		test.Fatal("source getter aliases sealed step")
	}
	state, _ = NewDocToolNumberState(source)
	bad := state
	bad.Source = HashContent([]byte("unrelated"))
	calls := 0
	if _, err := StepDocToolNumber(context.Background(), source, bad, func(context.Context, int, int) ([]byte, error) { calls++; return nil, nil }); err == nil || calls != 0 {
		test.Fatal("invalid checkpoint reached loader")
	}
}

func TestDocToolNumberHashCheckpointIntegrity(test *testing.T) {
	plan, states := docToolNumberTestPrepare(test, "1"+strings.Repeat("0", 140000)+"e-140000")
	for index, state := range states {
		for _, position := range []int{0, 36, 100, 107} {
			bad := state
			bad.HashState[position] ^= 1
			if bad.Validate(plan.Source) == nil {
				test.Fatal("bad hash buffer or byte count accepted", index, position)
			}
		}
		bad := state
		if state.Done() {
			bad.Digest = "invalid"
		} else {
			bad.Digest = HashContent([]byte("premature digest"))
		}
		if bad.Validate(plan.Source) == nil {
			test.Fatal("digest inconsistent with phase")
		}
		if !state.Done() && state.Next != plan.Source.Node.Value.From {
			if !bytes.Equal(state.HashState[36:100], make([]byte, 64)) {
				test.Fatal("checkpoint retained raw token bytes in hash buffer")
			}
		}
	}
}

type docToolNumberLateCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	at     int
}

func (ctx *docToolNumberLateCancelContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.at {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestDocToolNumberLateCancellationZeroesComputedResults(test *testing.T) {
	source, raw := docToolNumberTestSource(test, "1"+strings.Repeat("0", 65535)+"e-65535")
	state, _ := NewDocToolNumberState(source)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &docToolNumberLateCancelContext{Context: base, cancel: cancel, at: 66}
	step, err := StepDocToolNumber(ctx, source, state, func(_ context.Context, from, length int) ([]byte, error) { return raw[from : from+length], nil })
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocToolNumberStep{}) || ctx.calls != 66 {
		test.Fatal("late cancellation leaked fully computed step", ctx.calls, err)
	}
	plan, states := docToolNumberTestPrepare(test, "1")
	base, cancelFinish := context.WithCancel(context.Background())
	defer cancelFinish()
	ctx = &docToolNumberLateCancelContext{Context: base, cancel: cancelFinish, at: 2}
	result, err := FinishDocToolNumber(ctx, plan.Source, states[len(states)-1])
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, DocToolNumberPlan{}) {
		test.Fatal("late cancellation leaked fully computed plan", err)
	}
}

func TestDocToolNumberExactSyntaxAtStepBoundaries(test *testing.T) {
	for index, token := range []string{
		"1" + strings.Repeat("0", 65534) + ".0e-65534",
		"1" + strings.Repeat("0", 65534) + "e-65534",
		"1" + strings.Repeat("0", 65533) + "e-65533",
		"1e" + strings.Repeat("0", 65534) + "0",
	} {
		plan, states := docToolNumberTestPrepare(test, token)
		want := []string{"dot", "exponent", "exponent_sign", "exponent_digits"}[index]
		if plan.Text != "1" || states[1].Phase != want {
			test.Fatal("syntax checkpoint lost exact boundary", index, plan.Text, states[1].Phase)
		}
	}
}

func TestDocToolNumberUnrelatedEventSizeDoesNotChangeReadCost(test *testing.T) {
	for _, eventBytes := range []int{128, 1 << 20, MaxConversationDocumentBytes} {
		source, _ := docToolNumberTestSource(test, "1e-7")
		source.Tree.EventBytes = eventBytes
		source.Tree.EventHash = HashContent([]byte(fmt.Sprint(eventBytes)))
		state, err := NewDocToolNumberState(source)
		if err != nil {
			test.Fatal(err)
		}
		calls := 0
		step, err := StepDocToolNumber(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) {
			calls++
			if from != source.Node.Value.From || length != 4 {
				test.Fatal("number formatter read ancestors or unrelated event bytes")
			}
			return []byte("1e-7"), nil
		})
		if err != nil || calls != 1 || step.ReadBytes() != 4 || !step.State().Done() {
			test.Fatal("cost changed with unrelated event size", calls, err)
		}
		plan, err := FinishDocToolNumber(context.Background(), source, step.State())
		if err != nil || plan.Text != "1e-7" {
			test.Fatal("bounded scalar finish failed", err)
		}
	}
}

func TestDocToolNumberVerifiedTreeFactoryProvenance(test *testing.T) {
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":{"n":-0,"a":[5e-324,1e21,1e309]},"private":"PRIVATE_BODY_MARKER"}`)
	if err := fixture.scan(test); err != nil {
		test.Fatal(err)
	}
	seen := 0
	for _, node := range fixture.nodes {
		source, err := NewDocToolNumberSource(fixture.source, node)
		if node.Type != DocToolValueNumber {
			if err == nil {
				test.Fatal("non-number tree node accepted")
			}
			continue
		}
		if err != nil {
			test.Fatal(err)
		}
		state, _ := NewDocToolNumberState(source)
		step, err := StepDocToolNumber(context.Background(), source, state, func(_ context.Context, from, length int) ([]byte, error) {
			if from < node.Value.From || from > node.Value.To-length {
				test.Fatal("numeric step rescanned tree ancestors")
			}
			return bytes.Clone(fixture.raw[from : from+length]), nil
		})
		if err != nil {
			test.Fatal(err)
		}
		plan, err := FinishDocToolNumber(context.Background(), source, step.State())
		want := []string{"0", "5e-324", "1e+21", "null"}[seen]
		if err != nil || plan.Text != want || plan.Source.Tree.EventHash != HashContent(fixture.raw) || plan.Source.Node.Value != node.Value || plan.Source.Tree.Evidence != fixture.source.Evidence {
			test.Fatal("verified event/span identity lost", plan.Text, want, err)
		}
		seen++
	}
	if seen != 4 {
		test.Fatal("numeric leaves missing", seen)
	}
}
