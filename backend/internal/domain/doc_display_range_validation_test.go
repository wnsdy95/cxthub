package domain

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func docDisplayRangeValidationReferenceCoordinates(page DocDisplayPage, sourceBytes int, literal bool) error {
	if len(page.Text) == 0 || len(page.Text) > DocDisplayPageTextBytes || !utf8.ValidString(page.Text) || len(page.Runs) == 0 || len(page.Runs) > DocDisplayPageRuns {
		return ErrIntegrity
	}
	previous, sourceEnd := 0, 0
	for index, run := range page.Runs {
		if run.To <= previous || run.To > len(page.Text) || run.To < len(page.Text) && !utf8.RuneStart(page.Text[run.To]) || run.SourceFrom < 0 || run.SourceFrom >= run.SourceTo || run.SourceTo > sourceBytes || literal && index > 0 && run.SourceFrom != sourceEnd {
			return ErrIntegrity
		}
		text := page.Text[previous:run.To]
		width := run.SourceTo - run.SourceFrom
		if run.Linear {
			if width != len(text) {
				return ErrIntegrity
			}
		} else if literal {
			if utf8.RuneCountInString(text) != 1 {
				return ErrIntegrity
			}
			character, _ := utf8.DecodeRuneInString(text)
			switch width {
			case 2:
				if !strings.ContainsRune("\"\\/\b\f\n\r\t", character) {
					return ErrIntegrity
				}
			case 6:
				if character > 0xffff {
					return ErrIntegrity
				}
			case 12:
				if character <= 0xffff {
					return ErrIntegrity
				}
			default:
				return ErrIntegrity
			}
		}
		previous, sourceEnd = run.To, run.SourceTo
	}
	if previous != len(page.Text) {
		return ErrIntegrity
	}
	return nil
}

func docDisplayRangeValidationReference(page DocDisplayPage, offset, length int) (DocFindSpan, error) {
	if offset < 0 || length <= 0 || length > len(page.Text) || offset > len(page.Text)-length || !utf8.RuneStart(page.Text[offset]) || offset+length < len(page.Text) && !utf8.RuneStart(page.Text[offset+length]) {
		return DocFindSpan{}, ErrValidation
	}
	if page.Version < 1 || page.Version > 4 || docDisplayRangeValidationReferenceCoordinates(page, MaxConversationDocumentBytes, page.Version == 1 || page.Version == 3) != nil {
		return DocFindSpan{}, ErrIntegrity
	}
	span := DocFindSpan{From: math.MaxInt}
	previous := 0
	for _, run := range page.Runs {
		from, to := max(previous, offset), min(run.To, offset+length)
		if from < to {
			if run.Linear {
				from, to = run.SourceFrom+from-previous, run.SourceFrom+to-previous
			} else {
				from, to = run.SourceFrom, run.SourceTo
			}
			span.From, span.To = min(span.From, from), max(span.To, to)
		}
		previous = run.To
	}
	return span, nil
}

func docDisplayRangeValidationLiteralPage() DocDisplayPage {
	page := DocDisplayPage{Version: 1}
	sourceEnd := 1
	for _, unit := range []struct {
		text   string
		width  int
		linear bool
	}{
		{text: "ab", width: 2, linear: true},
		{text: "\n", width: 2},
		{text: "\ud55c", width: 6},
		{text: "🙂", width: 12},
		{text: "�", width: 6},
		{text: "\t", width: 2},
		{text: "/", width: 2},
		{text: "\\", width: 2},
		{text: "\"", width: 2},
		{text: "z", width: 1, linear: true},
	} {
		page.Text += unit.text
		page.Runs = append(page.Runs, DocDisplayRun{To: len(page.Text), SourceFrom: sourceEnd, SourceTo: sourceEnd + unit.width, Linear: unit.linear})
		sourceEnd += unit.width
	}
	return page
}

