package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

type docDisplayStringTestReader struct {
	reader     io.Reader
	chunk      int
	maxRequest int
	readBytes  int
	onRead     func()
}

func (reader *docDisplayStringTestReader) Read(buffer []byte) (int, error) {
	reader.maxRequest = max(reader.maxRequest, len(buffer))
	if reader.chunk > 0 {
		buffer = buffer[:min(reader.chunk, len(buffer))]
	}
	count, err := reader.reader.Read(buffer)
	reader.readBytes += count
	if reader.onRead != nil {
		reader.onRead()
	}
	return count, err
}

type docDisplayStringTestErrorReader struct {
	data string
	err  error
}

func (reader *docDisplayStringTestErrorReader) Read(buffer []byte) (int, error) {
	count := copy(buffer, reader.data)
	reader.data = reader.data[count:]
	return count, reader.err
}

type docDisplayStringTestContext struct {
	context.Context
	remaining int
}

func (ctx *docDisplayStringTestContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

type docDisplayStringTestBuild struct {
	source     DocDisplayStringSource
	state      DocDisplayStringState
	generation DocDisplayStringGeneration
	pages      []DocDisplayPage
	objects    map[ContentHash][]byte
	steps      int
}

func docDisplayStringTestSource(test *testing.T, raw []byte) DocDisplayStringSource {
	test.Helper()
	source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal("source discovery:", err)
	}
	return source
}

func docDisplayStringTestCheckpoint(test *testing.T, source DocDisplayStringSource, state DocDisplayStringState) []byte {
	test.Helper()
	raw, err := state.Encode(source)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		test.Fatalf("checkpoint size=%d error=%v", len(raw), err)
	}
	return raw
}

func docDisplayStringTestStore(test *testing.T, stored map[ContentHash][]byte, objects []DocRangeObject) {
	test.Helper()
	for _, object := range objects {
		if len(object.Bytes) == 0 || len(object.Bytes) > max(MaxDocDisplayPageBytes, MaxDocRangeDirectoryBytes) || HashContent(object.Bytes) != object.Hash {
			test.Fatal("invalid staged object", object.Hash, len(object.Bytes))
		}
		if previous, exists := stored[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
			test.Fatal("different objects share a hash")
		}
		stored[object.Hash] = bytes.Clone(object.Bytes)
	}
}

func docDisplayStringTestBuildAll(test *testing.T, raw []byte, resume bool) docDisplayStringTestBuild {
	test.Helper()
	source := docDisplayStringTestSource(test, raw)
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	built := docDisplayStringTestBuild{source: source, objects: make(map[ContentHash][]byte)}
	for !state.Done {
		built.steps++
		before := docDisplayStringTestCheckpoint(test, source, state)
		generation, objects, err := FinishDocDisplayString(context.Background(), source, state)
		if !errors.Is(err, ErrIntegrity) || generation != (DocDisplayStringGeneration{}) || len(objects) != 0 {
			test.Fatal("unfinished staging published a descriptor", generation, len(objects), err)
		}
		calls, requested, readEnd := 0, 0, state.Next
		step, err := StepDocDisplayString(context.Background(), source, state, func(ctx context.Context, offset, length int) ([]byte, error) {
			calls++
			requested += length
			if ctx.Err() != nil || offset != readEnd || length <= 0 || length > 4<<10 || requested > DocDisplayStepSourceBytes || calls > 64 || offset < source.Token.From || offset+length > source.Token.To {
				test.Fatal("unbounded or out-of-source request", offset, length)
			}
			readEnd = offset + length
			return bytes.Clone(raw[offset : offset+length]), nil
		})
		if err != nil || calls < 1 || calls > 64 || requested > DocDisplayStepSourceBytes || step.ReadBytes != requested || step.State.Next <= state.Next || step.State.Next-state.Next > requested {
			test.Fatal("step failed its progress/read contract", calls, requested, step.ReadBytes, err)
		}
		if after := docDisplayStringTestCheckpoint(test, source, state); !bytes.Equal(before, after) {
			test.Fatal("step mutated the caller's checkpoint")
		}
		docDisplayStringTestStore(test, built.objects, step.Objects)
		if step.Page != nil {
			page := *step.Page
			if page.Index != len(built.pages) || page.From != state.TextBytes || page.Validate(source) != nil {
				test.Fatal("invalid or discontinuous page", page.Index, page.From)
			}
			from, to := state.Next, step.State.Next
			if state.Next == source.Token.From {
				from++
			}
			if step.State.Done {
				to--
			}
			if page.Runs[0].SourceFrom != from || page.Runs[len(page.Runs)-1].SourceTo != to || step.State.TextBytes != state.TextBytes+len(page.Text) || step.State.Directory.Count != state.Directory.Count+1 {
				test.Fatal("page/state provenance skipped or repeated source bytes", page.Index, from, to)
			}
			encoded, err := page.Encode(source)
			if err != nil || !bytes.Equal(built.objects[HashContent(encoded)], encoded) {
				test.Fatal("page was not included in staged objects", err)
			}
			docDisplayStringTestRanges(test, source, page, raw)
			built.pages = append(built.pages, page)
		} else if !step.State.Done || len(step.Objects) != 0 || step.State.TextBytes != state.TextBytes || step.State.Directory.Count != state.Directory.Count {
			test.Fatal("empty page advanced visible/staged content")
		}
		state = step.State
		checkpoint := docDisplayStringTestCheckpoint(test, source, state)
		if resume {
			decoded, err := DecodeDocDisplayStringState(source, HashContent(checkpoint), checkpoint)
			if err != nil || !reflect.DeepEqual(state, decoded) {
				test.Fatal("checkpoint did not survive serialization", err)
			}
			state = decoded
		}
	}
	checkpoint := docDisplayStringTestCheckpoint(test, source, state)
	generation, objects, err := FinishDocDisplayString(context.Background(), source, state)
	if err != nil || generation.Validate() != nil || state.Next != source.Token.To {
		test.Fatal("finish failed", generation, err)
	}
	if !bytes.Equal(checkpoint, docDisplayStringTestCheckpoint(test, source, state)) {
		test.Fatal("finish mutated the checkpoint")
	}
	docDisplayStringTestStore(test, built.objects, objects)
	built.state, built.generation = state, generation
	for index := 0; index < generation.Pages.Count; index++ {
		page, err := ReadDocDisplayPage(context.Background(), generation, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			raw, exists := built.objects[hash]
			if !exists {
				return nil, fmt.Errorf("missing staged object %s", hash)
			}
			return raw, nil
		})
		if err != nil || !reflect.DeepEqual(page, built.pages[index]) {
			test.Fatal("published page differs from staged page", index, err)
		}
	}
	return built
}

