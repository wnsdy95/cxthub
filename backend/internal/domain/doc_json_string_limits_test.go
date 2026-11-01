package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDocJSONStringDefaultLiteralHashes(test *testing.T) {
	for _, scenario := range []struct {
		name        string
		token       string
		displayHash ContentHash
		decodedHash ContentHash
	}{
		{name: "mixed", token: "\"\ud55c\uae00🙂 \\n\\t\\\"\\\\\\ud83d\\ude42\\ud800<>&\"", displayHash: "sha256:b3894a31fb3cdc10676149fb978ebccf3ec20e1cc786fb4b4ca2d7cb9695c0f7", decodedHash: "sha256:17ee4c51db3df95c822bc17eacc8c192d6543417a4c564bf62c6f87f6cf5349c"},
		{name: "byte-boundary", token: `"` + strings.Repeat("a", DocDisplayPageTextBytes-3) + `🙂tail"`, displayHash: "sha256:b5e9dcf926d79b549139f8fd8d426b099a0c25da8164c6f1ffa64104c6b401f8", decodedHash: "sha256:fb2928d31c70328c6a5985b0da9bf6c75a5a134e289babe53aae176424bd75ec"},
		{name: "run-boundary", token: `"` + strings.Repeat(`\u0061`, DocDisplayPageRuns+1) + `\ud83d\ude42"`, displayHash: "sha256:4b63e87296b9a201d1ca996f5865e7ee31122673c3be7a9c30489462637ea31d", decodedHash: "sha256:00e75002cb955d907300e93176f40fcd5f73939cfb708c4b79a1d1acfa1a8382"},
		{name: "encoded-boundary", token: `"` + strings.Repeat("<>&", DocDisplayPageTextBytes/3+1) + `"`, displayHash: "sha256:82769b0da2b8165f1b39c3a631b922f4f757676ea0dee3a6d3b67542cf7be2a1", decodedHash: "sha256:e66f1ed8c6eaf7d524bcf27c5cd4e9dee2a04cd95a148b49f2ef2e5ceba8af5f"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(`{"kind":"tool_result","output":` + scenario.token + `}`)
			display := docDisplayStringTestBuildAll(test, raw, true)
			if display.source.Profile != "tool-result-string-v1" {
				test.Fatal("display profile changed", display.source.Profile)
			}
			displaySource, err := display.source.Encode()
			if err != nil {
				test.Fatal(err)
			}
			displayState, err := display.state.Encode(display.source)
			if err != nil {
				test.Fatal(err)
			}
			displayGeneration, err := display.generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			displayHash := docJSONStringSnapshotHash(test, displaySource, display.pages, displayState, displayGeneration)
			decodedSource := DocDecodedTextSource{Version: 1, Profile: "public-json-string-input-v1", EventHash: HashContent(raw), EventBytes: len(raw), Token: display.source.Token}
			decoded := buildDecodedTextSourceFixture(test, raw, decodedSource)
			decodedSourceBytes, err := decoded.source.Encode()
			if err != nil {
				test.Fatal(err)
			}
			decodedState, err := decoded.state.Encode(decoded.source)
			if err != nil {
				test.Fatal(err)
			}
			decodedGeneration, err := decoded.generation.Encode()
			if err != nil {
				test.Fatal(err)
			}
			decodedHash := docJSONStringSnapshotHash(test, decodedSourceBytes, decoded.pages, decodedState, decodedGeneration)
			var expected string
			if err := json.Unmarshal([]byte(scenario.token), &expected); err != nil {
				test.Fatal(err)
			}
			var displayText, decodedText strings.Builder
			for _, page := range display.pages {
				displayText.WriteString(page.Text)
			}
			for _, page := range decoded.pages {
				decodedText.WriteString(page.Text)
			}
			if displayText.String() != expected || decodedText.String() != expected {
				test.Fatal("default profiles differ from JSON decoding")
			}
			if displayHash != scenario.displayHash || decodedHash != scenario.decodedHash {
				test.Fatal("default source/page/state/generation bytes changed", displayHash, decodedHash)
			}
			for _, version := range []int{1, 3} {
				docJSONStringDefaultMaximumParity(test, raw, display.source.Token, version)
			}
		})
	}
}

