package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

type docToolKeyDigestFixture struct {
	source  DocToolKeyDigestSource
	objects map[ContentHash][]byte
	states  []DocToolKeyDigestState
	result  DocToolKeyDigestResult
}

func docToolKeyDigestTestSource(test testing.TB, token string) (DocToolKeyDigestSource, map[ContentHash][]byte) {
	test.Helper()
	prepared := prepareDocToolStringFixture(test, token, DocToolStringQuoted)
	source, err := NewDocToolKeyDigestSource(prepared.plan)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	source, err = DecodeDocToolKeyDigestSource(HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
	return source, prepared.objects
}

func docToolKeyDigestTestLoad(objects map[ContentHash][]byte) DocToolKeyDigestLoader {
	return func(_ context.Context, hash ContentHash) ([]byte, error) {
		raw, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return bytes.Clone(raw), nil
	}
}

func docToolKeyDigestSelectedLoad(test testing.TB, source DocToolKeyDigestSource, objects map[ContentHash][]byte, indices ...int) DocToolKeyDigestLoader {
	test.Helper()
	allowed := make(map[ContentHash]bool)
	budget := 0
	for _, index := range indices {
		if index < 0 {
			continue
		}
		leaf, err := ResolveDocRangeLeaf(context.Background(), source.Plan.Pages, index, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			allowed[hash] = true
			budget++
			return docToolKeyDigestTestLoad(objects)(ctx, hash)
		})
		if err != nil {
			test.Fatal(err)
		}
		allowed[leaf] = true
		budget++
	}
	return func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if !allowed[hash] || budget <= 0 {
			test.Fatal("read outside selected current/previous paths", hash)
		}
		budget--
		return docToolKeyDigestTestLoad(objects)(ctx, hash)
	}
}

func runDocToolKeyDigestFixture(test testing.TB, source DocToolKeyDigestSource, objects map[ContentHash][]byte) docToolKeyDigestFixture {
	test.Helper()
	state, err := NewDocToolKeyDigestState(source)
	if err != nil {
		test.Fatal(err)
	}
	fixture := docToolKeyDigestFixture{source: source, objects: objects}
	for !state.Done {
		fixture.states = append(fixture.states, state)
		before, err := state.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		load := docToolKeyDigestSelectedLoad(test, source, objects, state.NextPage-1, state.NextPage)
		step, err := StepDocToolKeyDigest(context.Background(), source, state, load)
		if err != nil || step.TextBytes <= 0 || step.TextBytes > 32<<10 || step.State.NextPage != state.NextPage+1 || step.State.TextBytes-state.TextBytes != step.TextBytes {
			test.Fatal("unbounded or failed step", err, step)
		}
		after, _ := state.Encode(source)
		if !bytes.Equal(before, after) {
			test.Fatal("previous state mutated")
		}
		raw, err := step.State.Encode(source)
		if err != nil || len(raw) >= 1024 || bytes.Contains(raw, []byte("PUBLIC_BODY_MARKER")) || bytes.Contains(raw, []byte("NEVER_PUBLIC_BODY_MARKER")) || !validDocFieldHashState(step.State.HashState, step.State.HashNext) {
			test.Fatal("checkpoint contains body or exceeds bound", len(raw), err)
		}
		if binary.BigEndian.Uint64(step.State.HashState[len(step.State.HashState)-8:]) != uint64(step.State.HashNext) || !bytes.Equal(step.State.HashState[4+sha256.Size:len(step.State.HashState)-8], make([]byte, sha256.BlockSize)) {
			test.Fatal("SHA checkpoint carries private tail")
		}
		state, err = DecodeDocToolKeyDigestState(source, HashContent(raw), raw)
		if err != nil {
			test.Fatal("checkpoint restart", err)
		}
		if err := ValidateDocToolKeyDigestCheckpoint(context.Background(), source, state, docToolKeyDigestSelectedLoad(test, source, objects, state.NextPage-1, state.NextPage-2)); err != nil {
			test.Fatal("checkpoint page verification", err)
		}
	}
	fixture.states = append(fixture.states, state)
	load := docToolKeyDigestSelectedLoad(test, source, objects, state.NextPage-1, state.NextPage-2)
	fixture.result, err = FinishDocToolKeyDigest(context.Background(), source, state, load)
	if err != nil {
		test.Fatal("finish", err)
	}
	raw, err := fixture.result.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocToolKeyDigestResult(source, HashContent(raw), raw)
	if err != nil || decoded != fixture.result {
		test.Fatal("result round trip", err)
	}
	return fixture
}