func docDisplayStringTestRanges(test *testing.T, source DocDisplayStringSource, page DocDisplayPage, raw []byte) {
	test.Helper()
	previous := DocFindSpan{}
	for offset, character := range page.Text {
		span, err := page.SourceRange(offset, utf8.RuneLen(character))
		if err != nil || span.From <= source.Token.From || span.To >= source.Token.To || span.From >= span.To || span.To > len(raw) || offset > 0 && previous.To != span.From {
			test.Fatal("rune provenance escaped or skipped source bytes", page.Index, offset, span, err)
		}
		encoded := raw[span.From:span.To]
		decoded := string(encoded)
		if encoded[0] == '\\' {
			if err := json.Unmarshal([]byte(`"`+string(encoded)+`"`), &decoded); err != nil {
				test.Fatal("source range is not an intact JSON escape", span, err)
			}
		}
		if decoded != string(character) {
			test.Fatalf("page=%d offset=%d source=%q decoded=%q want=%q", page.Index, offset, encoded, decoded, character)
		}
		previous = span
	}
	whole, err := page.SourceRange(0, len(page.Text))
	if err != nil || whole.From != page.Runs[0].SourceFrom || whole.To != previous.To {
		test.Fatal("whole-page provenance does not cover its runs", whole, err)
	}
	previousEnd := 0
	for _, run := range page.Runs {
		if previousEnd > 0 {
			_, width := utf8.DecodeLastRuneInString(page.Text[:previousEnd])
			_, nextWidth := utf8.DecodeRuneInString(page.Text[previousEnd:])
			left, _ := page.SourceRange(previousEnd-width, width)
			right, _ := page.SourceRange(previousEnd, nextWidth)
			crossing, err := page.SourceRange(previousEnd-width, width+nextWidth)
			if err != nil || crossing != (DocFindSpan{From: left.From, To: right.To}) {
				test.Fatal("cross-run provenance lost original bytes", crossing, err)
			}
		}
		previousEnd = run.To
	}
}

func TestDocDisplayStringSourceSelection(test *testing.T) {
	fixtures := []string{
		`{"kind":"tool_result","output":"public"}`,
		`{"output":"public","kind":"tool_result"}`,
		`{"kind":false,"kind":"tool_result","output":null,"output":"last"}`,
		`{"kind":"message","k\u0069nd":"tool\u005fresult","output":"first","out\u0070ut":"last"}`,
		`{"kind":"tool_result","output":{"nested":"ignored"},"output":""}`,
		`{"kind":"tool_result","output":"nonempty","out\u0070ut":" \t\u00a0"}`,
		`{"kind":"tool_result","output":" \n","output":"last"}`,
		`{"kind":"tool_result","output":" \n","locked":{"output":"nonempty"}}`,
		`{"kind":"tool_result","output":"","nested":[{"output":"nonempty"}]}`,
		`{"kind":"tool_result","locked":{"kind":"message","output":"secret"},"output":"public","nested":[{"kind":"tool_call","output":"secret"}]}`,
		`{"kind":"tool_result","output":"same","locked":{"output":"same"},"unknown":{"output":"same"}}`,
		`{"kind":"tool_result","output_extra":"secret","output\u0000":"secret","output":"public","locked":"opaque"}`,
		"{\"\\u006b\\u0069\\u006e\\u0064\":\"tool_result\",\"\\u006f\\u0075\\u0074\\u0070\\u0075\\u0074\":\"\ud55c\uae00🙂\"}",
		" \n\t{\"kind\" : \"tool_result\", \"output\" : \"line\\n\ub05d\", \"locked\" : {\"blob\":\"secret\"}}\r\n ",
	}
	for index, body := range fixtures {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			raw := []byte(body)
			decoder := json.NewDecoder(bytes.NewReader(raw))
			if _, err := decoder.Token(); err != nil {
				test.Fatal(err)
			}
			var want DocFindSpan
			var output json.RawMessage
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					test.Fatal(err)
				}
				var value json.RawMessage
				if err := decoder.Decode(&value); err != nil {
					test.Fatal(err)
				}
				if key == "output" {
					want = DocFindSpan{From: int(decoder.InputOffset()) - len(value), To: int(decoder.InputOffset())}
					output = value
				}
			}
			var decoded string
			if err := json.Unmarshal(output, &decoded); err != nil {
				test.Fatal(err)
			}
			wantEmpty := strings.TrimFunc(decoded, docRangePresentationSpace) == ""
			for _, chunk := range []int{1, 2, 7, 64 << 10} {
				reader := &docDisplayStringTestReader{reader: strings.NewReader(body), chunk: chunk}
				source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), reader)
				if err != nil || source.Validate() != nil || source.EventHash != HashContent(raw) || source.EventBytes != len(raw) || reader.readBytes != len(raw) || reader.maxRequest > 64<<10 {
					test.Fatal("source/read contract", chunk, source, reader.readBytes, reader.maxRequest, err)
				}
				if source.Token != want {
					test.Fatal("did not select the final root output", source.Token, want)
				}
				if source.Empty != wantEmpty {
					test.Fatal("empty proof did not describe the final root output", source.Empty, wantEmpty)
				}
				state, err := NewDocDisplayStringState(source)
				if err != nil || state.Done != wantEmpty || wantEmpty && state.Next != source.Token.To || state.TextBytes != 0 || state.Directory.Count != 0 || state.HasContent {
					test.Fatal("initial state did not skip exactly the empty sources", state, err)
				}
				encoded, err := source.Encode()
				if err != nil || len(encoded) > MaxDocDisplaySourceBytes {
					test.Fatal("unbounded source proof", len(encoded), err)
				}
			}
		})
	}
}