func docJSONStringDefaultMaximumParity(test *testing.T, raw []byte, token DocFindSpan, version int) {
	test.Helper()
	request := docJSONStringPageRequest{Version: version, Source: HashContent(raw), Token: token, Next: token.From}
	for {
		var defaultReads, maximumReads []DocFindSpan
		defaultResult, err := decodeDocJSONStringPage(context.Background(), request, func(_ context.Context, from, length int) ([]byte, error) {
			defaultReads = append(defaultReads, DocFindSpan{From: from, To: from + length})
			return raw[from : from+length], nil
		})
		if err != nil {
			test.Fatal(err)
		}
		maximum := request
		maximum.MaxBytes, maximum.MaxRuns, maximum.MaxRead = DocDisplayPageTextBytes, DocDisplayPageRuns, DocDisplayStepSourceBytes
		maximumResult, err := decodeDocJSONStringPage(context.Background(), maximum, func(_ context.Context, from, length int) ([]byte, error) {
			maximumReads = append(maximumReads, DocFindSpan{From: from, To: from + length})
			return raw[from : from+length], nil
		})
		if err != nil || !reflect.DeepEqual(defaultResult, maximumResult) || !reflect.DeepEqual(defaultReads, maximumReads) {
			test.Fatal("explicit maximum limits changed default results or reads", err)
		}
		if defaultResult.Done {
			return
		}
		request.Next = defaultResult.Next
		if defaultResult.Page != nil {
			request.Index++
			request.From += len(defaultResult.Page.Text)
		}
	}
}

func TestDocJSONStringPageLimits(test *testing.T) {
	for _, scenario := range []struct {
		name       string
		token      string
		maxBytes   int
		maxRuns    int
		maxRead    int
		next       int
		text       string
		resultNext int
		done       bool
		failure    bool
	}{
		{name: "one-byte", token: `"abc"`, maxBytes: 1, text: "a", resultNext: 2},
		{name: "utf8-fits", token: "\"\ud55c\"", maxBytes: 3, text: "\ud55c", resultNext: 4},
		{name: "utf8-does-not-fit", token: "\"\ud55c\"", maxBytes: 2, failure: true},
		{name: "astral-fits", token: `"🙂"`, maxBytes: 4, text: "🙂", resultNext: 5},
		{name: "astral-does-not-fit", token: `"🙂"`, maxBytes: 3, failure: true},
		{name: "escaped-fits", token: `"\ud55c"`, maxBytes: 3, text: "\ud55c", resultNext: 7},
		{name: "escaped-does-not-fit", token: `"\ud55c"`, maxBytes: 2, failure: true},
		{name: "surrogate-fits", token: `"\ud83d\ude42"`, maxBytes: 4, text: "🙂", resultNext: 13},
		{name: "surrogate-does-not-fit", token: `"\ud83d\ude42"`, maxBytes: 3, failure: true},
		{name: "prefix-before-large-rune", token: `"a🙂"`, maxBytes: 4, text: "a", resultNext: 2},
		{name: "resumed-rune-does-not-fit", token: `"a🙂"`, maxBytes: 3, next: 2, failure: true},
		{name: "one-run", token: `"abc"`, maxRuns: 1, text: "a", resultNext: 2},
		{name: "two-mixed-runs", token: `"ab\ncd"`, maxRuns: 2, text: "ab\n", resultNext: 5},
		{name: "two-escaped-runs", token: `"\n\t\u0061"`, maxRuns: 2, text: "\n\t", resultNext: 5},
		{name: "empty", token: `""`, maxBytes: 1, maxRuns: 1, maxRead: 12, resultNext: 2, done: true},
		{name: "closing-quote-only", token: `"abc"`, next: 4, maxRead: 12, resultNext: 5, done: true},
		{name: "read-budget-ascii", token: `"abcdefghijklmno"`, maxRead: 12, text: "abcdefghijk", resultNext: 12},
		{name: "read-budget-utf8", token: `"🙂🙂🙂🙂"`, maxRead: 12, text: "🙂🙂", resultNext: 9},
		{name: "read-budget-partial-escape", token: `"abc\u0061\u0062tail"`, maxRead: 12, text: "abca", resultNext: 10},
		{name: "read-budget-surrogate-no-progress", token: `"\ud83d\ude42tail"`, maxRead: 12, failure: true},
		{name: "read-budget-surrogate-resume", token: `"\ud83d\ude42tail"`, maxRead: 12, next: 1, text: "🙂", resultNext: 13},
		{name: "read-budget-unpaired-surrogate", token: `"\ud800\u0061tail"`, maxRead: 13, text: "�a", resultNext: 13},
		{name: "read-budget-unpaired-before-literal", token: `"\ud800plainplain"`, maxRead: 12, text: "�plain", resultNext: 12},
		{name: "read-budget-unpaired-before-simple-escape", token: `"\ud800\nlongtail"`, maxRead: 12, text: "�\nlon", resultNext: 12},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(scenario.token)
			request := docJSONStringPageRequest{Version: 3, Source: HashContent(raw), Token: DocFindSpan{To: len(raw)}, Next: scenario.next, MaxBytes: scenario.maxBytes, MaxRuns: scenario.maxRuns, MaxRead: scenario.maxRead}
			result, err := docJSONStringDecodeLimited(test, raw, request)
			if scenario.failure {
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, docJSONStringPageResult{}) {
					test.Fatal("non-progressing limits did not fail explicitly", result, err)
				}
				return
			}
			if err != nil || result.Next != scenario.resultNext || result.Done != scenario.done {
				test.Fatal("unexpected fragment progress", result, err)
			}
			if scenario.text == "" {
				if result.Page != nil {
					test.Fatal("empty fragment emitted a page", result.Page)
				}
				return
			}
			if result.Page == nil || result.Page.Text != scenario.text || result.HasContent != strings.ContainsFunc(scenario.text, func(character rune) bool { return !docMarkdownWhitespace(character) }) {
				test.Fatal("unexpected fragment text or content flag", result)
			}
		})
	}
}

