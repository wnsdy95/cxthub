package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func messageDisplayReviewJSON(test *testing.T, value any) []byte {
	test.Helper()
	raw, err := canonicalJSON(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func messageDisplayReviewLoad(objects map[ContentHash][]byte) func(context.Context, ContentHash) ([]byte, error) {
	return func(_ context.Context, hash ContentHash) ([]byte, error) {
		raw, found := objects[hash]
		if !found {
			return nil, ErrNotFound
		}
		return bytes.Clone(raw), nil
	}
}

func messageDisplayReviewPage(test *testing.T, fixture messageDisplayFixture, index int) DocMessageDisplayPage {
	test.Helper()
	hash, err := ResolveDocRangeLeaf(context.Background(), fixture.plan.Parts, index, messageDisplayReviewLoad(fixture.objects))
	if err != nil {
		test.Fatal(err)
	}
	page, err := DecodeDocMessageDisplayPage(fixture.plan.Source, hash, fixture.objects[hash])
	if err != nil {
		test.Fatal(err)
	}
	return page
}

func messageDisplayReviewReplacePage(test *testing.T, fixture messageDisplayFixture, position int, page DocMessageDisplayPage) messageDisplayFixture {
	test.Helper()
	fixture.objects = maps.Clone(fixture.objects)
	var hashes []ContentHash
	for index := 0; index < fixture.plan.Parts.Count; index++ {
		hash, err := ResolveDocRangeLeaf(context.Background(), fixture.plan.Parts, index, messageDisplayReviewLoad(fixture.objects))
		if err != nil {
			test.Fatal(err)
		}
		if index == position {
			raw := messageDisplayReviewJSON(test, page)
			hash = HashContent(raw)
			fixture.objects[hash] = raw
		}
		hashes = append(hashes, hash)
	}
	var objects []DocRangeObject
	root, err := buildDocRangeDirectory(context.Background(), hashes, &objects)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
	}
	fixture.plan.Parts = root
	return fixture
}

func TestDocMessageDisplayReviewCanonicalDescriptors(test *testing.T) {
	fixture := buildMessageDisplayFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"safe"},{"type":"image","name":"asset"}]}`))
	page := messageDisplayReviewPage(test, fixture, 0)
	for _, scenario := range []struct {
		name   string
		value  any
		limit  int
		decode func(ContentHash, []byte) error
	}{
		{"source", fixture.plan.Source, MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMessageDisplaySource(hash, raw)
			return err
		}},
		{"page", page, MaxDocMessageDisplayPageBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMessageDisplayPage(fixture.plan.Source, hash, raw)
			return err
		}},
		{"plan", fixture.plan, MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocMessageDisplayPlan(hash, raw)
			return err
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := messageDisplayReviewJSON(test, scenario.value)
			if err := scenario.decode(HashContent(raw), raw); err != nil {
				test.Fatal("canonical descriptor rejected", err)
			}
			for _, malformed := range [][]byte{
				nil,
				[]byte(`null`),
				append(bytes.Clone(raw), ' '),
				append([]byte(`{"unknown":true,`), raw[1:]...),
				append([]byte(`{"version":1,`), raw[1:]...),
				bytes.Replace(raw, []byte(`"version"`), []byte(`"Version"`), 1),
				bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1.0`), 1),
				bytes.Repeat([]byte(" "), scenario.limit+1),
			} {
				if err := scenario.decode(HashContent(malformed), malformed); !errors.Is(err, ErrIntegrity) {
					test.Fatal("noncanonical descriptor accepted", err)
				}
			}
			for _, hash := range []ContentHash{"", HashContent([]byte("foreign descriptor"))} {
				if err := scenario.decode(hash, raw); !errors.Is(err, ErrIntegrity) {
					test.Fatal("descriptor hash mismatch accepted", err)
				}
			}
		})
	}
	for _, profile := range []string{"", DocMessageInputProfile, DocDisplayProjectionProfile, "PUBLIC-MESSAGE-DISPLAY-PLAN-V1"} {
		changed := fixture.plan
		changed.Source.Profile = profile
		if changed.Source.Validate() == nil || changed.Validate() == nil {
			test.Fatal("foreign ordinary-message profile admitted", profile)
		}
	}
	changed := fixture.plan
	changed.Source.Input.Source.Summary = true
	if changed.Source.Validate() == nil || changed.Validate() == nil {
		test.Fatal("summary input admitted as ordinary message")
	}
}