func TestDocDisplayStringSourceUnsupported(test *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"kind":"tool_result"}`,
		`{"kind":"tool_result","output":null}`,
		`{"kind":"tool_result","output":false}`,
		`{"kind":"tool_result","output":42}`,
		`{"kind":"tool_result","output":[]}`,
		`{"kind":"tool_result","output":{}}`,
		`{"kind":"tool_result","output":"old","out\u0070ut":null}`,
		`{"kind":"tool_result","output":"old","output":{"output":"nested"}}`,
		`{"kind":"tool_result","locked":{"output":"secret"}}`,
		`{"kind":"tool_result","nested":[{"output":"secret"}]}`,
		`{"kind":"tool_result","Output":"wrong case"}`,
		`{"kind":"tool_result","kind":false,"output":"old"}`,
		`{"kind":"tool_result","kind":null,"output":"old"}`,
		`{"kind":"tool_result","kind":"tool_call","output":"old"}`,
		`{"kind":"tool_call","output":"not supported"}`,
		`{"kind":"reasoning","output":"not supported","redacted_summary":"later work"}`,
		`{"kind":"message","output":"not supported","blocks":[{"type":"text","text":"later work"}]}`,
		`{"kind":"unknown","output":"not supported"}`,
		`{"Kind":"tool_result","output":"wrong case"}`,
		`{"nested":{"kind":"tool_result","output":"secret"}}`,
	} {
		test.Run(body, func(test *testing.T) {
			source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent([]byte(body)), len(body), strings.NewReader(body))
			if !errors.Is(err, ErrDocFindDisplayUnavailable) || source != (DocDisplayStringSource{}) {
				test.Fatal("unsupported source returned a proof", source, err)
			}
			source, err = BuildDocDisplayStringSourceReader(context.Background(), HashContent([]byte("wrong hash")), len(body), strings.NewReader(body))
			if !errors.Is(err, ErrIntegrity) || source != (DocDisplayStringSource{}) {
				test.Fatal("unsupported source skipped full-event verification", source, err)
			}
		})
	}
}

func TestDocDisplayStringSourceIntegrityAndIO(test *testing.T) {
	for _, body := range []string{
		"", `null`, `[]`, `"tool_result"`, `{`,
		`{"kind":"tool_result","output":"x"} {}`,
		`{"kind":"tool_result","output":"x",}`,
		`{"kind":"tool_result","output":"\q"}`,
		`{"kind":"tool_result","output":"\u12xz"}`,
		`{"kind":"tool_result","output":"\u123"}`,
		`{"kind":"tool_result","output":"unterminated}`,
		`{"kind":"tool_result","output":"ok","locked":[1,]}`,
		`{"kind":"tool_result","output":"ok","locked":01}`,
		`{"kind":"tool_result","output":"ok","locked":1e}`,
		"{\"kind\":\"tool_result\",\"output\":\"literal\nnewline\"}",
		"{\"kind\":\"tool_result\",\"output\":\"\xff\"}",
		"{\"kind\":\"tool_result\",\"output\":\"ok\",\"locked\":\"\xc0\x80\"}",
	} {
		test.Run(fmt.Sprintf("malformed-%x", body), func(test *testing.T) {
			source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent([]byte(body)), len(body), strings.NewReader(body))
			if !errors.Is(err, ErrIntegrity) || source != (DocDisplayStringSource{}) {
				test.Fatal("malformed JSON returned a partial proof", source, err)
			}
		})
	}
	body := `{"kind":"tool_result","output":"public","locked":{"blob":"secret"}} `
	for _, scenario := range []struct {
		name   string
		hash   ContentHash
		length int
		reader io.Reader
	}{
		{"hash", HashContent([]byte("other")), len(body), strings.NewReader(body)},
		{"invalid-hash", "bad", len(body), strings.NewReader(body)},
		{"locked-suffix-hash", HashContent([]byte(body)), len(body), strings.NewReader(strings.ReplaceAll(body, "secret", "SECRET"))},
		{"trailing-whitespace-hash", HashContent([]byte(body)), len(body), strings.NewReader(body[:len(body)-1] + "\n")},
		{"negative-size", HashContent([]byte(body)), -1, strings.NewReader(body)},
		{"zero-size", HashContent([]byte(body)), 0, strings.NewReader(body)},
		{"short-size", HashContent([]byte(body)), len(body) - 1, strings.NewReader(body)},
		{"long-size", HashContent([]byte(body)), len(body) + 1, strings.NewReader(body)},
		{"too-large", HashContent([]byte(body)), MaxConversationDocumentBytes + 1, strings.NewReader(body)},
		{"integer-limit", HashContent([]byte(body)), int(^uint(0) >> 1), strings.NewReader(body)},
		{"nil-reader", HashContent([]byte(body)), len(body), nil},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			source, err := BuildDocDisplayStringSourceReader(context.Background(), scenario.hash, scenario.length, scenario.reader)
			if !errors.Is(err, ErrIntegrity) || source != (DocDisplayStringSource{}) {
				test.Fatal("unverified bytes returned a proof", source, err)
			}
		})
	}
	for _, readError := range []error{io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe} {
		for _, data := range []string{"", body[:len(body)/2], body} {
			source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent([]byte(body)), len(body), &docDisplayStringTestErrorReader{data: data, err: readError})
			if readError == io.EOF && data == body {
				if err != nil || source.Validate() != nil {
					test.Fatal("complete data with EOF was rejected", err)
				}
				continue
			}
			want := readError
			if readError == io.EOF || readError == io.ErrUnexpectedEOF {
				want = ErrIntegrity
			}
			if !errors.Is(err, want) || source != (DocDisplayStringSource{}) {
				test.Fatal("reader error returned a proof", len(data), readError, source, err)
			}
		}
	}
}

func TestDocDisplayStringMatchesJSONAndToolDisplay(test *testing.T) {
	for _, token := range []string{
		`""`, `"plain"`, "\" \ud55c\uae00🙂é\\nlast \"",
		`"quote\" slash\/ backslash\\ controls\b\f\n\r\t\u0000\u001f"`,
		`"\u0041\u00e9\uAC00\u2028\u2029\ufeff\ud83d\ude42"`,
		`"\ud800"`, `"\udfff"`, `"\ud800x"`, `"\ud800\u0061"`,
		`"\ud800\ud800\udc00\udc00"`, `"\uD83D\uDE42\ufffd"`,
		`" \t\n\r\f\u000b\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"`,
		`"\u0085"`, `"\u180e"`, `"\u200b"`, `" \nvisible\n "`,
	} {
		test.Run(token, func(test *testing.T) {
			raw := []byte(`{"locked":{"output":"hidden"},"kind":"tool_result","output":` + token + `,"metadata":"hidden"}`)
			built := docDisplayStringTestBuildAll(test, raw, true)
			var decoded string
			if err := json.Unmarshal([]byte(token), &decoded); err != nil {
				test.Fatal(err)
			}
			old, err := BuildDocToolDisplay(context.Background(), raw)
			if err != nil {
				test.Fatal("old display rejected fixture", err)
			}
			want := decoded
			if strings.TrimFunc(want, docRangePresentationSpace) == "" {
				want = ""
			}
			var text strings.Builder
			for _, page := range built.pages[:built.generation.Pages.Count] {
				text.WriteString(page.Text)
				for offset, character := range page.Text {
					span, err := page.SourceRange(offset, utf8.RuneLen(character))
					oldSpan, oldErr := old.SourceRange(page.From+offset, utf8.RuneLen(character))
					if err != nil || oldErr != nil || span != oldSpan {
						test.Fatal("old/new source mapping mismatch", span, oldSpan, err, oldErr)
					}
				}
			}
			if text.String() != want || old.Text() != want || built.generation.TextBytes != len(want) {
				test.Fatalf("stream=%q Go=%q old=%q", text.String(), want, old.Text())
			}
			if want == "" && (built.generation.Pages != (DocRangeDirectoryRoot{}) || built.generation.TextBytes != 0 || built.steps != 0 || len(built.objects) != 0 || len(built.pages) != 0) {
				test.Fatal("empty source performed staging or visible work")
			}
		})
	}
}

func TestDocDisplayStringUnicodeAtReadAndPageBoundaries(test *testing.T) {
	for _, boundary := range []int{DocDisplayPageTextBytes, DocDisplayStepSourceBytes} {
		for _, encoded := range []string{`\n`, `\uAC00`, `\ud83d\ude42`, `\ud800\u0061`, "\ud55c", "🙂"} {
			for split := 1; split < len(encoded); split++ {
				test.Run(fmt.Sprintf("boundary-%d/%x/split-%d", boundary, encoded, split), func(test *testing.T) {
					prefix := `{"locked":"` + strings.Repeat("z", 31) + `","kind":"tool_result","output":"`
					body := prefix + strings.Repeat("a", boundary-split-1) + encoded + "\ub05d\\u0000\\udc00\",\"locked\":{\"blob\":\"not displayed\"}}"
					raw := []byte(body)
					reader := &docDisplayStringTestReader{reader: strings.NewReader(body), chunk: 64 << 10}
					source, err := BuildDocDisplayStringSourceReader(context.Background(), HashContent(raw), len(raw), reader)
					if err != nil || source.Token.From != len(prefix)-1 {
						test.Fatal("split token discovery failed", source, err)
					}
					built := docDisplayStringTestBuildAll(test, raw, true)
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						test.Fatal(err)
					}
					var want string
					if err := json.Unmarshal(fields["output"], &want); err != nil {
						test.Fatal(err)
					}
					var text strings.Builder
					for _, page := range built.pages {
						text.WriteString(page.Text)
					}
					old, err := BuildDocToolDisplay(context.Background(), raw)
					if err != nil || text.String() != want || old.Text() != want || !utf8.ValidString(text.String()) {
						test.Fatal("split rune/escape was lost or duplicated", len(text.String()), len(want), err)
					}
				})
			}
		}
	}
	for split := 1; split < 12; split++ {
		test.Run(fmt.Sprintf("source-prepass-64KiB-%d", split), func(test *testing.T) {
			prefix := `{"kind":"tool_result","locked":"`
			body := prefix + strings.Repeat("x", (64<<10)-split-len(prefix)) + `\ud83d\ude42","output":"public🙂"}`
			raw := []byte(body)
			source := docDisplayStringTestSource(test, raw)
			if string(raw[source.Token.From:source.Token.To]) != `"public🙂"` {
				test.Fatal("locked split escape contaminated output")
			}
		})
	}
}