func docJSONStringDecodeLimited(test *testing.T, raw []byte, request docJSONStringPageRequest) (docJSONStringPageResult, error) {
	test.Helper()
	maxBytes, maxRuns, maxRead := request.MaxBytes, request.MaxRuns, request.MaxRead
	if maxBytes == 0 {
		maxBytes = DocDisplayPageTextBytes
	}
	if maxRuns == 0 {
		maxRuns = DocDisplayPageRuns
	}
	if maxRead == 0 {
		maxRead = DocDisplayStepSourceBytes
	}
	readBytes, readTo := 0, request.Next
	result, err := decodeDocJSONStringPage(context.Background(), request, func(_ context.Context, from, length int) ([]byte, error) {
		readBytes += length
		if from != readTo || from < request.Token.From || length <= 0 || length > 4096 || from+length > request.Token.To || readBytes > maxRead {
			test.Fatal("fragment exceeded read budget or token", from, length, readBytes, maxRead)
		}
		readTo += length
		return bytes.Clone(raw[from : from+length]), nil
	})
	if err == nil {
		if result.ReadBytes != readBytes || result.Next <= request.Next || result.Next > readTo {
			test.Fatal("fragment reported incorrect read progress", result, readBytes, readTo)
		}
		if result.Page != nil && (len(result.Page.Text) > maxBytes || len(result.Page.Runs) > maxRuns || result.Page.validateCoordinates(len(raw), true) != nil) {
			test.Fatal("fragment exceeded output limits or split a rune", result.Page)
		}
	}
	return result, err
}

func TestDocJSONStringLimitsResumeParity(test *testing.T) {
	token := `"` + strings.Repeat("ab\ud55c🙂\\n\\t\\\"\\\\\\/\\u003c\\ud55c\\ud83d\\ude42\\ud800x\\udc00", 513) + `"`
	prefix, suffix := `{"input":`, `,"other":"not decoded"}`
	raw := []byte(prefix + token + suffix)
	var expected string
	if err := json.Unmarshal([]byte(token), &expected); err != nil {
		test.Fatal(err)
	}
	for _, limits := range []struct {
		name     string
		maxBytes int
		maxRuns  int
		maxRead  int
	}{
		{name: "minimum-read-single-run", maxBytes: 4, maxRuns: 1, maxRead: 12},
		{name: "small-mixed", maxBytes: 7, maxRuns: 2, maxRead: 13},
		{name: "partial-read", maxBytes: 23, maxRuns: 7, maxRead: 17},
		{name: "refill-boundary", maxBytes: 1024, maxRuns: 512, maxRead: 4097},
		{name: "defaults"},
	} {
		test.Run(limits.name, func(test *testing.T) {
			request := docJSONStringPageRequest{Version: 3, Source: HashContent(raw), Token: DocFindSpan{From: len(prefix), To: len(raw) - len(suffix)}, Next: len(prefix), MaxBytes: limits.maxBytes, MaxRuns: limits.maxRuns, MaxRead: limits.maxRead}
			var decoded strings.Builder
			for steps := 0; ; steps++ {
				if steps > len(token) {
					test.Fatal("limited fragments did not finish")
				}
				result, err := docJSONStringDecodeLimited(test, raw, request)
				if err != nil {
					test.Fatal("limited fragment failed", steps, request.Next, err)
				}
				if result.Page != nil {
					page := result.Page
					if page.Index != request.Index || page.From != request.From || page.Version != request.Version || page.Source != request.Source {
						test.Fatal("fragment lost request coordinates", page)
					}
					for offset, character := range page.Text {
						span, err := page.SourceRange(offset, utf8.RuneLen(character))
						if err != nil || span.From < request.Next || span.To > result.Next {
							test.Fatal("fragment lost source mapping", span, err)
						}
						encoded := raw[span.From:span.To]
						original := string(encoded)
						if encoded[0] == '\\' {
							if err := json.Unmarshal([]byte(`"`+string(encoded)+`"`), &original); err != nil {
								test.Fatal("fragment split a JSON escape", span, err)
							}
						}
						if original != string(character) {
							test.Fatal("fragment source mapping changed text", original, character)
						}
					}
					decoded.WriteString(page.Text)
					request.Index++
					request.From += len(page.Text)
				}
				if result.Done {
					if result.Next != request.Token.To || decoded.String() != expected {
						test.Fatal("resumed fragments differ from JSON decoding")
					}
					break
				}
				if result.Page == nil {
					test.Fatal("limited fragment silently made no text progress")
				}
				request.Next = result.Next
			}
		})
	}
}

