package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDocDisplayMappingReviewCursorCanonicalityAndSourceIsolation(test *testing.T) {
	first := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","redacted_summary":"**same**","opaque":"first"}`))
	second := buildDecodedMarkdownFixture(test, []byte(`{"kind":"reasoning","opaque":"other source","redacted_summary":"**same**"}`))
	left, err := NewDocDecodedMarkdownAssemblyInput(first.plan)
	if err != nil {
		test.Fatal(err)
	}
	right, err := NewDocDecodedMarkdownAssemblyInput(second.plan)
	if err != nil {
		test.Fatal(err)
	}
	var native docMappedMarkdownCursor
	if err := json.Unmarshal(left.initial, &native); err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []json.RawMessage{
		append(bytes.Clone(left.initial), ' '),
		append([]byte(`{"version":1,`), left.initial[1:]...),
		append([]byte(`{"unknown":null,`), left.initial[1:]...),
		bytes.Replace(left.initial, []byte(`"version":1`), []byte(`"version":1.0`), 1),
		right.initial,
	} {
		calls := 0
		page, next, failure := left.read(context.Background(), candidate, 64, 8, func(context.Context, ContentHash) ([]byte, error) {
			calls++
			return nil, ErrNotFound
		})
		if !errors.Is(failure, ErrIntegrity) || calls != 0 || next != nil || !reflect.DeepEqual(page, DocDisplayPage{}) {
			test.Fatal("foreign or noncanonical cursor reached prepared objects", calls, failure)
		}
	}
	native.Render.Plan = right.source.Plan
	changed, err := canonicalJSON(native)
	if err != nil {
		test.Fatal(err)
	}
	if _, failure := left.cursor(changed); !errors.Is(failure, ErrIntegrity) {
		test.Fatal("outer source binding hid a foreign render plan", failure)
	}
}

func TestDocDisplayMappingReviewLiteralGapRatioRemainsStrict(test *testing.T) {
	fixture := newPreparedTextReaderFixture(test, strings.Repeat("x", 513*4))
	reader := fixture.reader(test)
	from, length := 1, len(fixture.text)-2
	span, err := reader.locateSource(context.Background(), from, length, false)
	if err != nil || span != (DocFindSpan{From: 1, To: 300512}) {
		test.Fatal("sparse nonliteral range was not supported", span, err)
	}
	if span, err := fixture.reader(test).locateSource(context.Background(), from, length, true); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
		test.Fatal("sparse raw gap weakened the literal byte-ratio guard", span, err)
	}
	for _, sourceFrom := range []int{1538, 6670} {
		for index := range fixture.pages[3].Runs {
			fixture.pages[3].Runs[index].SourceFrom = sourceFrom
			fixture.pages[3].Runs[index].SourceTo = sourceFrom + len(fixture.pages[3].Text)
		}
		if span, err := fixture.reader(test).locateSource(context.Background(), from, length, true); !errors.Is(err, ErrIntegrity) || span != (DocFindSpan{}) {
			test.Fatal("impossible literal gap ratio was accepted", sourceFrom, span, err)
		}
	}
}