func prepareDocToolKeyDigestFixture(test testing.TB, token string) docToolKeyDigestFixture {
	test.Helper()
	source, objects := docToolKeyDigestTestSource(test, token)
	return runDocToolKeyDigestFixture(test, source, objects)
}

func TestDocToolKeyDigestSemanticAliases(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		canonical string
		tokens    []string
	}{
		{"ascii", `"same"`, []string{`"same"`, `"\u0073a\u006de"`}},
		{"pair", `"🙂"`, []string{`"🙂"`, `"\uD83D\uDE42"`, `"\ud83d\uDE42"`}},
		{"escapes", `"\"\\/"`, []string{`"\"\\\/"`, `"\u0022\u005c\u002f"`}},
		{"controls", `"\n\r\t\b\f\u0000"`, []string{`"\n\r\t\b\f\u0000"`, `"\u000A\u000D\u0009\u0008\u000c\u0000"`}},
		{"unicode", "\"한글\u2028\u2029\"", []string{"\"한글\u2028\u2029\"", `"\ud55c\uae00\u2028\u2029"`}},
		{"lone-alias", `"\ud800"`, []string{`"\ud800"`, `"\uD800"`}},
		{"replacement-alias", `"�"`, []string{`"�"`, `"\ufffd"`}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			plans := make(map[ContentHash]bool)
			for _, token := range scenario.tokens {
				fixture := prepareDocToolKeyDigestFixture(test, token)
				if fixture.result.Digest != HashContent([]byte(scenario.canonical)) || fixture.result.TextBytes != len(scenario.canonical) {
					test.Fatal("semantic alias digest mismatch", token, fixture.result)
				}
				if plans[fixture.result.Plan] {
					test.Fatal("original source identity was lost")
				}
				plans[fixture.result.Plan] = true
			}
		})
	}
	seen := make(map[ContentHash]bool)
	for _, token := range []string{`"\ud800"`, `"\ud801"`, `"\udc00"`, `"\udfff"`, `"�"`, `"é"`, `"e\u0301"`} {
		fixture := prepareDocToolKeyDigestFixture(test, token)
		if seen[fixture.result.Digest] {
			test.Fatal("distinct UTF16 keys collapsed", token)
		}
		seen[fixture.result.Digest] = true
	}
}

func TestDocToolKeyDigestArrayIndices(test *testing.T) {
	for _, scenario := range []struct {
		token string
		valid bool
		value uint32
	}{
		{`""`, false, 0}, {`"0"`, true, 0}, {`"1"`, true, 1}, {`"10"`, true, 10},
		{`"4294967293"`, true, 4294967293}, {`"4294967294"`, true, 4294967294},
		{`"4294967295"`, false, 0}, {`"4294967296"`, false, 0}, {`"9999999999"`, false, 0},
		{`"18446744073709551616"`, false, 0}, {`"00"`, false, 0}, {`"01"`, false, 0},
		{`"-0"`, false, 0}, {`"-1"`, false, 0}, {`"+1"`, false, 0}, {`" 1"`, false, 0}, {`"1 "`, false, 0},
		{`"1.0"`, false, 0}, {`"1e2"`, false, 0}, {`"１２"`, false, 0}, {`"١"`, false, 0},
		{`"\u0030"`, true, 0}, {`"\u0031\u0030"`, true, 10}, {`"\u0030\u0030"`, false, 0},
		{`"\u0034\u0032\u0039\u0034\u0039\u0036\u0037\u0032\u0039\u0034"`, true, 4294967294},
		{`"\u0034\u0032\u0039\u0034\u0039\u0036\u0037\u0032\u0039\u0035"`, false, 0},
		{`"\"1"`, false, 0}, {`"1\""`, false, 0}, {`"\\u0031"`, false, 0}, {`"1\n"`, false, 0},
	} {
		test.Run(scenario.token, func(test *testing.T) {
			fixture := prepareDocToolKeyDigestFixture(test, scenario.token)
			if fixture.result.IsArrayIndex != scenario.valid || fixture.result.ArrayIndex != scenario.value {
				test.Fatal("incorrect JS array index", fixture.result)
			}
			var decoded string
			if err := json.Unmarshal([]byte(scenario.token), &decoded); err != nil {
				test.Fatal(err)
			}
			canonical, _ := json.Marshal(decoded)
			if fixture.result.Digest != HashContent(canonical) {
				test.Fatal("numeric classification changed quoted digest")
			}
		})
	}
}