func TestDocMessageDisplayReviewRejectsImpossiblePartsAndPlans(test *testing.T) {
	fixture := buildMessageDisplayFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"safe"},{"type":"text","text":"last"}]}`))
	page := messageDisplayReviewPage(test, fixture, 0)
	for name, mutate := range map[string]func(*DocMessageDisplayPart){
		"index":         func(part *DocMessageDisplayPart) { part.Index = math.MaxInt },
		"block":         func(part *DocMessageDisplayPart) { part.Block = math.MaxInt },
		"from":          func(part *DocMessageDisplayPart) { part.From = math.MaxInt },
		"negative-from": func(part *DocMessageDisplayPart) { part.From = math.MinInt },
		"separator":     func(part *DocMessageDisplayPart) { part.Separator = math.MaxInt },
		"empty":         func(part *DocMessageDisplayPart) { part.TextBytes = 0 },
		"overflow":      func(part *DocMessageDisplayPart) { part.TextBytes = math.MaxInt },
		"negative":      func(part *DocMessageDisplayPart) { part.TextBytes = math.MinInt },
		"kind":          func(part *DocMessageDisplayPart) { part.Kind = "native" },
		"child-hash":    func(part *DocMessageDisplayPart) { part.Plan = "invalid" },
		"first-from":    func(part *DocMessageDisplayPart) { part.First.From = math.MinInt },
		"first-to":      func(part *DocMessageDisplayPart) { part.First.To = math.MaxInt },
		"first-prefix":  func(part *DocMessageDisplayPart) { part.Separator = 1 },
	} {
		test.Run(name, func(test *testing.T) {
			changed := page
			changed.Parts = append([]DocMessageDisplayPart(nil), page.Parts...)
			mutate(&changed.Parts[0])
			raw := messageDisplayReviewJSON(test, changed)
			if _, err := DecodeDocMessageDisplayPage(fixture.plan.Source, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("authenticated impossible part accepted", err)
			}
		})
	}
	for name, mutate := range map[string]func(*DocMessageDisplayPlan){
		"count-overflow": func(plan *DocMessageDisplayPlan) { plan.Count = math.MaxInt },
		"count-negative": func(plan *DocMessageDisplayPlan) { plan.Count = math.MinInt },
		"bytes-overflow": func(plan *DocMessageDisplayPlan) { plan.TextBytes = math.MaxInt },
		"bytes-negative": func(plan *DocMessageDisplayPlan) { plan.TextBytes = math.MinInt },
		"too-short":      func(plan *DocMessageDisplayPlan) { plan.TextBytes = 2 },
		"empty-length":   func(plan *DocMessageDisplayPlan) { plan.TextBytes = 0 },
		"empty-count":    func(plan *DocMessageDisplayPlan) { plan.Count = 0 },
		"parts-count":    func(plan *DocMessageDisplayPlan) { plan.Parts.Count = math.MaxInt },
		"input-count":    func(plan *DocMessageDisplayPlan) { plan.Source.Input.Count = math.MaxInt },
	} {
		test.Run(name, func(test *testing.T) {
			changed := fixture.plan
			mutate(&changed)
			raw := messageDisplayReviewJSON(test, changed)
			if _, err := DecodeDocMessageDisplayPlan(HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
				test.Fatal("authenticated impossible plan accepted", err)
			}
			if _, err := NewDocMessageDisplayAssemblyInput(changed); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid plan admitted for replay", err)
			}
		})
	}
	for name, mutate := range map[string]func(*DocMessageDisplayPage){
		"index-overflow": func(value *DocMessageDisplayPage) { value.Index = math.MaxInt },
		"missing-prefix": func(value *DocMessageDisplayPage) { value.Parts[1].Separator = 0 },
		"gap":            func(value *DocMessageDisplayPage) { value.Parts[1].From++ },
		"block-order":    func(value *DocMessageDisplayPage) { value.Parts[1].Block = 0 },
		"source-order":   func(value *DocMessageDisplayPage) { value.Parts[1].First = value.Parts[0].First },
	} {
		test.Run(name, func(test *testing.T) {
			changed := page
			changed.Parts = append([]DocMessageDisplayPart(nil), page.Parts...)
			mutate(&changed)
			if changed.Validate(fixture.plan.Source) == nil {
				test.Fatal("impossible page ordering accepted")
			}
		})
	}
}

func TestDocMessageDisplayReviewCursorIsolationBeforeIO(test *testing.T) {
	fixture := buildMessageDisplayFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"abcdefgh"},{"type":"text","text":"last"}]}`))
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	var initial docMessageDisplayCursor
	if err := json.Unmarshal(input.initial, &initial); err != nil {
		test.Fatal(err)
	}
	candidates := []json.RawMessage{
		nil,
		append(bytes.Clone(input.initial), ' '),
		append([]byte(`{"unknown":true,`), input.initial[1:]...),
		append([]byte(`{"version":1,`), input.initial[1:]...),
		bytes.Replace(input.initial, []byte(`"version":1`), []byte(`"version":1.0`), 1),
		bytes.Repeat([]byte(" "), MaxDocDisplaySourceBytes+1),
	}
	for _, mutate := range []func(*docMessageDisplayCursor){
		func(cursor *docMessageDisplayCursor) { cursor.Part = math.MaxInt },
		func(cursor *docMessageDisplayCursor) { cursor.From = math.MaxInt },
		func(cursor *docMessageDisplayCursor) { cursor.TextBytes = math.MinInt },
		func(cursor *docMessageDisplayCursor) { cursor.Separator = math.MaxInt },
		func(cursor *docMessageDisplayCursor) { cursor.Prefix = math.MinInt },
		func(cursor *docMessageDisplayCursor) { cursor.From, cursor.TextBytes = 1, 1 },
		func(cursor *docMessageDisplayCursor) { cursor.Kind = DocMessageDisplayMarkdown },
		func(cursor *docMessageDisplayCursor) { cursor.Child = HashContent([]byte("other child")) },
		func(cursor *docMessageDisplayCursor) { cursor.BodyBytes = 1 },
		func(cursor *docMessageDisplayCursor) {
			cursor.Part, cursor.From, cursor.TextBytes = 1, fixture.plan.TextBytes, fixture.plan.TextBytes
		},
	} {
		changed := initial
		mutate(&changed)
		candidates = append(candidates, messageDisplayReviewJSON(test, changed))
	}
	for _, mutate := range []func(*DocMessageDisplayPlan){
		func(plan *DocMessageDisplayPlan) {
			plan.Source.Input.Source.EventHash = HashContent([]byte("other source"))
		},
		func(plan *DocMessageDisplayPlan) { plan.Parts.Hash = HashContent([]byte("other plan")) },
	} {
		changed := fixture.plan
		mutate(&changed)
		foreign, err := NewDocMessageDisplayAssemblyInput(changed)
		if err != nil || foreign.source.Plan == input.source.Plan {
			test.Fatal("invalid source-isolation fixture", err)
		}
		candidates = append(candidates, foreign.initial)
	}
	for index, candidate := range candidates {
		calls := 0
		page, next, err := input.read(context.Background(), candidate, 4, 1, func(context.Context, ContentHash) ([]byte, error) {
			calls++
			return nil, ErrNotFound
		})
		if !errors.Is(err, ErrIntegrity) || calls != 0 || next != nil || !reflect.DeepEqual(page, DocDisplayPage{}) {
			test.Fatal("malformed or foreign cursor reached objects", index, calls, page, next, err)
		}
	}
}

