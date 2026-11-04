package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const docToolPolicyReviewMaxSteps = 2048

func docToolPolicyReviewJSON(test *testing.T, value any) []byte {
	test.Helper()
	raw, err := canonicalJSON(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func docToolPolicyReviewLocalNode(test *testing.T, sources []string) []string {
	test.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		test.Skip("local Node is unavailable; no external oracle is downloaded")
	}
	const script = `
const fs = require('node:fs');
const sources = JSON.parse(fs.readFileSync(0, 'utf8'));
const text = value => typeof value === 'string' ? value : '';
const diff = (value, prefix) => text(value).split('\n').map(line => prefix + line).join('\n');
const rendered = sources.map(source => {
  const event = JSON.parse(source);
  if (event.kind === 'tool_result') {
    const output = typeof event.output === 'string' ? event.output : event.output != null ? JSON.stringify(event.output, null, 2) : '';
    return output.trim() ? output : '';
  }
  if (event.kind !== 'tool_call') throw new Error('not a tool call');
  const name = event.provider_tool_name || event.tool_name || 'tool';
  const input = event.input ?? {};
  const path = text(input.file_path);
  const head = path ? (path.split('/').length > 4 ? '…/' + path.split('/').slice(-3).join('/') : path) : text(input.command) || text(input.pattern) || text(input.url);
  let output = name + (head ? '(' + head + ')' : '');
  if (name === 'Edit' && (input.old_string || input.new_string)) {
    if (text(input.old_string)) output += '\n' + diff(input.old_string, '− ');
    if (text(input.new_string)) output += '\n' + diff(input.new_string, '+ ');
  } else if (name === 'Write' && input.content) {
    output += '\n' + diff(input.content, '+ ');
  } else if (name === 'Bash' && input.command) {
    output += '\n$ ' + text(input.command);
  } else if (Object.keys(input).length) {
    output += '\n' + JSON.stringify(input, null, 2);
  }
  return output;
});
process.stdout.write(JSON.stringify(rendered));
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-e", script)
	command.Stdin = bytes.NewReader(docToolPolicyReviewJSON(test, sources))
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	raw, err := command.Output()
	if err != nil {
		test.Fatalf("local Node oracle failed: %v; %s", err, diagnostic.String())
	}
	var rendered []string
	if json.Unmarshal(raw, &rendered) != nil || len(rendered) != len(sources) {
		test.Fatal("invalid local Node oracle response")
	}
	return rendered
}

func docToolPolicyReviewFinish(test *testing.T, fixture *docToolPolicyFixture) DocDisplayProjectionGeneration {
	test.Helper()
	for count := 0; count < docToolPolicyReviewMaxSteps && fixture.state.Phase != "completed"; count++ {
		fixture.advance(test)
	}
	if fixture.state.Phase != "completed" {
		test.Fatalf("bounded policy replay did not finish: source=%s bytes=%d phase=%s active=%s", HashContent(fixture.raw), len(fixture.raw), fixture.state.Phase, fixture.state.Active)
	}
	generation, err := ValidateDocEventDisplayPublication(context.Background(), fixture.state, fixture.load)
	if err != nil {
		test.Fatal("policy publication failed", err)
	}
	return generation
}

func docToolPolicyReviewCheck(test *testing.T, fixture *docToolPolicyFixture, generation DocDisplayProjectionGeneration, expected string, everyRune bool) {
	test.Helper()
	oracle, err := BuildDocToolDisplay(context.Background(), fixture.raw)
	if err != nil {
		test.Fatal("small source-span oracle unavailable", err)
	}
	if oracle.Text() != expected {
		test.Fatalf("local Node and small oracle differ: source=%s bytes=%d got=%q want=%q", HashContent(fixture.raw), len(fixture.raw), oracle.Text(), expected)
	}
	var output strings.Builder
	for index := 0; index < generation.Pages.Count; index++ {
		page, err := ReadDocDisplayProjectionPage(context.Background(), generation, index, fixture.load)
		if err != nil || page.From != output.Len() || page.ValidateProjection(generation.Source) != nil {
			test.Fatal("invalid policy display page", index, err)
		}
		for offset, character := range page.Text {
			if !everyRune && offset != 0 && offset%127 != 0 && offset+utf8.RuneLen(character) != len(page.Text) {
				continue
			}
			width := utf8.RuneLen(character)
			for _, length := range []int{width, min(len(page.Text)-offset, width+17)} {
				for length > width && offset+length < len(page.Text) && !utf8.RuneStart(page.Text[offset+length]) {
					length--
				}
				got, err := page.SourceRange(offset, length)
				want, failure := oracle.SourceRange(page.From+offset, length)
				if err != nil || failure != nil || got != want {
					test.Fatalf("policy source span changed: source=%s page=%d offset=%d length=%d got=%v want=%v read=%v oracle=%v", HashContent(fixture.raw), index, offset, length, got, want, err, failure)
				}
			}
		}
		output.WriteString(page.Text)
	}
	if output.String() != expected {
		test.Fatalf("actual policy differs from Node: source=%s bytes=%d got=%q want=%q", HashContent(fixture.raw), len(fixture.raw), output.String(), expected)
	}
	if generation.Pages.Count > 1 {
		loads := 0
		last, err := ReadDocDisplayProjectionPage(context.Background(), generation, generation.Pages.Count-1, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return fixture.load(ctx, hash)
		})
		if err != nil || last.Text == "" || loads > docRangeGenerationDepth(generation.Pages.Count)+1 {
			test.Fatal("late published page replayed a prefix", loads, err)
		}
	}
}

func docToolPolicyReviewPagedInput(name string) string {
	var fields []string
	for index := 0; index < 135; index++ {
		switch index {
		case 62:
			fields = append(fields, `"file_path":"stale/path"`)
		case 63:
			fields = append(fields, `"command":"stale command"`)
		case 90:
			fields = append(fields, `"pattern":"stale pattern","url":"stale url"`)
		case 126:
			fields = append(fields, `"old_string":"stale old"`)
		case 127:
			fields = append(fields, `"new_string":"stale new"`)
		case 128:
			fields = append(fields, `"content":"stale content"`)
		}
		fields = append(fields, fmt.Sprintf(`"f%03d":%d`, index, index))
	}
	fields = append(fields,
		`"\u0066ile_path":"/discard/a/b/\uD55C\uD83D\uDE42"`,
		`"\u0063ommand":"printf '\uAC00'\r\nlast"`,
		`"\u0070attern":"final pattern","\u0075rl":"https://local.invalid/final"`,
		`"\u006fld_string":"old\r\n\uD83D\uDE42\n"`,
		`"\u006eew_string":"new\n\uAC00\r"`,
		`"\u0063ontent":"written\n\uD83D\uDE42\n"`)
	return `{"kind":"tool_call","tool_name":"` + name + `","input":{` + strings.Join(fields, ",") + `}}`
}

func TestDocToolPolicyReviewEscapedDuplicateKeysAcrossRootPages(test *testing.T) {
	names := []string{"Edit", "Write", "Bash", "Read"}
	sources := make([]string, len(names))
	for index, name := range names {
		sources[index] = docToolPolicyReviewPagedInput(name)
	}
	expected := docToolPolicyReviewLocalNode(test, sources)
	for index, name := range names {
		test.Run(name, func(test *testing.T) {
			fixture := newDocToolPolicyFixture(test, sources[index])
			generation := docToolPolicyReviewFinish(test, fixture)
			docToolPolicyReviewCheck(test, fixture, generation, expected[index], true)
			pages := 0
			for _, previous := range fixture.steps {
				if previous.Phase != "tool_policy" {
					continue
				}
				state, err := decodeDocToolPolicyState(previous.Active, fixture.objects[previous.Active])
				if err != nil {
					test.Fatal(err)
				}
				pages = max(pages, state.Page)
			}
			if pages < 3 {
				test.Fatal("known-key selection did not cross three 64-entry pages", pages)
			}
			test.Logf("source=%s root_pages=%d saved_steps=%d displayed_bytes=%d", HashContent(fixture.raw), pages, len(fixture.steps), generation.Source.TextBytes)
		})
	}
}

func TestDocToolPolicyReviewNodeNumericAndNonstringTruth(test *testing.T) {
	values := []string{`-0`, `1e-9999`, `1e9999`, `-1e9999`, `5e-324`, `false`, `null`, `[]`, `{}`, `""`, `"\u0000"`}
	var sources, labels []string
	for _, name := range []string{"Edit", "Write", "Bash"} {
		for _, value := range values {
			field := `"old_string":` + value + `,"new_string":""`
			if name == "Write" {
				field = `"content":` + value
			} else if name == "Bash" {
				field = `"command":` + value + `,"pattern":"fallback"`
			}
			sources = append(sources, `{"kind":"tool_call","tool_name":"`+name+`","input":{`+field+`}}`)
			labels = append(labels, name+"/"+value)
		}
	}
	expected := docToolPolicyReviewLocalNode(test, sources)
	for index, label := range labels {
		test.Run(label, func(test *testing.T) {
			fixture := newDocToolPolicyFixture(test, sources[index])
			docToolPolicyReviewCheck(test, fixture, docToolPolicyReviewFinish(test, fixture), expected[index], true)
		})
	}
}

func docToolPolicyReviewLargeSources() []string {
	name := strings.Repeat(`\u004e`, 7000) + `\uD83D\uDE42`
	path := `/` + strings.Repeat(`\u0061`, 13000) + `/discard//\uD55C/last/`
	body := strings.Repeat(`x\uD83D\uDE42\n`, 900)
	deep := strings.Repeat(`{"nested":[`, 48) + `"deep\uD83D\uDE42"` + strings.Repeat(`]}`, 48)
	return []string{
		`{"kind":"tool_call","provider_tool_name":"` + name + `","tool_name":"Edit","input":{"file_path":"` + path + `","payload":"GENERIC_BODY_MARKER"}}`,
		`{"kind":"tool_call","tool_name":"Edit","input":{"file_path":"` + path + `","old_string":"` + body + `","new_string":"tail\r\n"}}`,
		`{"kind":"tool_call","tool_name":"Write","input":{"file_path":"` + path + `","content":"` + body + `"}}`,
		`{"kind":"tool_call","tool_name":"Bash","input":{"file_path":false,"command":"` + strings.Repeat(`echo \uAC00\n`, 900) + `"}}`,
		`{"kind":"tool_call","tool_name":"Deep","input":{"payload":` + deep + `,"text":"` + strings.Repeat(`\u0061`, 12000) + `"}}`,
		`{"kind":"tool_call","tool_name":"Bash","input":{"command":1e-` + strings.Repeat("0", DocToolNumberStepSourceBytes+31) + `9999,"pattern":"fallback"}}`,
	}
}

func TestDocToolPolicyReviewLargeHeadersDeepRestartAndNoRawAssembly(test *testing.T) {
	sources := docToolPolicyReviewLargeSources()
	names := []string{"large-name-path", "Edit", "Write", "Bash", "deep-value", "saved-number-underflow"}
	expected := docToolPolicyReviewLocalNode(test, sources)
	for index, name := range names {
		test.Run(name, func(test *testing.T) {
			fixture := newDocToolPolicyFixture(test, sources[index])
			generation := docToolPolicyReviewFinish(test, fixture)
			docToolPolicyReviewCheck(test, fixture, generation, expected[index], false)
			assembly, partialHeader, partialNumber := 0, false, false
			maxControl, maxPolicy := 0, 0
			for _, previous := range fixture.steps {
				raw, err := previous.Encode()
				if err != nil || len(raw) > MaxDocEventDisplayPreparationBytes {
					test.Fatal("saved outer control exceeded its bound", err)
				}
				maxControl = max(maxControl, len(raw))
				if previous.Phase == "tool_event_assembly" {
					assembly++
				}
				if previous.Phase != "tool_policy" {
					continue
				}
				body := fixture.objects[previous.Active]
				state, err := decodeDocToolPolicyState(previous.Active, body)
				if err != nil || bytes.Contains(body, []byte("GENERIC_BODY_MARKER")) || bytes.Contains(body, []byte(strings.Repeat("a", 100))) {
					test.Fatal("saved policy retained rendered text or failed to restore", err)
				}
				maxPolicy = max(maxPolicy, len(body))
				partialHeader = partialHeader || (state.Phase == "name" || state.Phase == "head") && state.Stats != nil && state.Stats.Bytes > 0 && state.Active != ""
				partialNumber = partialNumber || state.Phase == "truth" && state.Active != ""
			}
			if assembly == 0 || index < 3 && !partialHeader || name == "saved-number-underflow" && !partialNumber {
				test.Fatal("fixture missed its saved assembly/header/number checkpoints", assembly, partialHeader, partialNumber)
			}
			test.Logf("source=%s event_bytes=%d displayed_bytes=%d steps=%d pages=%d assembly_steps=%d max_control=%d max_policy=%d", HashContent(fixture.raw), len(fixture.raw), generation.Source.TextBytes, len(fixture.steps), generation.Pages.Count, assembly, maxControl, maxPolicy)
		})
	}
}

func docToolPolicyReviewRaw(fixture *docToolPolicyFixture, previous DocEventDisplayPreparation, calls *int) func(context.Context, int, int) ([]byte, error) {
	return func(_ context.Context, from, length int) ([]byte, error) {
		*calls = *calls + 1
		if previous.Phase == "tool_event_assembly" || from < 0 || length <= 0 || from > len(fixture.raw)-length {
			return nil, ErrIntegrity
		}
		return fixture.raw[from : from+length], nil
	}
}

func docToolPolicyReviewAssertPrior(test *testing.T, fixture *docToolPolicyFixture, previous DocEventDisplayPreparation, before []byte, aliases map[ContentHash][]byte) {
	test.Helper()
	after, err := previous.Encode()
	if err != nil || !bytes.Equal(before, after) || HashContent(fixture.raw) != previous.Source.EventHash {
		test.Fatal("failed policy operation changed predecessor aliases", err)
	}
	for hash, raw := range aliases {
		if HashContent(raw) != hash || !bytes.Equal(raw, fixture.objects[hash]) {
			test.Fatal("failed policy operation changed immutable object aliases", hash)
		}
	}
}

func TestDocToolPolicyReviewSavedFaultsAndCancellation(test *testing.T) {
	sources := docToolPolicyReviewLargeSources()
	for _, index := range []int{1, 4, 5} {
		fixture := newDocToolPolicyFixture(test, sources[index])
		docToolPolicyReviewFinish(test, fixture)
		selected := make(map[string]DocEventDisplayPreparation)
		for _, previous := range fixture.steps {
			label := previous.Phase
			if previous.Phase == "tool_policy" {
				state, err := decodeDocToolPolicyState(previous.Active, fixture.objects[previous.Active])
				if err != nil {
					test.Fatal(err)
				}
				label += "/" + state.Phase
				if state.Active != "" {
					label += "/active"
				}
			} else if previous.Phase != "tool_event_assembly" {
				continue
			}
			selected[label] = previous
		}
		if len(selected) > 14 || len(selected) < 7 {
			test.Fatal("unexpected bounded checkpoint sample", len(selected))
		}
		for label, previous := range selected {
			test.Run(fmt.Sprintf("%d/%s", index, label), func(test *testing.T) {
				before, err := previous.Encode()
				if err != nil {
					test.Fatal(err)
				}
				objectCalls, rawCalls := 0, 0
				probe := &outerReviewCancelContext{Context: context.Background()}
				baseline, err := StepDocEventDisplay(probe, previous, docToolPolicyReviewRaw(fixture, previous, &rawCalls), func(ctx context.Context, hash ContentHash) ([]byte, error) {
					objectCalls++
					return fixture.load(ctx, hash)
				}, fixture.tools)
				if err != nil || objectCalls == 0 || objectCalls > 256 || rawCalls > 32 {
					test.Fatal("saved baseline was unbounded or failed", objectCalls, rawCalls, err)
				}
				for _, mode := range []string{"object-fault", "object-cancel", "raw-fault", "raw-cancel", "final-cancel"} {
					if strings.HasPrefix(mode, "raw-") && rawCalls == 0 {
						continue
					}
					positions := []int{1, objectCalls}
					if strings.HasPrefix(mode, "raw-") {
						positions = []int{1, rawCalls}
					} else if mode == "final-cancel" {
						positions = []int{probe.checks - 1, probe.checks}
					}
					for _, failAt := range positions {
						test.Run(fmt.Sprintf("%s/%d", mode, failAt), func(test *testing.T) {
							base, cancel := context.WithCancel(context.Background())
							defer cancel()
							ctx := context.Context(base)
							if mode == "final-cancel" {
								ctx = &outerReviewCancelContext{Context: base, cancel: cancel, cancelAt: failAt}
							}
							fault := errors.New("saved policy injected fault")
							want := fault
							if strings.Contains(mode, "cancel") {
								want = context.Canceled
							}
							aliases := make(map[ContentHash][]byte)
							objects, reads := 0, 0
							step, err := StepDocEventDisplay(ctx, previous, func(ctx context.Context, from, length int) ([]byte, error) {
								body, err := docToolPolicyReviewRaw(fixture, previous, &reads)(ctx, from, length)
								if reads == failAt && strings.HasPrefix(mode, "raw-") {
									if mode == "raw-cancel" {
										cancel()
									} else {
										err = fault
									}
								}
								return body, err
							}, func(ctx context.Context, hash ContentHash) ([]byte, error) {
								if err := ctx.Err(); err != nil {
									return nil, err
								}
								objects++
								body, found := fixture.objects[hash]
								if !found {
									return nil, ErrIntegrity
								}
								aliases[hash] = body
								if objects == failAt && strings.HasPrefix(mode, "object-") {
									if mode == "object-cancel" {
										cancel()
									} else {
										return body, fault
									}
								}
								return body, nil
							}, fixture.tools)
							if !errors.Is(err, want) || !reflect.DeepEqual(step, DocEventDisplayStep{}) {
								test.Fatalf("saved failure leaked staged output: source=%s active=%s mode=%s fail_at=%d err=%v", HashContent(fixture.raw), previous.Active, mode, failAt, err)
							}
							docToolPolicyReviewAssertPrior(test, fixture, previous, before, aliases)
						})
					}
				}
				reads := 0
				retried, err := StepDocEventDisplay(context.Background(), previous, docToolPolicyReviewRaw(fixture, previous, &reads), fixture.load, fixture.tools)
				if err != nil || !reflect.DeepEqual(retried, baseline) {
					test.Fatal("saved checkpoint did not retry deterministically", err)
				}
			})
		}
	}
}

func docToolPolicyReviewSavedParts(test *testing.T, fixture *docToolPolicyFixture) (DocEventDisplayPreparation, docToolPolicyState) {
	test.Helper()
	for _, previous := range fixture.steps {
		if previous.Phase != "tool_policy" {
			continue
		}
		state, err := decodeDocToolPolicyState(previous.Active, fixture.objects[previous.Active])
		if err != nil {
			test.Fatal(err)
		}
		if state.Phase == "parts" && state.Part > 0 && state.Part < len(state.Parts) && state.Receipt != "" && state.Head != nil && state.Name != nil {
			return previous, state
		}
	}
	test.Fatal("no suitable saved policy part checkpoint")
	return DocEventDisplayPreparation{}, docToolPolicyState{}
}

func docToolPolicyReviewRejectOverlay(test *testing.T, fixture *docToolPolicyFixture, previous DocEventDisplayPreparation, overlay map[ContentHash][]byte) {
	test.Helper()
	before, err := previous.Encode()
	if err != nil {
		test.Fatal("mutated outer reference is not otherwise well formed", err)
	}
	aliases := make(map[ContentHash][]byte)
	reads := 0
	step, err := StepDocEventDisplay(context.Background(), previous, func(context.Context, int, int) ([]byte, error) {
		reads++
		return nil, ErrIntegrity
	}, func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if raw, found := overlay[hash]; found {
			if HashContent(raw) != hash {
				test.Fatal("test attempted an immutable-store overwrite")
			}
			return bytes.Clone(raw), nil
		}
		if raw, found := fixture.objects[hash]; found {
			aliases[hash] = raw
		}
		return fixture.load(ctx, hash)
	}, fixture.tools)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocEventDisplayStep{}) || reads != 0 {
		test.Fatal("transplanted control escaped its receipt/source binding", reads, err)
	}
	docToolPolicyReviewAssertPrior(test, fixture, previous, before, aliases)
}