func TestDocDisplaySourceRangeValidationReferenceParity(test *testing.T) {
	pages := []DocDisplayPage{
		docDisplayRangeValidationLiteralPage(),
		{Text: "ab\ud55c🙂\tZ&", Runs: []DocDisplayRun{
			{To: 2, SourceFrom: 40, SourceTo: 42, Linear: true},
			{To: 5, SourceFrom: 1, SourceTo: 4},
			{To: 9, SourceFrom: 100, SourceTo: 111},
			{To: 11, SourceFrom: 60, SourceTo: 62, Linear: true},
			{To: 12, SourceFrom: 8, SourceTo: 9},
		}},
		{Text: "é", Runs: []DocDisplayRun{{To: 2, SourceFrom: MaxConversationDocumentBytes - 2, SourceTo: MaxConversationDocumentBytes, Linear: true}}},
		{Text: "\"\\\n", Runs: []DocDisplayRun{{To: 3, SourceFrom: 0, SourceTo: 3, Linear: true}}},
		{Text: "ab", Runs: []DocDisplayRun{{To: 2, SourceFrom: 0, SourceTo: 1}}},
		{Text: ""},
	}
	for pageIndex, page := range pages {
		for _, version := range []int{-1, 0, 1, 2, 3, 4, 5, math.MaxInt} {
			page.Version = version
			for offset := -1; offset <= len(page.Text)+1; offset++ {
				for length := -1; length <= len(page.Text)+1; length++ {
					got, failure := page.SourceRange(offset, length)
					want, expectedFailure := docDisplayRangeValidationReference(page, offset, length)
					if got != want || failure != expectedFailure {
						test.Fatalf("page=%d version=%d offset=%d length=%d got=%v/%v want=%v/%v", pageIndex, version, offset, length, got, failure, want, expectedFailure)
					}
				}
			}
			for _, request := range [][2]int{{math.MinInt, 1}, {math.MaxInt, 1}, {0, math.MaxInt}, {1, math.MaxInt}, {0, math.MinInt}, {math.MaxInt, math.MaxInt}} {
				got, failure := page.SourceRange(request[0], request[1])
				want, expectedFailure := docDisplayRangeValidationReference(page, request[0], request[1])
				if got != want || failure != expectedFailure {
					test.Fatal("overflow request changed validation precedence", pageIndex, version, request, got, failure, want, expectedFailure)
				}
			}
		}
		for _, sourceBytes := range []int{-1, 0, 1, 2, 4, 111, MaxConversationDocumentBytes} {
			for _, literal := range []bool{false, true} {
				got := page.validateCoordinates(sourceBytes, literal)
				want := docDisplayRangeValidationReferenceCoordinates(page, sourceBytes, literal)
				if got != want {
					test.Fatal("coordinate-only validation changed", pageIndex, sourceBytes, literal, got, want)
				}
			}
		}
	}
}

func TestDocDisplaySourceRangeRejectsMutableUnselectedCorruption(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		offset int
		mutate func(*DocDisplayPage)
	}{
		{name: "prefix-source", offset: 6, mutate: func(page *DocDisplayPage) { page.Runs[0].SourceFrom = -1 }},
		{name: "suffix-source", mutate: func(page *DocDisplayPage) { page.Runs[3].SourceFrom = -1 }},
		{name: "suffix-source-limit", mutate: func(page *DocDisplayPage) { page.Runs[3].SourceTo = MaxConversationDocumentBytes + 1 }},
		{name: "suffix-source-empty", mutate: func(page *DocDisplayPage) { page.Runs[3].SourceFrom = page.Runs[3].SourceTo }},
		{name: "suffix-source-width", mutate: func(page *DocDisplayPage) { page.Runs[3].SourceTo++ }},
		{name: "suffix-display-limit", mutate: func(page *DocDisplayPage) { page.Runs[3].To++ }},
		{name: "suffix-display-order", mutate: func(page *DocDisplayPage) { page.Runs[3].To = page.Runs[2].To }},
		{name: "suffix-display-gap", mutate: func(page *DocDisplayPage) { page.Runs = page.Runs[:3] }},
		{name: "suffix-display-boundary", mutate: func(page *DocDisplayPage) { page.Runs[2].To-- }},
		{name: "suffix-text-utf8", mutate: func(page *DocDisplayPage) { page.Text = "a\n\xff\xff\xff\xffb" }},
		{name: "text-limit", mutate: func(page *DocDisplayPage) { page.Text = strings.Repeat("a", DocDisplayPageTextBytes+1) }},
		{name: "runs-replacement", mutate: func(page *DocDisplayPage) { page.Runs = nil }},
		{name: "version-replacement", mutate: func(page *DocDisplayPage) { page.Version = 5 }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for version := 1; version <= 4; version++ {
				page := DocDisplayPage{Version: version, Text: "a\n🙂b", Runs: []DocDisplayRun{
					{To: 1, SourceFrom: 1, SourceTo: 2, Linear: true},
					{To: 2, SourceFrom: 2, SourceTo: 4},
					{To: 6, SourceFrom: 4, SourceTo: 16},
					{To: 7, SourceFrom: 16, SourceTo: 17, Linear: true},
				}}
				before, failure := page.SourceRange(scenario.offset, 1)
				if failure != nil {
					test.Fatal("valid page failed before mutation", version, failure)
				}
				alias := page
				scenario.mutate(&alias)
				page.Text, page.Runs, page.Version = alias.Text, alias.Runs, alias.Version
				if span, failure := page.SourceRange(scenario.offset, 1); span != (DocFindSpan{}) || failure != ErrIntegrity {
					test.Fatal("selected cover hid mutable corruption", version, before, span, failure)
				}
				if failure := page.validateCoordinates(MaxConversationDocumentBytes, version == 1 || version == 3); failure != docDisplayRangeValidationReferenceCoordinates(page, MaxConversationDocumentBytes, version == 1 || version == 3) {
					test.Fatal("coordinate-only validation diverged after mutation", version, failure)
				}
			}
		})
	}
}