func TestDocDisplayStringResumeAcrossDirectoryAndRunLimits(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat(`\u0061`, DocDisplayPageRuns*(DocRangeDirectoryFanout+1)+1) + `"}`)
	resumed := docDisplayStringTestBuildAll(test, raw, true)
	uninterrupted := docDisplayStringTestBuildAll(test, raw, false)
	if !reflect.DeepEqual(resumed, uninterrupted) || len(resumed.pages) != DocRangeDirectoryFanout+2 {
		test.Fatal("resumption changed the content-addressed generation", len(resumed.pages), len(uninterrupted.pages))
	}
	for index, page := range resumed.pages {
		if index < len(resumed.pages)-1 && (len(page.Runs) != DocDisplayPageRuns || len(page.Text) != DocDisplayPageRuns) {
			test.Fatal("escape pages did not exercise the source-run limit", index, len(page.Text), len(page.Runs))
		}
	}
}

func TestDocDisplayStringEmptySourceSkipsAllStaging(test *testing.T) {
	for _, tail := range []string{"", `\u0061`} {
		raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat(`\u0020`, DocDisplayPageRuns*3) + tail + `"}`)
		built := docDisplayStringTestBuildAll(test, raw, true)
		if tail == "" {
			if built.steps != 0 || len(built.pages) != 0 || len(built.objects) != 0 || built.generation.Pages.Count != 0 || built.generation.TextBytes != 0 || built.state.HasContent || !built.state.Done || built.state.Next != built.source.Token.To || built.state.Directory.Count != 0 {
				test.Fatal("whitespace-only output created staged/visible work")
			}
		} else if len(built.pages) < 3 || len(built.objects) < 3 || built.generation.Pages.Count != len(built.pages) || built.generation.TextBytes != DocDisplayPageRuns*3+1 || !built.state.HasContent {
			test.Fatal("late non-whitespace discarded the leading whitespace pages")
		}
	}
}

func TestDocDisplayStringCheckpointRejectsNewSourceAndCorruption(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat("a", DocDisplayPageTextBytes+10) + `"}`)
	source := docDisplayStringTestSource(test, raw)
	initial, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := StepDocDisplayString(context.Background(), source, initial, func(_ context.Context, offset, length int) ([]byte, error) {
		return bytes.Clone(raw[offset : offset+length]), nil
	})
	if err != nil || step.State.Done {
		test.Fatal("fixture did not yield an unfinished checkpoint", err)
	}
	checkpoint := docDisplayStringTestCheckpoint(test, source, step.State)
	changed := bytes.Clone(raw)
	changed[source.Token.From+1] = 'b'
	newSource := docDisplayStringTestSource(test, changed)
	decoded, err := DecodeDocDisplayStringState(newSource, HashContent(checkpoint), checkpoint)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayStringState{}) {
		test.Fatal("checkpoint accepted a new source", decoded, err)
	}
	calls := 0
	result, err := StepDocDisplayString(context.Background(), newSource, step.State, func(context.Context, int, int) ([]byte, error) {
		calls++
		return nil, nil
	})
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocDisplayStringStep{}) || calls != 0 {
		test.Fatal("cross-source step did IO or returned partial data", calls, err)
	}
	for name, mutate := range map[string]func(*DocDisplayStringState){
		"version":      func(state *DocDisplayStringState) { state.Version++ },
		"source":       func(state *DocDisplayStringState) { state.Source = HashContent([]byte("other")) },
		"before-token": func(state *DocDisplayStringState) { state.Next = source.Token.From - 1 },
		"after-token":  func(state *DocDisplayStringState) { state.Next = source.Token.To + 1 },
		"early-done":   func(state *DocDisplayStringState) { state.Done = true },
		"negative":     func(state *DocDisplayStringState) { state.TextBytes = -1 },
		"directory":    func(state *DocDisplayStringState) { state.Directory.Count++ },
	} {
		test.Run(name, func(test *testing.T) {
			var state DocDisplayStringState
			if err := json.Unmarshal(checkpoint, &state); err != nil {
				test.Fatal(err)
			}
			mutate(&state)
			encoded, err := canonicalJSON(state)
			if err != nil {
				test.Fatal(err)
			}
			decoded, err := DecodeDocDisplayStringState(source, HashContent(encoded), encoded)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayStringState{}) {
				test.Fatal("self-hashed invalid checkpoint accepted", decoded, err)
			}
		})
	}
	for _, malformed := range [][]byte{
		nil, checkpoint[:len(checkpoint)-1], append(bytes.Clone(checkpoint), ' '),
		append([]byte(`{"unknown":true,`), checkpoint[1:]...),
		bytes.Repeat([]byte(" "), MaxDocDisplayStateBytes+1),
	} {
		decoded, err := DecodeDocDisplayStringState(source, HashContent(malformed), malformed)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayStringState{}) {
			test.Fatal("noncanonical checkpoint accepted", len(malformed), err)
		}
	}
	decoded, err = DecodeDocDisplayStringState(source, HashContent([]byte("wrong")), checkpoint)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayStringState{}) {
		test.Fatal("checkpoint hash was not authenticated", err)
	}
}

func TestDocDisplayStringFailuresReturnZeroPartial(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat("a", DocDisplayPageTextBytes+20) + `"}`)
	source := docDisplayStringTestSource(test, raw)
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	before := docDisplayStringTestCheckpoint(test, source, state)
	for _, scenario := range []struct {
		name string
		load func(context.Context, int, int) ([]byte, error)
		want error
	}{
		{"nil-loader", nil, ErrIntegrity},
		{"loader-error", func(context.Context, int, int) ([]byte, error) { return []byte(`"partial`), io.ErrClosedPipe }, io.ErrClosedPipe},
		{"empty-read", func(context.Context, int, int) ([]byte, error) { return nil, nil }, ErrIntegrity},
		{"short-read", func(_ context.Context, offset, length int) ([]byte, error) { return raw[offset : offset+length-1], nil }, ErrIntegrity},
		{"long-read", func(_ context.Context, offset, length int) ([]byte, error) { return make([]byte, length+1), nil }, ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			step, err := StepDocDisplayString(context.Background(), source, state, scenario.load)
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(step, DocDisplayStringStep{}) {
				test.Fatal("failed step leaked partial state/text/objects", step, err)
			}
			if !bytes.Equal(before, docDisplayStringTestCheckpoint(test, source, state)) {
				test.Fatal("failed step mutated its input checkpoint")
			}
		})
	}
	built := docDisplayStringTestBuildAll(test, []byte(`{"kind":"tool_result","output":"done"}`), true)
	step, err := StepDocDisplayString(context.Background(), built.source, built.state, func(context.Context, int, int) ([]byte, error) {
		test.Fatal("completed state performed IO")
		return nil, nil
	})
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocDisplayStringStep{}) {
		test.Fatal("completed state accepted another step", err)
	}
}