func TestDocToolPolicyReviewCanonicalControlAndReceiptMutations(test *testing.T) {
	fixture := newDocToolPolicyFixture(test, `{"kind":"tool_call","tool_name":"Edit","input":{"file_path":"/a/b/c/d/e","old_string":"old\n","new_string":"new\uD83D\uDE42"}}`)
	docToolPolicyReviewFinish(test, fixture)
	previous, saved := docToolPolicyReviewSavedParts(test, fixture)
	for name, mutate := range map[string]func(*docToolPolicyState){
		"phase":          func(state *docToolPolicyState) { state.Phase = "layout" },
		"root-page":      func(state *docToolPolicyState) { state.Page = 0 },
		"truth":          func(state *docToolPolicyState) { state.Truth[0] = !state.Truth[0] },
		"truth-position": func(state *docToolPolicyState) { state.TruthAt = 0 },
		"part-position":  func(state *docToolPolicyState) { state.Part = 0 },
		"literal":        func(state *docToolPolicyState) { state.Parts[1].Text = ")" },
		"name-prefix":    func(state *docToolPolicyState) { state.Name.Prefix = []byte("Bash") },
		"head-slash":     func(state *docToolPolicyState) { state.Head.Last[0]-- },
		"head-space":     func(state *docToolPolicyState) { state.Head.NonSpace = !state.Head.NonSpace },
		"decoded-plan":   func(state *docToolPolicyState) { state.Parts[0].Plan = state.Head.Generation },
		"member":         func(state *docToolPolicyState) { state.Members[4] = state.Members[5] },
		"source-count":   func(state *docToolPolicyState) { state.Source.RootCount-- },
	} {
		test.Run(name, func(test *testing.T) {
			state, err := decodeDocToolPolicyState(previous.Active, fixture.objects[previous.Active])
			if err != nil {
				test.Fatal(err)
			}
			mutate(&state)
			raw, err := state.encode()
			if err != nil {
				test.Fatal("canonical mutation is not otherwise structurally valid", err)
			}
			changed := previous
			changed.Active = HashContent(raw)
			docToolPolicyReviewRejectOverlay(test, fixture, changed, map[ContentHash][]byte{changed.Active: raw})
		})
	}
	for name, raw := range map[string][]byte{
		"control-space":     append(bytes.Clone(fixture.objects[previous.Active]), ' '),
		"control-duplicate": append([]byte(`{"version":1,`), fixture.objects[previous.Active][1:]...),
		"control-unknown":   append([]byte(`{"unknown":true,`), fixture.objects[previous.Active][1:]...),
	} {
		test.Run(name, func(test *testing.T) {
			changed := previous
			changed.Active = HashContent(raw)
			docToolPolicyReviewRejectOverlay(test, fixture, changed, map[ContentHash][]byte{changed.Active: raw})
		})
	}
	var receipt docToolPolicyReceipt
	if json.Unmarshal(fixture.objects[saved.Receipt], &receipt) != nil {
		test.Fatal("invalid saved receipt")
	}
	for name, mutate := range map[string]func(*docToolPolicyReceipt){
		"receipt-version": func(receipt *docToolPolicyReceipt) { receipt.Version++ },
		"receipt-profile": func(receipt *docToolPolicyReceipt) { receipt.Profile = "foreign" },
		"receipt-source":  func(receipt *docToolPolicyReceipt) { receipt.Source = HashContent(nil) },
		"receipt-digest":  func(receipt *docToolPolicyReceipt) { receipt.Digest = HashContent(nil) },
	} {
		test.Run(name, func(test *testing.T) {
			changedReceipt := receipt
			mutate(&changedReceipt)
			raw := docToolPolicyReviewJSON(test, changedReceipt)
			state := saved
			state.Receipt = HashContent(raw)
			control, err := state.encode()
			if err != nil {
				test.Fatal(err)
			}
			changed := previous
			changed.Active = HashContent(control)
			docToolPolicyReviewRejectOverlay(test, fixture, changed, map[ContentHash][]byte{changed.Active: control, state.Receipt: raw})
		})
	}
}