func TestDocDisplaySourceRangeLiteralValidationRemainsVersionSpecific(test *testing.T) {
	for _, scenario := range []struct {
		name string
		text string
		runs []DocDisplayRun
	}{
		{name: "source-gap", text: "ab", runs: []DocDisplayRun{{To: 1, SourceFrom: 0, SourceTo: 1, Linear: true}, {To: 2, SourceFrom: 2, SourceTo: 3, Linear: true}}},
		{name: "two-byte-unescaped", text: "ax", runs: []DocDisplayRun{{To: 1, SourceFrom: 0, SourceTo: 1, Linear: true}, {To: 2, SourceFrom: 1, SourceTo: 3}}},
		{name: "nonlinear-multiple-runes", text: "aXY", runs: []DocDisplayRun{{To: 1, SourceFrom: 0, SourceTo: 1, Linear: true}, {To: 3, SourceFrom: 1, SourceTo: 7}}},
		{name: "surrogate-pair-bmp", text: "a\ud55c", runs: []DocDisplayRun{{To: 1, SourceFrom: 0, SourceTo: 1, Linear: true}, {To: 4, SourceFrom: 1, SourceTo: 13}}},
		{name: "single-escape-non-bmp", text: "a🙂", runs: []DocDisplayRun{{To: 1, SourceFrom: 0, SourceTo: 1, Linear: true}, {To: 5, SourceFrom: 1, SourceTo: 7}}},
		{name: "unsupported-escape-width", text: "a\n", runs: []DocDisplayRun{{To: 1, SourceFrom: 0, SourceTo: 1, Linear: true}, {To: 2, SourceFrom: 1, SourceTo: 4}}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			page := DocDisplayPage{Version: 2, Text: scenario.text, Runs: scenario.runs}
			for _, version := range []int{2, 1, 4, 3, 2} {
				page.Version = version
				span, failure := page.SourceRange(0, 1)
				if version == 1 || version == 3 {
					if span != (DocFindSpan{}) || failure != ErrIntegrity {
						test.Fatal("literal validation ignored unselected run or version replacement", version, span, failure)
					}
				} else if span != (DocFindSpan{From: 0, To: 1}) || failure != nil {
					test.Fatal("projection inherited literal constraints", version, span, failure)
				}
			}
		})
	}
}

func TestDocDisplaySourceRangeValidationErrorPrecedence(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		page    DocDisplayPage
		offset  int
		length  int
		failure error
	}{
		{name: "empty-page", length: 1, failure: ErrValidation},
		{name: "negative-offset-before-version", page: DocDisplayPage{Version: 5, Text: "a"}, offset: -1, length: 1, failure: ErrValidation},
		{name: "empty-selection-before-runs", page: DocDisplayPage{Version: 1, Text: "a"}, failure: ErrValidation},
		{name: "overflow-before-version", page: DocDisplayPage{Version: 5, Text: "a"}, offset: math.MaxInt, length: math.MaxInt, failure: ErrValidation},
		{name: "start-boundary-before-version", page: DocDisplayPage{Version: 5, Text: "a\ud55cb"}, offset: 2, length: 2, failure: ErrValidation},
		{name: "end-boundary-before-version", page: DocDisplayPage{Version: 5, Text: "a\ud55cb"}, length: 2, failure: ErrValidation},
		{name: "continuation-before-invalid-text", page: DocDisplayPage{Version: 1, Text: "a\x80"}, offset: 1, length: 1, failure: ErrValidation},
		{name: "valid-request-invalid-version", page: DocDisplayPage{Version: 5, Text: "a"}, length: 1, failure: ErrIntegrity},
		{name: "valid-request-invalid-runs", page: DocDisplayPage{Version: 1, Text: "a"}, length: 1, failure: ErrIntegrity},
		{name: "valid-boundary-invalid-utf8", page: DocDisplayPage{Version: 1, Text: "a\xff", Runs: []DocDisplayRun{{To: 2, SourceFrom: 0, SourceTo: 2, Linear: true}}}, length: 1, failure: ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			span, failure := scenario.page.SourceRange(scenario.offset, scenario.length)
			if span != (DocFindSpan{}) || failure != scenario.failure {
				test.Fatal("validation precedence or zero output changed", span, failure, scenario.failure)
			}
		})
	}
}