func TestDocToolKeyDigestLongCheckpointRestarts(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		token     string
		canonical string
		minPages  int
	}{
		{"directory-boundary", `"` + strings.Repeat(`\u0061`, 70000) + `"`, `"` + strings.Repeat("a", 70000) + `"`, 129},
		{"mixed", `"` + strings.Repeat(`PUBLIC_BODY_MARKER\u0061\uD83D\uDE42\ud800\"`, 3000) + `"`, `"` + strings.Repeat(`PUBLIC_BODY_MARKERa🙂\ud800\"`, 3000) + `"`, 10},
		{"digits", `"` + strings.Repeat("7", 100000) + `"`, `"` + strings.Repeat("7", 100000) + `"`, 3},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := prepareDocToolKeyDigestFixture(test, scenario.token)
			if fixture.result.Digest != HashContent([]byte(scenario.canonical)) || fixture.result.IsArrayIndex || fixture.result.ArrayIndex != 0 || fixture.source.Plan.Pages.Count < scenario.minPages || len(fixture.states) != fixture.source.Plan.Pages.Count+1 {
				test.Fatal("long resumed digest mismatch", fixture.result, fixture.source.Plan.Pages.Count)
			}
		})
	}
}

func docToolKeyDigestASCIIPlan(test testing.TB, text string, split int) (DocToolKeyDigestSource, map[ContentHash][]byte) {
	test.Helper()
	source, _ := docToolStringTestSource(text, DocToolStringQuoted)
	sourceHash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[ContentHash][]byte)
	var hashes []ContentHash
	from := 0
	for index, to := range []int{split, len(text)} {
		page := DocToolStringPage{Version: 1, Source: sourceHash, RawFrom: source.Token.From + from, RawTo: source.Token.From + to, Display: DocDisplayPage{Version: 5, Source: sourceHash, Index: index, From: from, Text: text[from:to], Runs: []DocDisplayRun{{To: to - from, SourceFrom: source.Token.From + from, SourceTo: source.Token.From + to, Linear: true}}}}
		raw, err := page.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		hashes = append(hashes, HashContent(raw))
		objects[HashContent(raw)] = raw
		from = to
	}
	var staged []DocRangeObject
	root, err := buildDocRangeDirectory(context.Background(), hashes, &staged)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range staged {
		objects[object.Hash] = object.Bytes
	}
	input, err := NewDocToolKeyDigestSource(DocToolStringPlan{Version: 1, Source: source, Pages: root, TextBytes: len(text)})
	if err != nil {
		test.Fatal(err)
	}
	return input, objects
}

func TestDocToolKeyDigestEveryTailAndShortFinalPage(test *testing.T) {
	seen := make(map[int]bool)
	for remainder := 0; remainder < sha256.BlockSize; remainder++ {
		test.Run(fmt.Sprint(remainder), func(test *testing.T) {
			text := `"` + strings.Repeat("a", 256+remainder-1) + `z"`
			source, objects := docToolKeyDigestASCIIPlan(test, text, 256+remainder)
			fixture := runDocToolKeyDigestFixture(test, source, objects)
			state := fixture.states[len(fixture.states)-1]
			seen[state.TextBytes-state.HashNext] = true
			if fixture.result.Digest != HashContent([]byte(text)) || fixture.states[1].TextBytes-fixture.states[1].HashNext != remainder {
				test.Fatal("SHA tail replay mismatch", remainder)
			}
			lastHash, err := ResolveDocRangeLeaf(context.Background(), source.Plan.Pages, 1, docToolKeyDigestTestLoad(objects))
			if err != nil {
				test.Fatal(err)
			}
			loadedLast := false
			_, err = FinishDocToolKeyDigest(context.Background(), source, state, func(ctx context.Context, hash ContentHash) ([]byte, error) {
				loadedLast = loadedLast || hash == lastHash
				return docToolKeyDigestTestLoad(objects)(ctx, hash)
			})
			if err != nil || !loadedLast {
				test.Fatal("finish did not rehydrate final page", err)
			}
		})
	}
	if len(seen) != sha256.BlockSize {
		test.Fatal("missing SHA tail lengths", len(seen))
	}
}