func TestDocDisplayStringCancellation(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"` + strings.Repeat("a", DocDisplayStepSourceBytes*2) + `"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &docDisplayStringTestReader{reader: bytes.NewReader(raw)}
	source, err := BuildDocDisplayStringSourceReader(ctx, HashContent(raw), len(raw), reader)
	if !errors.Is(err, context.Canceled) || source != (DocDisplayStringSource{}) || reader.readBytes != 0 {
		test.Fatal("pre-canceled discovery read/published data", reader.readBytes, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reader = &docDisplayStringTestReader{reader: bytes.NewReader(raw), chunk: 7, onRead: cancel}
	source, err = BuildDocDisplayStringSourceReader(ctx, HashContent(raw), len(raw), reader)
	if !errors.Is(err, context.Canceled) || source != (DocDisplayStringSource{}) {
		test.Fatal("mid-discovery cancellation returned a proof", err)
	}
	source = docDisplayStringTestSource(test, raw)
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	before := docDisplayStringTestCheckpoint(test, source, state)
	for _, remaining := range []int{1, 3, 30, 1000} {
		ctx := &docDisplayStringTestContext{Context: context.Background(), remaining: remaining}
		step, err := StepDocDisplayString(ctx, source, state, func(_ context.Context, offset, length int) ([]byte, error) {
			return bytes.Clone(raw[offset : offset+length]), nil
		})
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocDisplayStringStep{}) || !bytes.Equal(before, docDisplayStringTestCheckpoint(test, source, state)) {
			test.Fatal("canceled step leaked or mutated progress", remaining, err)
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	step, err := StepDocDisplayString(ctx, source, state, func(_ context.Context, offset, length int) ([]byte, error) {
		cancel()
		return bytes.Clone(raw[offset : offset+length]), nil
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocDisplayStringStep{}) {
		test.Fatal("cancellation during load leaked progress", err)
	}
	built := docDisplayStringTestBuildAll(test, []byte(`{"kind":"tool_result","output":"done"}`), true)
	generation, objects, err := FinishDocDisplayString(ctx, built.source, built.state)
	if !errors.Is(err, context.Canceled) || generation != (DocDisplayStringGeneration{}) || len(objects) != 0 {
		test.Fatal("canceled finish published a descriptor", err)
	}
}

func TestDocDisplayStringPageRangeRejectsNonRuneBoundaries(test *testing.T) {
	built := docDisplayStringTestBuildAll(test, []byte("{\"kind\":\"tool_result\",\"output\":\"\ud55c🙂\\n\ub05d\"}"), true)
	page := built.pages[0]
	for _, interval := range [][2]int{{-1, 1}, {0, 0}, {0, -1}, {len(page.Text), 1}, {0, len(page.Text) + 1}, {1, 1}, {0, 1}, {3, 1}, {4, 3}, {int(^uint(0) >> 1), 1}, {1, int(^uint(0) >> 1)}} {
		span, err := page.SourceRange(interval[0], interval[1])
		if !errors.Is(err, ErrValidation) || span != (DocFindSpan{}) {
			test.Fatal("invalid UTF8 interval returned provenance", interval, span, err)
		}
	}
}

type docDisplayStringTestGeneratedEvent struct {
	prefix string
	count  int
	suffix string
}

func (event docDisplayStringTestGeneratedEvent) size() int {
	return len(event.prefix) + event.count + len(event.suffix)
}

func (event docDisplayStringTestGeneratedEvent) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, ErrValidation
	}
	count := min(len(buffer), max(0, event.size()-int(offset)))
	for position := range count {
		absolute := int(offset) + position
		switch {
		case absolute < len(event.prefix):
			buffer[position] = event.prefix[absolute]
		case absolute < len(event.prefix)+event.count:
			buffer[position] = 'x'
		default:
			buffer[position] = event.suffix[absolute-len(event.prefix)-event.count]
		}
	}
	if count != len(buffer) {
		return count, io.EOF
	}
	return count, nil
}

func (event docDisplayStringTestGeneratedEvent) reader() io.Reader {
	return io.NewSectionReader(event, 0, int64(event.size()))
}

func (event docDisplayStringTestGeneratedEvent) hash(test *testing.T) ContentHash {
	test.Helper()
	digest := sha256.New()
	count, err := io.Copy(digest, event.reader())
	if err != nil || count != int64(event.size()) {
		test.Fatal("generated source hash", count, err)
	}
	return ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
}

func TestDocDisplayStringSourceTenfoldOneStepBounds(test *testing.T) {
	var smallAllocated, smallRetained uint64
	var smallRead, smallProgress, smallCheckpoint int
	var smallRequests [][2]int
	for _, payload := range []int{1 << 20, 10 << 20} {
		event := docDisplayStringTestGeneratedEvent{prefix: `{"kind":"tool_result","output":"`, count: payload, suffix: `","locked":{"blob":"not displayed"}}`}
		hash := event.hash(test)
		reader := &docDisplayStringTestReader{reader: event.reader()}
		runtime.GC()
		var before, after, retained runtime.MemStats
		runtime.ReadMemStats(&before)
		source, err := BuildDocDisplayStringSourceReader(context.Background(), hash, event.size(), reader)
		if err != nil || source.Token != (DocFindSpan{From: len(event.prefix) - 1, To: len(event.prefix) + payload + 1}) || reader.readBytes != event.size() || reader.maxRequest > 64<<10 {
			test.Fatal("generated prepass lost bounds", source, reader.readBytes, reader.maxRequest, err)
		}
		state, err := NewDocDisplayStringState(source)
		if err != nil {
			test.Fatal(err)
		}
		calls, requested, maxRequest := 0, 0, 0
		var requests [][2]int
		step, err := StepDocDisplayString(context.Background(), source, state, func(_ context.Context, offset, length int) ([]byte, error) {
			calls++
			requested += length
			maxRequest = max(maxRequest, length)
			requests = append(requests, [2]int{offset - source.Token.From, length})
			if length <= 0 || length > 4<<10 || offset < source.Token.From || offset+length > source.Token.To {
				test.Fatal("step exceeded adaptive refill/source bounds", offset, length)
			}
			raw := make([]byte, length)
			_, err := event.ReadAt(raw, int64(offset))
			return raw, err
		})
		checkpoint := docDisplayStringTestCheckpoint(test, source, step.State)
		proof, proofErr := source.Encode()
		runtime.ReadMemStats(&after)
		runtime.GC()
		runtime.ReadMemStats(&retained)
		runtime.KeepAlive(source)
		runtime.KeepAlive(step)
		runtime.KeepAlive(checkpoint)
		runtime.KeepAlive(proof)
		allocated := after.TotalAlloc - before.TotalAlloc
		live := uint64(0)
		if retained.HeapAlloc > before.HeapAlloc {
			live = retained.HeapAlloc - before.HeapAlloc
		}
		if err != nil || proofErr != nil || calls < 1 || calls > 64 || requested <= 0 || requested > DocDisplayStepSourceBytes || maxRequest > 4<<10 || step.ReadBytes != requested || step.Page == nil || len(step.Page.Text) != DocDisplayPageTextBytes || len(step.Page.Runs) != 1 || step.State.Done {
			test.Fatal("one step scaled with event size", calls, requested, maxRequest, step.ReadBytes, err, proofErr)
		}
		if len(checkpoint) > MaxDocDisplayStateBytes || len(proof) > MaxDocDisplaySourceBytes || allocated > 2<<20 || live > 1<<20 {
			test.Fatal("unbounded allocation/retention", payload, allocated, live, len(checkpoint), len(proof))
		}
		if payload == 1<<20 {
			smallAllocated, smallRetained = allocated, live
			smallRead, smallProgress, smallCheckpoint = step.ReadBytes, step.State.Next-source.Token.From, len(checkpoint)
			smallRequests = requests
		} else if allocated > smallAllocated+256<<10 || live > smallRetained+256<<10 || step.ReadBytes != smallRead || step.State.Next-source.Token.From != smallProgress || len(checkpoint) > smallCheckpoint+32 || !reflect.DeepEqual(requests, smallRequests) {
			test.Fatal("10x source increased one-step work/retention", smallAllocated, allocated, smallRetained, live)
		}
		test.Logf("full-prepass source=%d read=%d; one-step read=%d calls=%d max-request=%d allocation=%d retained=%d checkpoint=%d", event.size(), reader.readBytes, step.ReadBytes, calls, maxRequest, allocated, live, len(checkpoint))
	}
}

func TestDocDisplayStringDenseEscapeReadAmplification(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":"x` + strings.Repeat(`\n`, DocDisplayPageRuns*(DocRangeDirectoryFanout+1)) + `y"}`)
	source := docDisplayStringTestSource(test, raw)
	state, err := NewDocDisplayStringState(source)
	if err != nil {
		test.Fatal(err)
	}
	var text strings.Builder
	physicalTotal, consumedTotal, steps := 0, 0, 0
	for !state.Done {
		before := docDisplayStringTestCheckpoint(test, source, state)
		calls, physical, maxRequest := 0, 0, 0
		step, err := StepDocDisplayString(context.Background(), source, state, func(_ context.Context, offset, length int) ([]byte, error) {
			if length <= 0 || offset < state.Next || offset+length > source.Token.To {
				test.Fatal("dense escape load escaped the remaining source", offset, length)
			}
			calls++
			physical += length
			maxRequest = max(maxRequest, length)
			return bytes.Clone(raw[offset : offset+length]), nil
		})
		consumed := step.State.Next - state.Next
		if err != nil || consumed <= 0 || consumed > physical || step.ReadBytes != physical || calls < 1 || calls > 64 || physical > DocDisplayStepSourceBytes || maxRequest > 4<<10 || physical > consumed+(4<<10)+12 {
			test.Fatalf("step=%d physical=%d consumed=%d calls=%d max-request=%d reported=%d error=%v", steps, physical, consumed, calls, maxRequest, step.ReadBytes, err)
		}
		if !bytes.Equal(before, docDisplayStringTestCheckpoint(test, source, state)) {
			test.Fatal("adaptive refill mutated the input checkpoint")
		}
		if step.Page == nil || step.Page.Validate(source) != nil || !step.State.Done && (len(step.Page.Runs) != DocDisplayPageRuns || utf8.RuneCountInString(step.Page.Text) != DocDisplayPageRuns) {
			test.Fatal("dense fixture did not exercise the unchanged run/page caps", steps)
		}
		text.WriteString(step.Page.Text)
		physicalTotal += physical
		consumedTotal += consumed
		steps++
		checkpoint := docDisplayStringTestCheckpoint(test, source, step.State)
		state, err = DecodeDocDisplayStringState(source, HashContent(checkpoint), checkpoint)
		if err != nil {
			test.Fatal("dense escape checkpoint did not resume", err)
		}
	}
	generation, _, err := FinishDocDisplayString(context.Background(), source, state)
	want := "x" + strings.Repeat("\n", DocDisplayPageRuns*(DocRangeDirectoryFanout+1)) + "y"
	if err != nil || generation.Validate() != nil || generation.TextBytes != len(want) || generation.Pages.Count != steps || text.String() != want || consumedTotal != source.Token.To-source.Token.From || physicalTotal > consumedTotal+steps*((4<<10)+12) {
		test.Fatal("dense escape refills lost text or amplified aggregate work", steps, physicalTotal, consumedTotal, err)
	}
	test.Logf("dense steps=%d physical=%d consumed=%d text=%d", steps, physicalTotal, consumedTotal, len(want))
}

func TestDocDisplayStringSelectedPageLogarithmicAuthentication(test *testing.T) {
	for _, count := range []int{1, DocRangeDirectoryFanout, DocRangeDirectoryFanout + 1, DocRangeDirectoryFanout * DocRangeDirectoryFanout, DocRangeDirectoryFanout*DocRangeDirectoryFanout + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			pageText := strings.Repeat("x", DocDisplayPageRuns)
			event := docDisplayStringTestGeneratedEvent{prefix: `{"kind":"tool_result","output":"`, count: count * len(pageText), suffix: `"}`}
			source, err := BuildDocDisplayStringSourceReader(context.Background(), event.hash(test), event.size(), event.reader())
			if err != nil {
				test.Fatal(err)
			}
			sourceHash, err := source.Hash()
			if err != nil {
				test.Fatal(err)
			}
			unselected := HashContent([]byte("unselected page deliberately absent"))
			leaves := make([]ContentHash, count)
			for index := range leaves {
				leaves[index] = unselected
			}
			stored := make(map[ContentHash][]byte)
			selected := []int{0}
			if count > 1 {
				selected = append(selected, count/2, count-1)
			}
			for _, index := range selected {
				from := index * len(pageText)
				page := DocDisplayPage{Version: 1, Source: sourceHash, Index: index, From: from, Text: pageText, Runs: []DocDisplayRun{{To: len(pageText), SourceFrom: source.Token.From + 1 + from, SourceTo: source.Token.From + 1 + from + len(pageText), Linear: true}}}
				encoded, err := page.Encode(source)
				if err != nil {
					test.Fatal(err)
				}
				leaves[index] = HashContent(encoded)
				stored[leaves[index]] = encoded
			}
			var directories []DocRangeObject
			root, err := buildDocRangeDirectory(context.Background(), leaves, &directories)
			if err != nil {
				test.Fatal(err)
			}
			docDisplayStringTestStore(test, stored, directories)
			generation := DocDisplayStringGeneration{Version: 1, Source: source, TextBytes: count * len(pageText), Pages: root}
			if generation.Validate() != nil {
				test.Fatal("invalid selected-page fixture")
			}
			depth := 1
			for capacity := DocRangeDirectoryFanout; count > capacity; capacity *= DocRangeDirectoryFanout {
				depth++
			}
			for _, index := range selected {
				var path []ContentHash
				loaded := 0
				page, err := ReadDocDisplayPage(context.Background(), generation, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
					path = append(path, hash)
					data, exists := stored[hash]
					if !exists {
						return nil, fmt.Errorf("unselected or missing object %s", hash)
					}
					loaded += len(data)
					return data, nil
				})
				if err != nil || page.Index != index || page.Text != pageText || len(path) != depth+1 || loaded > depth*MaxDocRangeDirectoryBytes+MaxDocDisplayPageBytes || path[len(path)-1] != leaves[index] {
					test.Fatal("selected read exceeded its Merkle path", index, len(path), depth+1, loaded, err)
				}
				for _, target := range path {
					for _, corruption := range []string{"changed", "truncated", "oversized", "missing", "substitution"} {
						page, err := ReadDocDisplayPage(context.Background(), generation, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
							if hash != target {
								return stored[hash], nil
							}
							switch corruption {
							case "missing":
								return nil, io.ErrUnexpectedEOF
							case "truncated":
								return stored[hash][:len(stored[hash])-1], nil
							case "oversized":
								return make([]byte, MaxDocDisplayPageBytes+1), nil
							case "substitution":
								if hash == path[len(path)-1] {
									return stored[path[0]], nil
								}
								return stored[path[len(path)-1]], nil
							default:
								changed := bytes.Clone(stored[hash])
								changed[len(changed)/2] ^= 1
								return changed, nil
							}
						})
						want := ErrIntegrity
						if corruption == "missing" {
							want = io.ErrUnexpectedEOF
						}
						if !errors.Is(err, want) || !reflect.DeepEqual(page, DocDisplayPage{}) {
							test.Fatal("corrupt selected object returned a partial page", index, corruption, target, err)
						}
					}
					ctx, cancel := context.WithCancel(context.Background())
					page, err := ReadDocDisplayPage(ctx, generation, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
						if hash == target {
							cancel()
						}
						return stored[hash], nil
					})
					cancel()
					if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocDisplayPage{}) {
						test.Fatal("canceled selected path returned a page", index, target, err)
					}
				}
			}
		})
	}
}