func TestDocDisplaySourceRangeMaximumRunsAndLateCorruption(test *testing.T) {
	for version := 1; version <= 4; version++ {
		page := DocDisplayPage{Version: version, Text: strings.Repeat("a", DocDisplayPageRuns)}
		for index := 0; index < DocDisplayPageRuns; index++ {
			page.Runs = append(page.Runs, DocDisplayRun{To: index + 1, SourceFrom: index * 6, SourceTo: (index + 1) * 6})
		}
		for _, request := range [][2]int{{0, 1}, {DocDisplayPageRuns - 1, 1}, {0, DocDisplayPageRuns}, {17, DocDisplayPageRuns - 17}} {
			span, failure := page.SourceRange(request[0], request[1])
			want := DocFindSpan{From: request[0] * 6, To: (request[0] + request[1]) * 6}
			if span != want || failure != nil {
				test.Fatal("maximum-run cover changed", version, request, span, failure, want)
			}
		}
		page.Runs[len(page.Runs)-1].SourceTo = MaxConversationDocumentBytes + 1
		if span, failure := page.SourceRange(0, 1); span != (DocFindSpan{}) || failure != ErrIntegrity {
			test.Fatal("early selected cover skipped final run validation", version, span, failure)
		}
		page.Runs[len(page.Runs)-1].SourceTo = DocDisplayPageRuns * 6
		page.Runs = append(page.Runs, DocDisplayRun{To: DocDisplayPageRuns + 1, SourceFrom: DocDisplayPageRuns * 6, SourceTo: (DocDisplayPageRuns + 1) * 6})
		page.Text += "a"
		if span, failure := page.SourceRange(0, 1); span != (DocFindSpan{}) || failure != ErrIntegrity {
			test.Fatal("run count limit changed", version, span, failure)
		}
		page = DocDisplayPage{Version: version, Text: strings.Repeat("a", DocDisplayPageTextBytes), Runs: []DocDisplayRun{{To: DocDisplayPageTextBytes, SourceFrom: 0, SourceTo: DocDisplayPageTextBytes, Linear: true}}}
		if span, failure := page.SourceRange(DocDisplayPageTextBytes-1, 1); span != (DocFindSpan{From: DocDisplayPageTextBytes - 1, To: DocDisplayPageTextBytes}) || failure != nil {
			test.Fatal("maximum-text linear clipping changed", version, span, failure)
		}
	}
}

func TestDocDisplaySourceRangePreservesPageEqualityAndWireEncoding(test *testing.T) {
	source := DocDisplayStringSource{Version: 1, Profile: DocDisplayStringProfile, EventHash: HashContent([]byte(`"ab"`)), EventBytes: 4, Token: DocFindSpan{From: 0, To: 4}}
	hash, failure := source.Hash()
	if failure != nil {
		test.Fatal(failure)
	}
	page := DocDisplayPage{Version: 1, Source: hash, Text: "ab", Runs: []DocDisplayRun{{To: 2, SourceFrom: 1, SourceTo: 3, Linear: true}}}
	before := page
	before.Runs = append([]DocDisplayRun(nil), page.Runs...)
	want := `{"from":0,"index":0,"runs":[{"linear":true,"source_from":1,"source_to":3,"to":2}],"source":"` + string(hash) + `","text":"ab","version":1}`
	for _, request := range [][2]int{{0, 1}, {1, 1}, {0, 2}, {2, 1}, {-1, 1}} {
		span, failure := page.SourceRange(request[0], request[1])
		expectedSpan, expectedFailure := docDisplayRangeValidationReference(before, request[0], request[1])
		if span != expectedSpan || failure != expectedFailure || !reflect.DeepEqual(page, before) {
			test.Fatal("range resolution mutated public page state", request, span, failure)
		}
	}
	raw, failure := page.Encode(source)
	if failure != nil || string(raw) != want || HashContent(raw) != HashContent([]byte(want)) {
		test.Fatal("range resolution changed canonical page bytes or hash", string(raw), failure)
	}
	decoded, failure := DecodeDocDisplayPage(source, HashContent(raw), raw)
	if failure != nil || !reflect.DeepEqual(decoded, before) {
		test.Fatal("page round trip changed equality", decoded, failure)
	}
	encoded, failure := decoded.Encode(source)
	if failure != nil || !bytes.Equal(encoded, raw) {
		test.Fatal("page round trip changed wire encoding", failure)
	}
}