func TestDocToolKeyDigestRejectsMalformedStateBeforeIO(test *testing.T) {
	source, objects := docToolKeyDigestASCIIPlan(test, `"`+strings.Repeat("a", 500)+`"`, 300)
	fixture := runDocToolKeyDigestFixture(test, source, objects)
	for name, mutate := range map[string]func(*DocToolKeyDigestState){
		"version":       func(state *DocToolKeyDigestState) { state.Version++ },
		"plan":          func(state *DocToolKeyDigestState) { state.Plan = HashContent([]byte("another plan")) },
		"raw-overflow":  func(state *DocToolKeyDigestState) { state.Next = math.MaxInt },
		"raw-negative":  func(state *DocToolKeyDigestState) { state.Next = -1 },
		"page-overflow": func(state *DocToolKeyDigestState) { state.NextPage = math.MaxInt },
		"page-negative": func(state *DocToolKeyDigestState) { state.NextPage = -1 },
		"text-overflow": func(state *DocToolKeyDigestState) { state.TextBytes = math.MaxInt },
		"hash-next":     func(state *DocToolKeyDigestState) { state.HashNext++ },
		"hash-header":   func(state *DocToolKeyDigestState) { state.HashState[0] ^= 1 },
		"private-tail":  func(state *DocToolKeyDigestState) { state.HashState[4+sha256.Size] = 'x' },
		"hash-count": func(state *DocToolKeyDigestState) {
			binary.BigEndian.PutUint64(state.HashState[len(state.HashState)-8:], math.MaxUint64)
		},
		"hash-length": func(state *DocToolKeyDigestState) { state.HashState = state.HashState[:len(state.HashState)-1] },
		"done":        func(state *DocToolKeyDigestState) { state.Done = true },
		"digits":      func(state *DocToolKeyDigestState) { state.IndexDigits = 1 },
		"value":       func(state *DocToolKeyDigestState) { state.IndexValue = 1 },
		"candidate":   func(state *DocToolKeyDigestState) { state.IndexPossible = true },
	} {
		test.Run(name, func(test *testing.T) {
			state := fixture.states[1]
			state.HashState = bytes.Clone(state.HashState)
			mutate(&state)
			load := func(context.Context, ContentHash) ([]byte, error) {
				test.Fatal("invalid state reached object loader")
				return nil, nil
			}
			step, err := StepDocToolKeyDigest(context.Background(), source, state, load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolKeyDigestStep{}) {
				test.Fatal("invalid state returned progress", err)
			}
			raw, _ := canonicalJSON(state)
			decoded, err := DecodeDocToolKeyDigestState(source, HashContent(raw), raw)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocToolKeyDigestState{}) {
				test.Fatal("invalid state decoded", err)
			}
		})
	}
	initial := fixture.states[0]
	initial.HashState = bytes.Clone(initial.HashState)
	initial.HashState[4] ^= 1
	if initial.Validate(source) == nil {
		test.Fatal("forged initial SHA state accepted")
	}
}