func TestDocMessageDisplayReviewSelectedPageAdmission(test *testing.T) {
	fixture := buildMessageDisplayFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"safe"},{"type":"text","text":"last"}]}`))
	page := messageDisplayReviewPage(test, fixture, 0)
	for name, mutate := range map[string]func(*DocMessageDisplayPage){
		"short-page":   func(value *DocMessageDisplayPage) { value.Parts = value.Parts[:1] },
		"wrong-index":  func(value *DocMessageDisplayPage) { value.Index++ },
		"wrong-source": func(value *DocMessageDisplayPage) { value.Source = HashContent([]byte("foreign source")) },
		"wrong-length": func(value *DocMessageDisplayPage) { value.Parts[1].TextBytes++ },
		"short-length": func(value *DocMessageDisplayPage) { value.Parts[1].TextBytes-- },
	} {
		test.Run(name, func(test *testing.T) {
			changed := page
			changed.Parts = append([]DocMessageDisplayPart(nil), page.Parts...)
			mutate(&changed)
			corrupted := messageDisplayReviewReplacePage(test, fixture, 0, changed)
			parts, err := ReadDocMessageDisplayParts(context.Background(), corrupted.plan, 0, messageDisplayReviewLoad(corrupted.objects))
			if !errors.Is(err, ErrIntegrity) || parts != nil {
				test.Fatal("authenticated page disagreed with the parent generation", parts, err)
			}
		})
	}
	for _, index := range []int{math.MinInt, -1, fixture.plan.Parts.Count, math.MaxInt} {
		calls := 0
		parts, err := ReadDocMessageDisplayParts(context.Background(), fixture.plan, index, func(context.Context, ContentHash) ([]byte, error) {
			calls++
			return nil, ErrNotFound
		})
		if !errors.Is(err, ErrIntegrity) || calls != 0 || parts != nil {
			test.Fatal("invalid page index reached objects", index, calls, parts, err)
		}
	}
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, cancelDuringLoad := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		if !cancelDuringLoad {
			cancel()
		}
		got, next, err := input.read(ctx, input.initial, 4, 1, func(_ context.Context, hash ContentHash) ([]byte, error) {
			calls++
			cancel()
			return fixture.objects[hash], nil
		})
		cancel()
		if !errors.Is(err, context.Canceled) || next != nil || !reflect.DeepEqual(got, DocDisplayPage{}) || !cancelDuringLoad && calls != 0 {
			test.Fatal("canceled replay returned partial output", cancelDuringLoad, calls, got, next, err)
		}
	}
}

func TestDocMessageDisplayReviewSelectedChildBinding(test *testing.T) {
	fixture := buildMessageDisplayFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"same"},{"type":"text","text":"same"},{"type":"image","name":"same"},{"type":"image","name":"same"}]}`))
	parts := messageDisplayReviewPage(test, fixture, 0).Parts
	var markdown DocDecodedMarkdownPlan
	if err := json.Unmarshal(fixture.objects[parts[0].Plan], &markdown); err != nil {
		test.Fatal(err)
	}
	var placeholder DocMessagePlaceholderPlan
	if err := json.Unmarshal(fixture.objects[parts[2].Plan], &placeholder); err != nil {
		test.Fatal(err)
	}
	foreignMarkdown := markdown
	foreignMarkdown.Input.Source.EventHash = HashContent([]byte("foreign event"))
	foreignPlaceholder := placeholder
	foreignPlaceholder.Source.EventBytes++
	for index, generation := range []*DocDecodedTextGeneration{placeholder.Type, placeholder.Name} {
		if generation == nil {
			continue
		}
		copied := *generation
		copied.Source.EventBytes = foreignPlaceholder.Source.EventBytes
		if index == 0 {
			foreignPlaceholder.Type = &copied
		} else {
			foreignPlaceholder.Name = &copied
		}
	}
	if foreignMarkdown.Validate() != nil || foreignPlaceholder.Validate() != nil {
		test.Fatal("foreign child fixture must remain internally valid")
	}
	for name, scenario := range map[string]struct {
		part  DocMessageDisplayPart
		child any
	}{
		"markdown-source":    {parts[0], foreignMarkdown},
		"markdown-token":     {parts[0], json.RawMessage(fixture.objects[parts[1].Plan])},
		"placeholder-source": {parts[2], foreignPlaceholder},
		"placeholder-block":  {parts[2], json.RawMessage(fixture.objects[parts[3].Plan])},
		"text-as-placeholder": {func() DocMessageDisplayPart {
			part := parts[0]
			part.Kind = DocMessageDisplayPlaceholder
			return part
		}(), placeholder},
		"placeholder-as-text": {func() DocMessageDisplayPart {
			part := parts[2]
			part.Kind = DocMessageDisplayMarkdown
			return part
		}(), markdown},
		"wrong-length": {func() DocMessageDisplayPart {
			part := parts[0]
			part.TextBytes--
			return part
		}(), markdown},
	} {
		test.Run(name, func(test *testing.T) {
			raw := messageDisplayReviewJSON(test, scenario.child)
			part := scenario.part
			part.Plan = HashContent(raw)
			objects := maps.Clone(fixture.objects)
			objects[part.Plan] = raw
			if _, err := readDocMessageDisplayChild(context.Background(), &docMessageInputReader{generation: fixture.plan.Source.Input}, part, true, messageDisplayReviewLoad(objects)); !errors.Is(err, ErrIntegrity) {
				test.Fatal("foreign source, token, kind, or block selected", err)
			}
		})
	}
}

