package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

type docToolJSONReviewFixture struct {
	Version int `json:"version"`
	Cases   []docToolJSONReviewCase
}

type docToolJSONReviewCase struct {
	Name, Domain, Source, Text string
	StoredText                 *string `json:"stored_text"`
	Ranges                     []struct {
		Text, Source string
		Occurrence   int
	}
}

func docToolJSONReviewCases(test testing.TB) []docToolJSONReviewCase {
	test.Helper()
	raw, err := os.ReadFile("../../../schemas/fixtures/tool-display-json-edge-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture docToolJSONReviewFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		test.Fatal(err)
	}
	if fixture.Version != DocToolDisplayVersion || len(fixture.Cases) == 0 {
		test.Fatal("invalid JSON edge review fixture", fixture.Version)
	}
	seen := make(map[string]bool)
	for _, scenario := range fixture.Cases {
		if scenario.Name == "" || seen[scenario.Name] || !json.Valid([]byte(scenario.Source)) {
			test.Fatal("invalid or repeated JSON edge case", scenario.Name)
		}
		seen[scenario.Name] = true
		switch scenario.Domain {
		case "canonical", "legacy_normalized", "typed_decode_rejected":
		default:
			test.Fatal("unknown admission domain", scenario.Domain)
		}
		for _, expected := range scenario.Ranges {
			if expected.Text == "" || expected.Source == "" || expected.Occurrence < 0 ||
				!strings.Contains(scenario.Text, expected.Text) || strings.Count(scenario.Source, expected.Source) <= expected.Occurrence {
				test.Fatal("invalid exact provenance expectation", scenario.Name, expected)
			}
		}
	}
	return fixture.Cases
}

func TestDocToolDisplayJSONReviewParity(test *testing.T) {
	for _, scenario := range docToolJSONReviewCases(test) {
		test.Run(scenario.Domain+"/"+scenario.Name, func(test *testing.T) {
			source := []byte(scenario.Source)
			before := bytes.Clone(source)
			display, err := BuildDocToolDisplay(context.Background(), source)
			if !bytes.Equal(source, before) {
				test.Fatal("projection changed original source bytes")
			}
			if err != nil || display == nil {
				test.Fatalf("BuildDocToolDisplay = %v, %v; local JSON.parse/stringify wants %q (%s)", display, err, scenario.Text, scenario.Domain)
			}
			if display.Text() != scenario.Text {
				test.Errorf("display = %q; local JSON.parse/stringify wants %q (%s)", display.Text(), scenario.Text, scenario.Domain)
			}
			assertDocToolDisplayRanges(test, source, display)
			for _, expected := range scenario.Ranges {
				offset := strings.Index(display.Text(), expected.Text)
				if offset < 0 || expected.Text == "" || expected.Source == "" {
					test.Errorf("missing display query or invalid provenance assertion: %q -> %q", expected.Text, expected.Source)
					continue
				}
				from := 0
				for occurrence := 0; occurrence <= expected.Occurrence; occurrence++ {
					relative := bytes.Index(source[from:], []byte(expected.Source))
					if relative < 0 {
						test.Fatalf("fixture source occurrence %d missing: %q", expected.Occurrence, expected.Source)
					}
					from += relative
					if occurrence < expected.Occurrence {
						from += len(expected.Source)
					}
				}
				want := DocFindSpan{From: from, To: from + len(expected.Source)}
				span, err := display.SourceRange(offset, len(expected.Text))
				if err != nil || span != want {
					test.Errorf("query %q provenance = %+v, %v; want exact original bytes %+v (%q)", expected.Text, span, err, want, expected.Source)
				}
			}
		})
	}
}