func TestDocToolKeyDigestCanonicalCodecsAndBindings(test *testing.T) {
	fixture := prepareDocToolKeyDigestFixture(test, `"42"`)
	source := fixture.source
	state := fixture.states[1]
	for _, scenario := range []struct {
		name   string
		raw    []byte
		limit  int
		decode func(ContentHash, []byte) bool
	}{
		{"source", mustDocToolKeyDigestJSON(test, source.Encode), MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) bool {
			value, err := DecodeDocToolKeyDigestSource(hash, raw)
			return errors.Is(err, ErrIntegrity) && reflect.DeepEqual(value, DocToolKeyDigestSource{})
		}},
		{"state", mustDocToolKeyDigestJSON(test, func() ([]byte, error) { return state.Encode(source) }), MaxDocToolKeyDigestStateBytes, func(hash ContentHash, raw []byte) bool {
			value, err := DecodeDocToolKeyDigestState(source, hash, raw)
			return errors.Is(err, ErrIntegrity) && reflect.DeepEqual(value, DocToolKeyDigestState{})
		}},
		{"result", mustDocToolKeyDigestJSON(test, func() ([]byte, error) { return fixture.result.Encode(source) }), MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) bool {
			value, err := DecodeDocToolKeyDigestResult(source, hash, raw)
			return errors.Is(err, ErrIntegrity) && value == (DocToolKeyDigestResult{})
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for _, raw := range [][]byte{nil, append(bytes.Clone(scenario.raw), ' '), append([]byte(`{"unknown":0,`), scenario.raw[1:]...), append([]byte(`{"version":1,`), scenario.raw[1:]...), bytes.Replace(scenario.raw, []byte(`"version":1`), []byte(`"version":2`), 1), bytes.Repeat([]byte("x"), scenario.limit+1)} {
				if !scenario.decode(HashContent(raw), raw) {
					test.Fatal("noncanonical or invalid encoding accepted")
				}
			}
			if !scenario.decode(HashContent([]byte("wrong hash")), scenario.raw) {
				test.Fatal("wrong expected hash accepted")
			}
		})
	}
	alias, _ := docToolKeyDigestTestSource(test, `"\u0034\u0032"`)
	if state.Validate(alias) == nil || fixture.result.Validate(alias) == nil {
		test.Fatal("checkpoint or result accepted another source's semantic alias")
	}
	for _, mode := range []DocToolStringMode{DocToolStringAdd, DocToolStringRemove} {
		plan := source.Plan
		plan.Source.Mode = mode
		value, err := NewDocToolKeyDigestSource(plan)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(value, DocToolKeyDigestSource{}) {
			test.Fatal("nonquoted source accepted")
		}
	}
	for _, mutate := range []func(*DocToolKeyDigestResult){
		func(result *DocToolKeyDigestResult) { result.Digest = "invalid" },
		func(result *DocToolKeyDigestResult) { result.TextBytes++ },
		func(result *DocToolKeyDigestResult) { result.ArrayIndex = math.MaxUint32 },
		func(result *DocToolKeyDigestResult) { result.IsArrayIndex = false },
	} {
		result := fixture.result
		mutate(&result)
		if result.Validate(source) == nil {
			test.Fatal("malformed result accepted")
		}
	}
}