func TestDocToolPolicyReviewCoherentForeignPartsAndPlans(test *testing.T) {
	left := newDocToolPolicyFixture(test, `{"kind":"tool_call","tool_name":"Write","input":{"file_path":"/a/b/c/d","content":"left\n"}}`)
	right := newDocToolPolicyFixture(test, `{"kind":"tool_call","tool_name":"Write","input":{"file_path":"/a/b/c/d","content":"else\n"}}`)
	docToolPolicyReviewFinish(test, left)
	docToolPolicyReviewFinish(test, right)
	leftPlan, err := decodeDocToolEventPlan(left.state.ToolEvent, left.objects[left.state.ToolEvent])
	if err != nil {
		test.Fatal(err)
	}
	rightPlan, err := decodeDocToolEventPlan(right.state.ToolEvent, right.objects[right.state.ToolEvent])
	if err != nil {
		test.Fatal(err)
	}
	if leftPlan.Source.Event.EventBytes != rightPlan.Source.Event.EventBytes || leftPlan.Source.Event.EventHash == rightPlan.Source.Event.EventHash {
		test.Fatal("foreign fixture does not preserve lengths while changing event identity")
	}
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if raw, found := right.objects[hash]; found {
			return bytes.Clone(raw), nil
		}
		return left.load(ctx, hash)
	}
	checked := 0
	for index, part := range rightPlan.Parts {
		if part.Kind == "literal" {
			continue
		}
		input, err := part.assembly(context.Background(), leftPlan.Source, load)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(input, DocDisplayAssemblyInput{}) {
			test.Fatal("coherent foreign child bypassed event identity", index, part.Kind, err)
		}
		checked++
	}
	if checked < 3 {
		test.Fatal("foreign fixture did not exercise decoded and diff plans", checked)
	}
	var assembly DocEventDisplayPreparation
	for _, previous := range left.steps {
		if previous.Phase == "tool_event_assembly" {
			assembly = previous
			break
		}
	}
	if assembly.Active == "" {
		test.Fatal("missing saved assembly state")
	}
	test.Run("foreign-plan-original-source", func(test *testing.T) {
		changed := assembly
		changed.Plan, changed.ToolEvent = right.state.ToolEvent, right.state.ToolEvent
		docToolPolicyReviewRejectOverlay(test, left, changed, right.objects)
	})
	test.Run("coherent-foreign-source-original-assembly", func(test *testing.T) {
		changed := right.state
		changed.Phase, changed.Generation, changed.Active = "tool_event_assembly", "", assembly.Active
		combined := make(map[ContentHash][]byte, len(right.objects)+1)
		for hash, raw := range right.objects {
			combined[hash] = raw
		}
		combined[assembly.Active] = left.objects[assembly.Active]
		docToolPolicyReviewRejectOverlay(test, right, changed, combined)
	})
	test.Run("same-event-parts-original-assembly", func(test *testing.T) {
		plan, err := decodeDocToolEventPlan(left.state.ToolEvent, left.objects[left.state.ToolEvent])
		if err != nil {
			test.Fatal(err)
		}
		plan.Parts[0], plan.Parts[3] = plan.Parts[3], plan.Parts[0]
		raw, err := plan.encode()
		if err != nil {
			test.Fatal(err)
		}
		changed := assembly
		changed.Plan, changed.ToolEvent = HashContent(raw), HashContent(raw)
		docToolPolicyReviewRejectOverlay(test, left, changed, map[ContentHash][]byte{changed.Plan: raw})
	})
}