func TestDocToolDisplayJSONReviewStorageAdmission(test *testing.T) {
	ctx := context.Background()
	envelope := CIREnvelope{CIRVersion: "1"}
	envelopeJSON, err := canonicalJSON(envelope)
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range docToolJSONReviewCases(test) {
		test.Run(scenario.Domain+"/"+scenario.Name, func(test *testing.T) {
			raw := []byte(fmt.Sprintf(`{"envelope":%s,"events":[%s]}`, envelopeJSON, scenario.Source))
			wantExact := scenario.Domain == "canonical"
			var verifier CanonicalDocVerifier
			for attempt := 0; attempt < 2; attempt++ {
				verified, err := verifier.Verify(ctx, HashContent(raw), raw)
				if wantExact {
					if err != nil || !verified.Valid() || !bytes.Equal(verified.Bytes(), raw) {
						test.Fatalf("canonical admission attempt %d = %v; source must be accepted unchanged", attempt, err)
					}
				} else if !errors.Is(err, ErrIntegrity) || verified.Valid() {
					test.Fatalf("noncanonical raw event admitted under its own byte hash: %v", err)
				}
			}
			originalReference, err := VerifyStoredDocBytes(HashContent(raw), raw)
			if wantExact && (err != nil || !originalReference.Valid()) ||
				!wantExact && (!errors.Is(err, ErrIntegrity) || originalReference.Valid()) {
				test.Fatal("legacy verification with original byte hash disagrees with exact admission", err)
			}
			var event CIREvent
			err = json.Unmarshal([]byte(scenario.Source), &event)
			if scenario.Domain == "typed_decode_rejected" {
				if err == nil {
					test.Fatal("overflow unexpectedly survived typed CIR wire decoding")
				}
				var typeError *json.UnmarshalTypeError
				if !errors.As(err, &typeError) || !strings.HasPrefix(typeError.Value, "number ") {
					test.Fatal("wire input rejected for a reason other than float64 overflow", err)
				}
				return
			}
			if err != nil {
				test.Fatal("legacy-normalizable event failed typed decoding", err)
			}
			document := CIRDocument{Envelope: envelope, Events: []CIREvent{event}}
			canonical, err := CanonicalBytes(document)
			if err != nil {
				test.Fatal(err)
			}
			if bytes.Equal(raw, canonical) != wantExact {
				test.Fatalf("incorrect exact-versus-normalized classification; canonical event document = %s", canonical)
			}
			hash := HashContent(canonical)
			stored, err := VerifyStoredDocBytes(hash, raw)
			if err != nil || !stored.Valid() || stored.Hash() != hash {
				test.Fatal("legacy storage rejected bytes under their normalized content identity", err)
			}
			verified, err := VerifySessionDoc(SessionDoc{Hash: hash, CIR: document})
			if err != nil || !verified.Valid() || !bytes.Equal(verified.Bytes(), canonical) {
				test.Fatal("typed storage proof did not publish canonical bytes", err)
			}
			if proof, err := verifier.Verify(ctx, hash, canonical); err != nil || !proof.Valid() {
				test.Fatal("normalized event rejected by canonical storage admission", err)
			}
			normalizedEvent, err := canonicalJSON(event)
			if err != nil {
				test.Fatal(err)
			}
			stream, err := verified.EventStreamRange(ctx, 0, len(normalizedEvent))
			if err != nil || !bytes.Equal(stream, normalizedEvent) {
				test.Fatal("verified storage exposed original noncanonical event bytes", err)
			}
			if scenario.StoredText != nil {
				display, err := BuildDocToolDisplay(ctx, stream)
				if err != nil || display == nil || display.Text() != *scenario.StoredText {
					test.Fatalf("normalized storage display = %v, %v; want %q", display, err, *scenario.StoredText)
				}
			}
			test.Logf("exact=%v legacy-normalized=true; verified event=%s", wantExact, normalizedEvent)
		})
	}
}