func TestDocMessageDisplayReviewHydratedCursorMetadata(test *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"kind":"message","blocks":[{"type":"text","text":"**abcdefgh**"}]}`),
		[]byte(`{"kind":"message","blocks":[{"type":"image","name":"abcdefgh"}]}`),
	} {
		fixture := buildMessageDisplayFixture(test, raw)
		input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		_, next, err := input.read(context.Background(), input.initial, 4, 1, messageDisplayReviewLoad(fixture.objects))
		if err != nil {
			test.Fatal(err)
		}
		var cursor docMessageDisplayCursor
		if err := json.Unmarshal(next, &cursor); err != nil || len(cursor.Body) == 0 {
			test.Fatal("fixture did not retain a child cursor", err)
		}
		for name, mutate := range map[string]func(*docMessageDisplayCursor){
			"unknown-body": func(value *docMessageDisplayCursor) {
				value.Body = append([]byte(`{"unknown":true,`), value.Body[1:]...)
			},
			"native-plan": func(value *docMessageDisplayCursor) {
				value.Body = bytes.Replace(value.Body, []byte(value.Child), []byte(HashContent([]byte("other child"))), 1)
			},
			"child-size":    func(value *docMessageDisplayCursor) { value.BodyBytes-- },
			"parent-offset": func(value *docMessageDisplayCursor) { value.TextBytes++ },
			"native-metadata": func(value *docMessageDisplayCursor) {
				if value.Kind == DocMessageDisplayMarkdown {
					var body docMappedMarkdownCursor
					if err := json.Unmarshal(value.Body, &body); err != nil {
						test.Fatal(err)
					}
					body.Render.Byte++
					value.Body = messageDisplayReviewJSON(test, body)
				} else {
					var body DocMessagePlaceholderCursor
					if err := json.Unmarshal(value.Body, &body); err != nil {
						test.Fatal(err)
					}
					body.From++
					value.Body = messageDisplayReviewJSON(test, body)
				}
			},
			"render-plan": func(value *docMessageDisplayCursor) {
				if value.Kind != DocMessageDisplayMarkdown {
					value.Kind = DocMessageDisplayMarkdown
					return
				}
				var body docMappedMarkdownCursor
				if err := json.Unmarshal(value.Body, &body); err != nil {
					test.Fatal(err)
				}
				body.Render.Plan = HashContent([]byte("other render plan"))
				value.Body = messageDisplayReviewJSON(test, body)
			},
		} {
			test.Run(string(cursor.Kind)+"/"+name, func(test *testing.T) {
				changed := cursor
				mutate(&changed)
				page, resumed, err := input.read(context.Background(), messageDisplayReviewJSON(test, changed), 4, 1, messageDisplayReviewLoad(fixture.objects))
				if !errors.Is(err, ErrIntegrity) || resumed != nil || !reflect.DeepEqual(page, DocDisplayPage{}) {
					test.Fatal("changed child metadata returned partial output", page, resumed, err)
				}
			})
		}
	}
}

func TestDocMessageDisplayReviewPrefixOnlyContinuation(test *testing.T) {
	for _, first := range []string{`a`, `a\n`, `a\n\n`} {
		raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"` + first + `"},{"type":"text","text":""},{"type":"text","text":"# "},{"type":"text","text":"# **\uD83D\uDE42tail**"},{"type":"image","name":"한글"},{"type":"text","text":null},{"type":"text","text":"end"}]}`)
		fixture := buildMessageDisplayFixture(test, raw)
		input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		baseline, err := BuildDocEventDisplay(context.Background(), raw)
		if err != nil {
			test.Fatal(err)
		}
		assertMappingReviewReplay(test, input, fixture.objects, baseline, 4, 1)
		parts := messageDisplayReviewPage(test, fixture, 0).Parts
		cursor := bytes.Clone(input.initial)
		prefixOnly := 0
		for position := 0; position < input.source.TextBytes; {
			var before docMessageDisplayCursor
			if err := json.Unmarshal(cursor, &before); err != nil {
				test.Fatal(err)
			}
			page, next, err := input.read(context.Background(), cursor, 4, 1, messageDisplayReviewLoad(fixture.objects))
			if err != nil || page.Text == "" {
				test.Fatal("small-budget replay stalled", err)
			}
			if before.Part > 0 && len(before.Body) == 0 {
				part := parts[before.Part]
				var after docMessageDisplayCursor
				if err := json.Unmarshal(next, &after); err != nil {
					test.Fatal(err)
				}
				bodyPosition, err := docMessageDisplayBodyPosition(after)
				span, spanErr := page.SourceRange(0, len(page.Text))
				if err != nil || spanErr != nil || page.Text != strings.Repeat("\n", part.Separator) || after.Part != before.Part || after.Prefix != part.Separator || bodyPosition != 0 || span != part.First {
					test.Fatal("prefix-only page consumed body or changed anchor", before, after, page, span, err, spanErr)
				}
				prefixOnly++
			}
			position += len(page.Text)
			cursor = next
		}
		if prefixOnly != len(parts)-1 {
			test.Fatal("fixture missed separator-only continuations", prefixOnly, len(parts))
		}
	}
}