func TestDocToolPolicyReviewInitialRootProofBeforeRaw(test *testing.T) {
	fixture := newDocToolPolicyFixture(test, `{"kind":"tool_call","tool_name":"Write","input":{"file_path":"/a/b/c/d","content":"line\n","decoy":{}}}`)
	docToolPolicyReviewFinish(test, fixture)
	var previous DocEventDisplayPreparation
	var initial docToolPolicyState
	for _, saved := range fixture.steps {
		if saved.Phase != "tool_policy" {
			continue
		}
		state, err := decodeDocToolPolicyState(saved.Active, fixture.objects[saved.Active])
		if err != nil {
			test.Fatal(err)
		}
		if state.Phase == "select" && state.Receipt == "" {
			previous, initial = saved, state
			break
		}
	}
	if previous.Active == "" || initial.Source.RootCount != 3 || initial.Source.RootPlan == "" {
		test.Fatal("missing initial root-bound policy checkpoint")
	}
	decoyFrom := bytes.Index(fixture.raw, []byte(`{}`))
	decoy, err := fixture.tools.OrderPlan(context.Background(), decoyFrom)
	if err != nil || decoy.Source.Count != 0 || decoy.Source.Parent.Depth != 1 {
		test.Fatal("decoy is not a genuine empty nested-container order plan", err)
	}
	decoyRaw, err := decoy.Encode(*initial.Source.Tree, *initial.Source.Keys)
	if err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"zero-root-count", "wrong-root-count", "nested-root-plan-zero-count"} {
		test.Run(name, func(test *testing.T) {
			state, err := decodeDocToolPolicyState(previous.Active, fixture.objects[previous.Active])
			if err != nil {
				test.Fatal(err)
			}
			overlay := make(map[ContentHash][]byte)
			switch name {
			case "zero-root-count":
				state.Source.RootCount = 0
			case "wrong-root-count":
				state.Source.RootCount++
			case "nested-root-plan-zero-count":
				state.Source.RootPlan, state.Source.RootCount = HashContent(decoyRaw), 0
				overlay[state.Source.RootPlan] = decoyRaw
			}
			if !state.Source.matches(previous) || state.Receipt != "" {
				test.Fatal("mutation did not preserve the previously insufficient initial source match")
			}
			raw, err := state.encode()
			if err != nil {
				test.Fatal("initial-root mutation is not otherwise canonical and well formed", err)
			}
			changed := previous
			changed.Active = HashContent(raw)
			overlay[changed.Active] = raw
			docToolPolicyReviewRejectOverlay(test, fixture, changed, overlay)
		})
	}
}