func TestDocDisplayStringPageRejectsAuthenticatedInvalidPayload(test *testing.T) {
	built := docDisplayStringTestBuildAll(test, []byte("{\"kind\":\"tool_result\",\"output\":\"\ud55c🙂\\n\ub05d\"}"), true)
	pageRaw, err := built.pages[0].Encode(built.source)
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*DocDisplayPage){
		"version":          func(page *DocDisplayPage) { page.Version++ },
		"source":           func(page *DocDisplayPage) { page.Source = HashContent([]byte("other source")) },
		"wrong-index":      func(page *DocDisplayPage) { page.Index, page.From = 1, 1 },
		"wrong-first-from": func(page *DocDisplayPage) { page.From = 1 },
		"empty-text":       func(page *DocDisplayPage) { page.Text = "" },
		"oversized-text":   func(page *DocDisplayPage) { page.Text = strings.Repeat("x", DocDisplayPageTextBytes+1) },
		"missing-runs":     func(page *DocDisplayPage) { page.Runs = nil },
		"rune-split":       func(page *DocDisplayPage) { page.Runs[0].To = 1 },
		"source-quote":     func(page *DocDisplayPage) { page.Runs[0].SourceFrom = built.source.Token.From },
		"source-overrun":   func(page *DocDisplayPage) { page.Runs[len(page.Runs)-1].SourceTo = built.source.Token.To },
		"noncontiguous":    func(page *DocDisplayPage) { page.Runs[1].SourceFrom++ },
		"uncovered-text":   func(page *DocDisplayPage) { page.Runs = page.Runs[:len(page.Runs)-1] },
		"wrong-run-width":  func(page *DocDisplayPage) { page.Runs[0].SourceTo++ },
	} {
		test.Run(name, func(test *testing.T) {
			var page DocDisplayPage
			if err := json.Unmarshal(pageRaw, &page); err != nil {
				test.Fatal(err)
			}
			mutate(&page)
			encoded, err := canonicalJSON(page)
			if err != nil {
				test.Fatal(err)
			}
			var objects []DocRangeObject
			root, err := buildDocRangeDirectory(context.Background(), []ContentHash{HashContent(encoded)}, &objects)
			if err != nil {
				test.Fatal(err)
			}
			stored := map[ContentHash][]byte{HashContent(encoded): encoded}
			docDisplayStringTestStore(test, stored, objects)
			generation := built.generation
			generation.Pages = root
			decoded, err := ReadDocDisplayPage(context.Background(), generation, 0, func(_ context.Context, hash ContentHash) ([]byte, error) {
				return stored[hash], nil
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocDisplayPage{}) {
				test.Fatal("authenticated but invalid page became visible", decoded, err)
			}
		})
	}
	for _, encoded := range [][]byte{
		append(bytes.Clone(pageRaw), ' '),
		append([]byte(`{"unknown":true,`), pageRaw[1:]...),
	} {
		page, err := DecodeDocDisplayPage(built.source, HashContent(encoded), encoded)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
			test.Fatal("self-hashed noncanonical page accepted", err)
		}
	}
}