func TestDocMessageDisplayReviewRejectsForgedSeparatorAnchor(test *testing.T) {
	raw := []byte(`{"kind":"message","blocks":[{"type":"text","text":"left"},{"type":"text","text":"**right**","opaque":"hidden"}]}`)
	fixture := buildMessageDisplayFixture(test, raw)
	page := messageDisplayReviewPage(test, fixture, 0)
	child, err := readDocMessageDisplayChild(context.Background(), &docMessageInputReader{generation: fixture.plan.Source.Input}, page.Parts[1], true, messageDisplayReviewLoad(fixture.objects))
	if err != nil {
		test.Fatal(err)
	}
	for _, marker := range []string{"right", "hidden"} {
		test.Run(marker, func(test *testing.T) {
			changed := page
			changed.Parts = append([]DocMessageDisplayPart(nil), page.Parts...)
			start := bytes.Index(raw, []byte(marker))
			if marker == "right" {
				start++
			}
			changed.Parts[1].First = DocFindSpan{From: start, To: start + 1}
			corrupted := messageDisplayReviewReplacePage(test, fixture, 0, changed)
			input, err := NewDocMessageDisplayAssemblyInput(corrupted.plan)
			if err != nil {
				test.Fatal(err)
			}
			cursor := docMessageDisplayCursor{Version: 1, Plan: input.source.Plan, Part: 1, From: changed.Parts[1].From, TextBytes: changed.Parts[1].From}
			for _, hydrated := range []bool{false, true} {
				name := "unhydrated"
				if hydrated {
					name = "hydrated"
					cursor.Kind, cursor.Child, cursor.BodyBytes = changed.Parts[1].Kind, changed.Parts[1].Plan, changed.Parts[1].TextBytes
					cursor.Separator, cursor.Body = changed.Parts[1].Separator, bytes.Clone(child.Input.initial)
				}
				test.Run(name, func(test *testing.T) {
					got, next, err := input.read(context.Background(), messageDisplayReviewJSON(test, cursor), 4, 1, messageDisplayReviewLoad(corrupted.objects))
					if !errors.Is(err, ErrIntegrity) || next != nil || !reflect.DeepEqual(got, DocDisplayPage{}) {
						test.Fatalf("forged first-rune anchor returned separator provenance: text=%q runs=%v next_bytes=%d err=%v", got.Text, got.Runs, len(next), err)
					}
				})
			}
		})
	}
}