func TestDocToolPolicyReviewSameSourcePlanEvidenceAndPublication(test *testing.T) {
	fixture := newDocToolPolicyFixture(test, `{"kind":"tool_call","tool_name":"Write","input":{"file_path":"/a/b/c/d","content":"written\n\uD83D\uDE42"}}`)
	docToolPolicyReviewFinish(test, fixture)
	original, err := decodeDocToolEventPlan(fixture.state.ToolEvent, fixture.objects[fixture.state.ToolEvent])
	if err != nil || original.Evidence == "" {
		test.Fatal("final plan is not bound to completed policy evidence", err)
	}
	evidence, err := decodeDocToolPolicyState(original.Evidence, fixture.objects[original.Evidence])
	if err != nil || evidence.Phase != "complete" || evidence.Receipt == "" {
		test.Fatal("final evidence is not a genuine receipted completed policy state", err)
	}
	evidenceRaw := bytes.Clone(fixture.objects[original.Evidence])
	receiptRaw := bytes.Clone(fixture.objects[evidence.Receipt])
	for _, name := range []string{"empty-parts", "synthetic-anchor", "drop-body"} {
		test.Run(name, func(test *testing.T) {
			plan, err := decodeDocToolEventPlan(fixture.state.ToolEvent, fixture.objects[fixture.state.ToolEvent])
			if err != nil {
				test.Fatal(err)
			}
			switch name {
			case "empty-parts":
				plan.Parts = nil
			case "synthetic-anchor":
				changed := false
				for index := range plan.Parts {
					if plan.Parts[index].Kind == "literal" {
						plan.Parts[index].Token = plan.Source.Tree.Root
						changed = true
						break
					}
				}
				if !changed {
					test.Fatal("fixture has no synthetic punctuation anchor")
				}
			case "drop-body":
				last := len(plan.Parts) - 1
				if last < 1 || plan.Parts[last].Kind != "diff" || plan.Parts[last-1].Kind != "literal" {
					test.Fatal("fixture has no completed added-line body")
				}
				plan.Parts = plan.Parts[:last-1]
			}
			plan.TextBytes = 0
			for _, part := range plan.Parts {
				plan.TextBytes += part.TextBytes
			}
			if plan.Evidence != original.Evidence || !bytes.Equal(docToolPolicyReviewJSON(test, plan.Source), docToolPolicyReviewJSON(test, original.Source)) {
				test.Fatal("test changed trusted evidence or event source instead of only final output facts")
			}
			planRaw, err := plan.encode()
			if err != nil {
				test.Fatal("same-source mutated plan is not otherwise valid", err)
			}
			overlay := map[ContentHash][]byte{HashContent(planRaw): planRaw}
			store := func(raw []byte) ContentHash {
				hash := HashContent(raw)
				if prior, found := fixture.objects[hash]; found && !bytes.Equal(prior, raw) {
					test.Fatal("test attempted to overwrite a trusted immutable object")
				}
				if prior, found := overlay[hash]; found && !bytes.Equal(prior, raw) {
					test.Fatal("candidate immutable object changed")
				}
				overlay[hash] = bytes.Clone(raw)
				return hash
			}
			load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
				if raw, found := overlay[hash]; found {
					return bytes.Clone(raw), nil
				}
				return fixture.load(ctx, hash)
			}
			input, err := plan.assembly(context.Background(), load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(input, DocDisplayAssemblyInput{}) {
				test.Fatal("same-source final plan escaped completed-policy evidence", name, err)
			}
			children := make([]DocDisplayAssemblyInput, 0, len(plan.Parts))
			for _, part := range plan.Parts {
				child, err := part.assembly(context.Background(), plan.Source, load)
				if err != nil {
					test.Fatal("candidate child is not independently source-bound", err)
				}
				children = append(children, child)
			}
			source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Source.Event.EventHash, EventBytes: plan.Source.Event.EventBytes, Plan: HashContent(planRaw), TextBytes: plan.TextBytes}
			candidate, err := newDocCompositeAssemblyInput(source, children)
			if err != nil {
				test.Fatal("coherent candidate cannot be represented independently of evidence verification", err)
			}
			assembly, err := NewDocDisplayAssemblyState(candidate)
			if err != nil {
				test.Fatal(err)
			}
			assemblyRaw, err := assembly.Encode(candidate)
			if err != nil {
				test.Fatal(err)
			}
			changed := fixture.state
			changed.Plan, changed.ToolEvent = source.Plan, source.Plan
			changed.Phase, changed.Generation, changed.Active = "tool_event_assembly", "", store(assemblyRaw)
			docToolPolicyReviewRejectOverlay(test, fixture, changed, overlay)
			for count := 0; count < 64 && !assembly.Output.Done; count++ {
				step, err := StepDocDisplayAssembly(context.Background(), candidate, assembly, load)
				if err != nil {
					test.Fatal("coherent candidate cannot be rendered without policy authorization", err)
				}
				for _, object := range step.Objects {
					if store(object.Bytes) != object.Hash {
						test.Fatal("invalid candidate output hash")
					}
				}
				assembly = step.State
			}
			if !assembly.Output.Done {
				test.Fatal("small forged generation exceeded its fixed assembly budget")
			}
			generation, objects, err := FinishDocDisplayAssembly(context.Background(), candidate, assembly, load)
			if err != nil {
				test.Fatal(err)
			}
			for _, object := range objects {
				store(object.Bytes)
			}
			generationRaw, err := generation.Encode()
			if err != nil || generation.Source != source {
				test.Fatal("candidate generation envelope is not coherently source-bound", err)
			}
			changed.Phase, changed.Active, changed.Generation = "completed", "", store(generationRaw)
			before, err := changed.Encode()
			if err != nil {
				test.Fatal("forged publication envelope is not otherwise well formed", err)
			}
			published, err := ValidateDocEventDisplayPublication(context.Background(), changed, load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(published, DocDisplayProjectionGeneration{}) {
				test.Fatal("coherent forged generation was published despite unchanged evidence", name, err)
			}
			after, encodeErr := changed.Encode()
			if encodeErr != nil || !bytes.Equal(before, after) || !bytes.Equal(evidenceRaw, fixture.objects[original.Evidence]) || !bytes.Equal(receiptRaw, fixture.objects[evidence.Receipt]) {
				test.Fatal("negative evidence verification changed trusted aliases", encodeErr)
			}
		})
	}
}