func TestDocToolDisplayJSONReviewProgrammaticNumberAdmission(test *testing.T) {
	var nodeSources, nodeExpected []string
	for _, scenario := range []struct {
		name, token, text string
		overflow          bool
	}{
		{"positive_overflow", "1e400", "null", true},
		{"negative_overflow", "-1e400", "null", true},
		{"underflow", "1e-400", "0", false},
		{"unrounded_integer", "9007199254740993", "9007199254740992", false},
	} {
		for _, call := range []bool{false, true} {
			kind := EventToolResult
			if call {
				kind = EventToolCall
			}
			test.Run(string(kind)+"/"+scenario.name, func(test *testing.T) {
				event := CIREvent{Kind: kind, CallID: "c", Seq: 1, Output: []any{json.Number(scenario.token)}}
				wantText := "[\n  " + scenario.text + "\n]"
				if call {
					event.Output = nil
					event.ToolName = "generic"
					event.Input = map[string]any{"number": json.Number(scenario.token)}
					wantText = "generic\n{\n  \"number\": " + scenario.text + "\n}"
				}
				document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: []CIREvent{event}}
				raw, err := CanonicalBytes(document)
				if err != nil {
					test.Fatal("programmatic json.Number no longer accepted by canonical writer", err)
				}
				hash := HashContent(raw)
				verified, err := VerifySessionDoc(SessionDoc{Hash: hash, CIR: document})
				if !errors.Is(err, ErrIntegrity) || verified.Valid() {
					test.Fatal("programmatic noncanonical number admitted by typed storage API", err)
				}
				if validated, err := ValidatedSessionDocBytes(SessionDoc{Hash: hash, CIR: document}); !errors.Is(err, ErrIntegrity) || len(validated) != 0 {
					test.Fatal("programmatic noncanonical number admitted by validated byte writer", err)
				}
				if err := ValidateSessionDocHash(SessionDoc{Hash: hash, CIR: document}); !errors.Is(err, ErrIntegrity) {
					test.Fatal("programmatic noncanonical number admitted by hash validator", err)
				}
				eventJSON, err := canonicalJSON(event)
				if err != nil || !bytes.Contains(eventJSON, []byte(scenario.token)) {
					test.Fatal("programmatic numeric token was normalized away", err)
				}
				stream := eventJSON
				var verifier CanonicalDocVerifier
				if proof, err := verifier.Verify(context.Background(), hash, raw); !errors.Is(err, ErrIntegrity) || proof.Valid() {
					test.Fatal("wire verifier unexpectedly accepted non-float64-canonical numeric token", err)
				}
				if reference, err := VerifyStoredDocBytes(hash, raw); !errors.Is(err, ErrIntegrity) || reference.Valid() {
					test.Fatal("legacy typed decoder unexpectedly reproduced the programmatic hash", err)
				}
				var decoded CIREvent
				if err := json.Unmarshal(eventJSON, &decoded); (err != nil) != scenario.overflow {
					test.Fatal("typed wire number acceptance changed", err)
				}
				nodeSources = append(nodeSources, string(stream))
				nodeExpected = append(nodeExpected, wantText)
				display, err := BuildDocToolDisplay(context.Background(), stream)
				if err != nil || display == nil || display.Text() != wantText {
					test.Fatalf("programmatic number display = %v, %v; want JSON.parse/stringify parity %q", display, err, wantText)
				}
				assertDocToolDisplayRanges(test, stream, display)
				offset := strings.Index(display.Text(), scenario.text)
				from := bytes.Index(stream, []byte(scenario.token))
				span, err := display.SourceRange(offset, len(scenario.text))
				if err != nil || span != (DocFindSpan{From: from, To: from + len(scenario.token)}) {
					test.Fatal("normalized number lost whole-token provenance", span, err)
				}
				test.Logf("typed/wire/legacy publication rejects json.Number(%q) without changing its hash; raw display matches JSON.parse/stringify", scenario.token)
			})
		}
	}
	test.Run("local_node", func(test *testing.T) {
		rendered := docToolJSONReviewNode(test, nodeSources)
		for index, expected := range nodeExpected {
			if rendered[index] != expected {
				test.Errorf("programmatic source %s: frozen text = %q; local Node = %q", nodeSources[index], expected, rendered[index])
			}
		}
	})
}