func TestDocJSONStringRejectsInvalidLimitsBeforeRead(test *testing.T) {
	for _, limits := range []docJSONStringPageRequest{
		{MaxBytes: -1}, {MaxBytes: DocDisplayPageTextBytes + 1},
		{MaxRuns: -1}, {MaxRuns: DocDisplayPageRuns + 1},
		{MaxRead: -1}, {MaxRead: 1}, {MaxRead: 11}, {MaxRead: DocDisplayStepSourceBytes + 1},
	} {
		limits.Token = DocFindSpan{To: 3}
		result, err := decodeDocJSONStringPage(context.Background(), limits, func(context.Context, int, int) ([]byte, error) {
			test.Fatal("invalid limits fetched source bytes")
			return nil, nil
		})
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, docJSONStringPageResult{}) {
			test.Fatal("invalid limits accepted", limits, result, err)
		}
	}
}

func TestDocJSONStringSharedInputReadBudget(test *testing.T) {
	for _, limit := range []int{0, 12, 13, 1024, 4097, DocDisplayStepSourceBytes} {
		maxRead := limit
		if maxRead == 0 {
			maxRead = DocDisplayStepSourceBytes
		}
		readBytes, calls := 0, 0
		input := docDisplayStringInput{ctx: context.Background(), end: DocDisplayStepSourceBytes * 2, maxRead: limit}
		input.load = func(_ context.Context, from, length int) ([]byte, error) {
			calls++
			if from != readBytes || length <= 0 || length > 4096 || calls == 1 && length > 1024 || readBytes+length > maxRead {
				test.Fatal("shared input exceeded budget", limit, from, length, readBytes)
			}
			readBytes += length
			return bytes.Repeat([]byte("a"), length), nil
		}
		for {
			available, err := input.ready(DocDisplayPageTextBytes)
			if err != nil {
				test.Fatal(err)
			}
			if !available {
				break
			}
			input.position = len(input.raw)
		}
		if input.used != maxRead || readBytes != maxRead || input.readTo != maxRead {
			test.Fatal("shared input did not honor its complete budget", limit, input.used, readBytes)
		}
		previousCalls := calls
		for attempts := 0; attempts < 3; attempts++ {
			if available, err := input.ready(DocDisplayPageTextBytes); available || err != nil || calls != previousCalls {
				test.Fatal("exhausted shared input fetched additional bytes", available, err)
			}
		}
	}
}

func TestDocJSONStringLimitedReadFailuresDiscardProgress(test *testing.T) {
	readError := errors.New("limited source failure")
	for _, scenario := range []struct {
		name     string
		short    bool
		expected error
	}{
		{name: "loader-error", expected: readError},
		{name: "short-read", short: true, expected: ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(`"\ud83d\ude42tail"`)
			request := docJSONStringPageRequest{Version: 3, Source: HashContent(raw), Token: DocFindSpan{To: len(raw)}, MaxBytes: 4, MaxRead: 13}
			calls, readBytes := 0, 0
			result, err := decodeDocJSONStringPage(context.Background(), request, func(_ context.Context, from, length int) ([]byte, error) {
				calls++
				readBytes += length
				if readBytes > request.MaxRead {
					test.Fatal("failed load exceeded budget")
				}
				if calls == 2 {
					if scenario.short {
						return nil, nil
					}
					return nil, readError
				}
				return raw[from : from+length], nil
			})
			if calls != 2 || !errors.Is(err, scenario.expected) || !reflect.DeepEqual(result, docJSONStringPageResult{}) {
				test.Fatal("limited read failure retained partial output", calls, result, err)
			}
		})
	}
}

func docJSONStringSnapshotHash(test *testing.T, source []byte, pages []DocDisplayPage, state, generation []byte) ContentHash {
	test.Helper()
	var snapshot bytes.Buffer
	snapshot.Write(source)
	for _, page := range pages {
		encoded, err := canonicalJSON(page)
		if err != nil {
			test.Fatal(err)
		}
		snapshot.Write(encoded)
	}
	snapshot.Write(state)
	snapshot.Write(generation)
	return HashContent(snapshot.Bytes())
}