func TestDocDisplayStringReadRejectsInvalidSelectionBeforeIO(test *testing.T) {
	built := docDisplayStringTestBuildAll(test, []byte(`{"kind":"tool_result","output":"x"}`), true)
	for _, index := range []int{-1, built.generation.Pages.Count, built.generation.Pages.Count + 1, int(^uint(0) >> 1)} {
		page, err := ReadDocDisplayPage(context.Background(), built.generation, index, func(context.Context, ContentHash) ([]byte, error) {
			test.Fatal("invalid selection performed IO", index)
			return nil, nil
		})
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
			test.Fatal("invalid selection returned a page", index, err)
		}
	}
	for _, generation := range []DocDisplayStringGeneration{{}, {Version: 1, Source: built.source}, {Version: 1, Source: built.source, TextBytes: 1}} {
		page, err := ReadDocDisplayPage(context.Background(), generation, 0, func(context.Context, ContentHash) ([]byte, error) {
			test.Fatal("invalid/empty descriptor performed IO")
			return nil, nil
		})
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
			test.Fatal("invalid/empty generation returned a page", err)
		}
	}
	page, err := ReadDocDisplayPage(context.Background(), built.generation, 0, nil)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) {
		test.Fatal("nil loader returned a page", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err = ReadDocDisplayPage(ctx, built.generation, 0, func(context.Context, ContentHash) ([]byte, error) {
		test.Fatal("pre-canceled page read performed IO")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocDisplayPage{}) {
		test.Fatal("pre-canceled read returned a page", err)
	}
}

func TestDocDisplayStringWindowsMatchIndependentWholeStringSearch(test *testing.T) {
	token := `"` + strings.Repeat(`\u0061`, DocDisplayPageRuns*2-2) + `\ud83d\ude42\u006b` + strings.Repeat(`\u0061`, DocDisplayPageRuns+2) + `\u212a\u017f\u03a3\u03c2\u0062"`
	raw := []byte(`{"kind":"tool_result","output":` + token + `}`)
	built := docDisplayStringTestBuildAll(test, raw, true)
	var text string
	if err := json.Unmarshal([]byte(token), &text); err != nil {
		test.Fatal(err)
	}
	old, err := BuildDocToolDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	var positions []int
	characters := []rune(text)
	for offset := range text {
		positions = append(positions, offset)
	}
	positions = append(positions, len(text))
	for _, query := range []string{"AA", strings.Repeat("a", MaxDocFindQueryRunes), "🙂KA", "KsσσB", "not present"} {
		test.Run(fmt.Sprintf("query-%x", query), func(test *testing.T) {
			var expected [][2]int
			queryRunes := utf8.RuneCountInString(query)
			for index := 0; index+queryRunes <= len(characters); index++ {
				if strings.EqualFold(string(characters[index:index+queryRunes]), query) {
					expected = append(expected, [2]int{positions[index], positions[index+queryRunes] - positions[index]})
				}
			}
			for _, limit := range []int{1, 7, DocFindPageMatches} {
				var actual [][2]int
				for index, current := range built.pages {
					var previous *DocDisplayPage
					if index > 0 {
						previous = &built.pages[index-1]
					}
					window, err := BuildDocDisplaySearchWindow(built.source, previous, current)
					if err != nil {
						test.Fatal("window rejected streamed pages", index, err)
					}
					from := current.From
					if previous != nil {
						prefix := []rune(previous.Text)
						from -= len(string(prefix[max(0, len(prefix)-MaxDocFindQueryRunes+1):]))
					}
					if window.From() != from || window.To() != current.From+len(current.Text) || window.Text() != text[window.From():window.To()] || utf8.RuneCountInString(text[window.From():current.From]) > MaxDocFindQueryRunes-1 {
						test.Fatal("window retained the wrong prefix or offsets", index, window.From(), window.To())
					}
					for start := window.From(); start < window.To(); {
						matches, next, err := FindDocDisplayWindow(context.Background(), window, query, start, limit)
						if err != nil || next <= start || next > window.To() || len(matches) > limit {
							test.Fatal("window search lost bounded cursor progress", index, start, next, len(matches), err)
						}
						for _, match := range matches {
							if match.Offset < start || match.Offset+match.Length <= current.From || match.PreviewOffset < window.From() || match.PreviewOffset+len(match.Preview) > window.To() || text[match.PreviewOffset:match.PreviewOffset+len(match.Preview)] != match.Preview {
								test.Fatal("match used local offsets or duplicated prior-only text", match)
							}
							span, err := old.SourceRange(match.Offset, match.Length)
							if err != nil || match.SourceRange == nil || *match.SourceRange != span {
								test.Fatal("cross-page match lost its original JSON provenance", match, span, err)
							}
							actual = append(actual, [2]int{match.Offset, match.Length})
						}
						start = next
					}
				}
				if !reflect.DeepEqual(actual, expected) {
					test.Fatalf("limit=%d window matches=%d independent matches=%d", limit, len(actual), len(expected))
				}
			}
		})
	}
}

func TestDocDisplayStringLiteralHTMLSplitsByEncodedPageBudget(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		pattern string
		count   int
	}{
		{"less-than", "<", DocDisplayPageTextBytes + 1},
		{"greater-than", ">", DocDisplayPageTextBytes + 1},
		{"ampersand", "&", DocDisplayPageTextBytes + 1},
		{"mixed", "<>&", DocDisplayPageTextBytes/3 + 1},
		{"mixed-long-line", "<>&", (DocDisplayPageTextBytes*2+MaxDocFindQueryRunes)/3 + 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			payload := strings.Repeat(fixture.pattern, fixture.count)
			raw := []byte(`{"kind":"tool_result","output":"` + payload + `","locked":{"blob":"not displayed"}}`)
			var event struct {
				Output string `json:"output"`
			}
			if err := json.Unmarshal(raw, &event); err != nil || event.Output != payload || len(payload) <= DocDisplayPageTextBytes || bytes.Contains(raw, []byte(`\u003c`)) || bytes.Contains(raw, []byte(`\u003e`)) || bytes.Contains(raw, []byte(`\u0026`)) {
				test.Fatal("fixture did not preserve the long literal HTML source", err)
			}
			unsplit, err := json.Marshal(payload[:DocDisplayPageTextBytes])
			if err != nil || len(unsplit) <= MaxDocDisplayPageBytes {
				test.Fatal("fixture did not exceed the encoded page budget before the text cap", len(unsplit), err)
			}
			built := docDisplayStringTestBuildAll(test, raw, true)
			uninterrupted := docDisplayStringTestBuildAll(test, raw, false)
			if !reflect.DeepEqual(built, uninterrupted) {
				test.Fatal("encoding-budget split changed after checkpoint resumption")
			}
			if len(built.pages) < 2 || built.generation.TextBytes != len(payload) || built.generation.Pages.Count != len(built.pages) {
				test.Fatal("long literal HTML did not produce a complete multipage generation")
			}
			var text strings.Builder
			maxEncoded := 0
			for index, current := range built.pages {
				encoded, err := current.Encode(built.source)
				if err != nil || len(encoded) > MaxDocDisplayPageBytes || len(current.Text) > DocDisplayPageTextBytes || len(current.Runs) != 1 || !current.Runs[0].Linear {
					test.Fatal("literal HTML exceeded page encoding/text/run bounds", index, len(encoded), len(current.Text), len(current.Runs), err)
				}
				if index < len(built.pages)-1 && len(current.Text) >= DocDisplayPageTextBytes {
					test.Fatal("page waited for the text cap instead of the smaller encoding budget", index)
				}
				maxEncoded = max(maxEncoded, len(encoded))
				text.WriteString(current.Text)
				var previous *DocDisplayPage
				if index > 0 {
					previous = &built.pages[index-1]
				}
				window, err := BuildDocDisplaySearchWindow(built.source, previous, current)
				if err != nil || window.Text() != payload[window.From():window.To()] || window.To() != current.From+len(current.Text) {
					test.Fatal("HTML budget split broke the search window", index, err)
				}
				if previous == nil {
					continue
				}
				start := current.From - (MaxDocFindQueryRunes - 1)
				query := payload[start : current.From+1]
				matches, _, err := FindDocDisplayWindow(context.Background(), window, query, start, 1)
				span := DocFindSpan{From: built.source.Token.From + 1 + start, To: built.source.Token.From + 1 + current.From + 1}
				if err != nil || len(matches) != 1 || matches[0].Offset != start || matches[0].Length != len(query) || matches[0].SourceRange == nil || *matches[0].SourceRange != span {
					test.Fatal("maximum-length query lost the encoding-budget page boundary", index, matches, span, err)
				}
				match := matches[0]
				if match.PreviewOffset < window.From() || match.PreviewOffset+len(match.Preview) > window.To() || match.Preview != payload[match.PreviewOffset:match.PreviewOffset+len(match.Preview)] {
					test.Fatal("HTML preview used local or incomplete text offsets", index, match)
				}
				matches, _, err = FindDocDisplayWindow(context.Background(), window, current.Text[:1], window.From(), 1)
				if err != nil || len(matches) != 1 || matches[0].Offset != current.From {
					test.Fatal("HTML search re-emitted a previous-page-only match", index, matches, err)
				}
			}
			if text.String() != event.Output {
				test.Fatal("encoding-budget splits omitted, duplicated, or escaped display text")
			}
			test.Logf("literal source text=%d pages=%d largest encoded page=%d", len(payload), len(built.pages), maxEncoded)
		})
	}
}