func mustDocToolKeyDigestJSON(test testing.TB, encode func() ([]byte, error)) []byte {
	test.Helper()
	raw, err := encode()
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func TestDocToolKeyDigestFailuresAndCancellationAreAtomic(test *testing.T) {
	source, objects := docToolKeyDigestASCIIPlan(test, `"`+strings.Repeat("a", 300)+`"`, 300)
	fixture := runDocToolKeyDigestFixture(test, source, objects)
	sentinel := errors.New("selected object unavailable")
	for _, operation := range []string{"step-initial", "step-resume", "finish"} {
		test.Run(operation, func(test *testing.T) {
			state := fixture.states[0]
			if operation == "step-resume" {
				state = fixture.states[1]
			} else if operation == "finish" {
				state = fixture.states[2]
			}
			before := mustDocToolKeyDigestJSON(test, func() ([]byte, error) { return state.Encode(source) })
			run := func(ctx context.Context, load DocToolKeyDigestLoader) (bool, error) {
				if operation == "finish" {
					result, err := FinishDocToolKeyDigest(ctx, source, state, load)
					return result == (DocToolKeyDigestResult{}), err
				}
				step, err := StepDocToolKeyDigest(ctx, source, state, load)
				return reflect.DeepEqual(step, DocToolKeyDigestStep{}), err
			}
			calls := 0
			_, err := run(context.Background(), func(ctx context.Context, hash ContentHash) ([]byte, error) {
				calls++
				return docToolKeyDigestTestLoad(objects)(ctx, hash)
			})
			if err != nil || calls == 0 {
				test.Fatal("baseline", err)
			}
			for failAt := 1; failAt <= calls; failAt++ {
				for _, mode := range []string{"error", "corrupt", "cancel"} {
					ctx, cancel := context.WithCancel(context.Background())
					seen := 0
					zero, err := run(ctx, func(ctx context.Context, hash ContentHash) ([]byte, error) {
						seen++
						raw, err := docToolKeyDigestTestLoad(objects)(ctx, hash)
						if seen == failAt {
							switch mode {
							case "error":
								return nil, sentinel
							case "corrupt":
								return append(raw, ' '), nil
							case "cancel":
								cancel()
							}
						}
						return raw, err
					})
					cancel()
					want := sentinel
					if mode == "corrupt" {
						want = ErrIntegrity
					} else if mode == "cancel" {
						want = context.Canceled
					}
					if !zero || !errors.Is(err, want) {
						test.Fatal("failure returned partial progress", mode, failAt, zero, err)
					}
					after := mustDocToolKeyDigestJSON(test, func() ([]byte, error) { return state.Encode(source) })
					if !bytes.Equal(before, after) {
						test.Fatal("failed operation mutated prior state")
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			zero, err := run(ctx, func(context.Context, ContentHash) ([]byte, error) {
				test.Fatal("canceled operation performed IO")
				return nil, nil
			})
			if !zero || !errors.Is(err, context.Canceled) {
				test.Fatal("pre-canceled operation returned progress")
			}
		})
	}
	for _, state := range fixture.states[:2] {
		result, err := FinishDocToolKeyDigest(context.Background(), source, state, docToolKeyDigestTestLoad(objects))
		if !errors.Is(err, ErrIntegrity) || result != (DocToolKeyDigestResult{}) {
			test.Fatal("unfinished checkpoint yielded digest")
		}
	}
	step, err := StepDocToolKeyDigest(context.Background(), source, fixture.states[2], docToolKeyDigestTestLoad(objects))
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolKeyDigestStep{}) {
		test.Fatal("completed checkpoint advanced")
	}
	if step, err := StepDocToolKeyDigest(context.Background(), source, fixture.states[0], nil); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolKeyDigestStep{}) {
		test.Fatal("nil step loader accepted")
	}
	if result, err := FinishDocToolKeyDigest(context.Background(), source, fixture.states[2], nil); !errors.Is(err, ErrIntegrity) || result != (DocToolKeyDigestResult{}) {
		test.Fatal("nil finish loader accepted")
	}
}

func TestDocToolKeyDigestCheckpointPositionAndPageAdjacency(test *testing.T) {
	source, objects := docToolKeyDigestASCIIPlan(test, `"`+strings.Repeat("a", 500)+`"`, 300)
	fixture := runDocToolKeyDigestFixture(test, source, objects)
	state := fixture.states[1]
	for _, mutate := range []func(*DocToolKeyDigestState){
		func(value *DocToolKeyDigestState) { value.Next++ },
		func(value *DocToolKeyDigestState) { value.TextBytes++ },
	} {
		corrupt := state
		mutate(&corrupt)
		if corrupt.Validate(source) != nil {
			test.Fatal("position corruption should require selected page verification")
		}
		if err := ValidateDocToolKeyDigestCheckpoint(context.Background(), source, corrupt, docToolKeyDigestTestLoad(objects)); !errors.Is(err, ErrIntegrity) {
			test.Fatal("checkpoint page position mismatch accepted", err)
		}
		step, err := StepDocToolKeyDigest(context.Background(), source, corrupt, docToolKeyDigestTestLoad(objects))
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolKeyDigestStep{}) {
			test.Fatal("mismatched checkpoint yielded progress", err)
		}
	}
	page, err := ReadDocToolStringPage(context.Background(), source.Plan, 1, docToolKeyDigestTestLoad(objects))
	if err != nil {
		test.Fatal(err)
	}
	page.RawFrom++
	page.Display.From++
	page.Display.Text = page.Display.Text[1:]
	page.Display.Runs[0].To--
	page.Display.Runs[0].SourceFrom++
	raw, err := page.Encode(source.Plan.Source)
	if err != nil {
		test.Fatal(err)
	}
	first, err := ResolveDocRangeLeaf(context.Background(), source.Plan.Pages, 0, docToolKeyDigestTestLoad(objects))
	if err != nil {
		test.Fatal(err)
	}
	var staged []DocRangeObject
	source.Plan.Pages, err = buildDocRangeDirectory(context.Background(), []ContentHash{first, HashContent(raw)}, &staged)
	if err != nil {
		test.Fatal(err)
	}
	objects[HashContent(raw)] = raw
	for _, object := range staged {
		objects[object.Hash] = object.Bytes
	}
	state, err = NewDocToolKeyDigestState(source)
	if err != nil {
		test.Fatal(err)
	}
	initial, err := StepDocToolKeyDigest(context.Background(), source, state, docToolKeyDigestTestLoad(objects))
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocToolKeyDigest(context.Background(), source, initial.State, docToolKeyDigestTestLoad(objects))
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolKeyDigestStep{}) {
		test.Fatal("gapped prepared pages accepted", err)
	}
}