func TestDocMessageDisplayReviewRejectsInvalidChildCursorBeforePrefix(test *testing.T) {
	fixture := buildMessageDisplayFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"left"},{"type":"text","text":"right"}]}`))
	part := messageDisplayReviewPage(test, fixture, 0).Parts[1]
	child, err := readDocMessageDisplayChild(context.Background(), &docMessageInputReader{generation: fixture.plan.Source.Input}, part, true, messageDisplayReviewLoad(fixture.objects))
	if err != nil {
		test.Fatal(err)
	}
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	var body docMappedMarkdownCursor
	if err := json.Unmarshal(child.Input.initial, &body); err != nil {
		test.Fatal(err)
	}
	body.Render.Byte = 1
	cursor := docMessageDisplayCursor{Version: 1, Plan: input.source.Plan, Part: part.Index, From: part.From, TextBytes: part.From, Separator: part.Separator, Kind: part.Kind, Child: part.Plan, BodyBytes: part.TextBytes, Body: messageDisplayReviewJSON(test, body)}
	got, next, err := input.read(context.Background(), messageDisplayReviewJSON(test, cursor), 4, 1, messageDisplayReviewLoad(fixture.objects))
	if !errors.Is(err, ErrIntegrity) || next != nil || !reflect.DeepEqual(got, DocDisplayPage{}) {
		test.Fatalf("invalid native child cursor returned prefix-only output: text=%q runs=%v next_bytes=%d err=%v", got.Text, got.Runs, len(next), err)
	}
}

func TestDocMessageDisplayReviewPageBoundarySourceCovers(test *testing.T) {
	blocks := make([]map[string]string, DocMessageDisplayPageParts+2)
	for index := range blocks {
		text := "block"
		if index == 0 {
			text = strings.Repeat("a", DocDisplayPageTextBytes-1) + "L"
		} else if index == 1 {
			text = "🙂right"
		} else if index == DocMessageDisplayPageParts-1 {
			text = "edge-left"
		} else if index == DocMessageDisplayPageParts {
			text = "edge-right"
		}
		blocks[index] = map[string]string{"type": "text", "text": text}
	}
	raw := messageDisplayReviewJSON(test, map[string]any{"kind": "message", "blocks": blocks})
	fixture := buildMessageDisplayFixture(test, raw)
	if fixture.plan.Parts.Count != 2 {
		test.Fatal("fixture missed the part-page boundary", fixture.plan.Parts.Count)
	}
	input, err := NewDocMessageDisplayAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	baseline, err := BuildDocEventDisplay(context.Background(), raw)
	if err != nil {
		test.Fatal(err)
	}
	assertMappedAssemblyReplay(test, input, fixture.objects, baseline)
	assembled := assembleDisplayFixture(test, input, fixture.objects)
	var previous *DocDisplayPage
	seen := make(map[string]bool)
	for index := 0; index < assembled.generation.Pages.Count; index++ {
		page, err := ReadDocDisplayProjectionPage(context.Background(), assembled.generation, index, messageDisplayReviewLoad(assembled.objects))
		if err != nil {
			test.Fatal(err)
		}
		for offset, character := range page.Text {
			for _, length := range []int{utf8.RuneLen(character), len(page.Text) - offset} {
				got, err := page.SourceRange(offset, length)
				want, baselineErr := baseline.SourceRange(page.From+offset, length)
				if err != nil || baselineErr != nil || got != want {
					test.Fatal("page changed source cover", page.Index, offset, length, got, want, err, baselineErr)
				}
			}
		}
		window, err := BuildDocDisplayProjectionSearchWindow(assembled.generation.Source, previous, page)
		if err != nil {
			test.Fatal(err)
		}
		for _, query := range []string{"L\n\n🙂", "edge-left\n\nedge-right"} {
			matches, _, err := FindDocDisplayWindow(context.Background(), window, query, 0, 10)
			if err != nil {
				test.Fatal(err)
			}
			for _, match := range matches {
				want, err := baseline.SourceRange(match.Offset, match.Length)
				if err != nil || match.SourceRange == nil || *match.SourceRange != want {
					test.Fatal("cross-page or cross-part match changed source cover", query, match, want, err)
				}
				seen[query] = true
			}
		}
		previous = &page
	}
	if len(seen) != 2 {
		test.Fatal("boundary queries did not survive assembly", seen)
	}
}