func docToolJSONReviewNode(test testing.TB, sources []string) []string {
	test.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		test.Skip("optional local Node comparison: node is not installed; frozen expectations still run")
	}
	input, err := json.Marshal(sources)
	if err != nil {
		test.Fatal(err)
	}
	const script = `
const fs = require('node:fs');
const sources = JSON.parse(fs.readFileSync(0, 'utf8'));
const rendered = sources.map(source => {
  const event = JSON.parse(source);
  if (event.kind === 'tool_result') {
    const out = typeof event.output === 'string' ? event.output : event.output != null ? JSON.stringify(event.output, null, 2) : '';
    return out.trim() ? out : '';
  }
  if (event.kind !== 'tool_call') throw new Error('not a tool event');
  const name = event.provider_tool_name || event.tool_name || 'tool';
  const input = event.input ?? {};
  if (['Edit', 'Write', 'Bash'].includes(name) || [input.file_path, input.command, input.pattern, input.url].some(value => typeof value === 'string' && value)) {
    throw new Error('review oracle intentionally covers only generic calls without header arguments');
  }
  return name + (Object.keys(input).length ? '\n' + JSON.stringify(input, null, 2) : '');
});
process.stdout.write(JSON.stringify(rendered));
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-e", script)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		test.Fatalf("local Node oracle failed: %v; %s", err, stderr.String())
	}
	var rendered []string
	if err := json.Unmarshal(output, &rendered); err != nil || len(rendered) != len(sources) {
		test.Fatal("invalid local Node oracle result", err)
	}
	return rendered
}

func TestDocToolDisplayJSONReviewLocalNode(test *testing.T) {
	cases := docToolJSONReviewCases(test)
	var sources, normalized []string
	var normalizedNames []string
	for _, scenario := range cases {
		sources = append(sources, scenario.Source)
		var event CIREvent
		if err := json.Unmarshal([]byte(scenario.Source), &event); err == nil {
			raw, err := canonicalJSON(event)
			if err != nil {
				test.Fatal(err)
			}
			normalized = append(normalized, string(raw))
			normalizedNames = append(normalizedNames, scenario.Name)
		}
	}
	rendered := docToolJSONReviewNode(test, append(sources, normalized...))
	for index, scenario := range cases {
		test.Run("frozen_oracle/"+scenario.Name, func(test *testing.T) {
			if rendered[index] != scenario.Text {
				test.Errorf("frozen expectation = %q; local Node = %q", scenario.Text, rendered[index])
			}
		})
	}
	for index, source := range normalized {
		test.Run("normalized_storage/"+normalizedNames[index], func(test *testing.T) {
			display, err := BuildDocToolDisplay(context.Background(), []byte(source))
			if err != nil || display == nil || display.Text() != rendered[len(cases)+index] {
				test.Fatalf("canonical display = %v, %v; local Node = %q; source = %s", display, err, rendered[len(cases)+index], source)
			}
			assertDocToolDisplayRanges(test, []byte(source), display)
		})
	}
}

func TestDocToolDisplayJSONReviewFiniteNumberNode(test *testing.T) {
	var sources []string
	var names []string
	for exponent := -323; exponent <= 308; exponent++ {
		value := math.Pow10(exponent)
		for _, number := range []float64{math.Nextafter(value, 0), value, math.Nextafter(value, math.Inf(1))} {
			if number == 0 || math.IsInf(number, 0) {
				continue
			}
			raw, err := canonicalJSON(CIREvent{Kind: EventToolResult, CallID: "c", Seq: 1, Output: []any{number, -number}})
			if err != nil {
				test.Fatal(err)
			}
			sources = append(sources, string(raw))
			names = append(names, strconv.FormatFloat(number, 'g', -1, 64))
		}
	}
	rendered := docToolJSONReviewNode(test, sources)
	for index, source := range sources {
		display, err := BuildDocToolDisplay(context.Background(), []byte(source))
		if err != nil || display == nil || display.Text() != rendered[index] {
			test.Errorf("finite boundary %s: display = %v, %v; local Node = %q; source = %s", names[index], display, err, rendered[index], source)
		}
	}
}
