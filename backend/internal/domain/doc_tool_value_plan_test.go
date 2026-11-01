package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type docToolValuePlanFixture struct {
	plan    DocToolValuePlan
	pages   []DocToolValuePage
	objects map[ContentHash][]byte
	oracle  *docToolDisplay
}

func (fixture docToolValuePlanFixture) load(_ context.Context, hash ContentHash) ([]byte, error) {
	raw, found := fixture.objects[hash]
	if !found {
		return nil, ErrNotFound
	}
	return bytes.Clone(raw), nil
}

func docToolValuePlanSource(test testing.TB, value string) (DocToolValueSource, []byte) {
	test.Helper()
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+value+`,"opaque":"PRIVATE_BODY_MARKER"}`)
	if err := fixture.scan(test); err != nil {
		test.Fatal(err)
	}
	treeHash, err := fixture.source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	keys := DocToolKeyMapSource{Version: 1, Profile: DocToolKeyMapProfile, Tree: treeHash, Nodes: len(fixture.nodes)}
	orders, err := NewDocToolOrdersSource(fixture.source, keys)
	if err != nil {
		test.Fatal(err)
	}
	source, err := NewDocToolValueSource(fixture.source, keys, orders, fixture.nodes[len(fixture.nodes)-1])
	if err != nil {
		test.Fatal(err)
	}
	return source, fixture.raw
}

func docToolValuePlanPage(source DocToolValueSource, text string) DocToolValuePage {
	hash, _ := source.Hash()
	return DocToolValuePage{Version: 1, Source: hash, Sealed: true, Final: true, Display: DocDisplayPage{Version: 6, Source: hash, Text: text, Runs: []DocDisplayRun{{To: len(text), SourceFrom: source.Tree.Root.From, SourceTo: source.Tree.Root.To}}}}
}

func docToolValuePlanDirectory(test testing.TB, source DocToolValueSource, pages []DocToolValuePage) docToolValuePlanFixture {
	test.Helper()
	fixture := docToolValuePlanFixture{pages: pages, objects: make(map[ContentHash][]byte)}
	hashes := make([]ContentHash, len(pages))
	textBytes := 0
	for index, page := range pages {
		raw, err := page.Encode(source)
		if err != nil {
			test.Fatal(index, err)
		}
		hashes[index] = HashContent(raw)
		fixture.objects[hashes[index]] = raw
		textBytes += len(page.Display.Text)
	}
	var objects []DocRangeObject
	root, err := buildDocRangeDirectory(context.Background(), hashes, &objects)
	if err != nil {
		test.Fatal(err)
	}
	for _, object := range objects {
		fixture.objects[object.Hash] = object.Bytes
	}
	fixture.plan = DocToolValuePlan{Version: 1, Source: source, Pages: root, TextBytes: textBytes}
	if fixture.plan.Validate() != nil {
		test.Fatal("invalid synthetic plan")
	}
	return fixture
}

func newDocToolValuePlanFixture(test testing.TB, value string) docToolValuePlanFixture {
	test.Helper()
	source, raw := docToolValuePlanSource(test, value)
	parser, err := newDocFindSourceSyntaxParser(raw, source.Tree.Root, false)
	if err != nil {
		test.Fatal(err)
	}
	builder := docToolDisplayBuilder{ctx: context.Background(), parser: parser, display: docToolDisplay{sourceBytes: len(raw)}, metadata: docToolDisplayBaseBytes, budget: MaxDocToolDisplayBytes}
	if err := builder.pretty(source.Tree.Root, 0); err != nil {
		test.Fatal(err)
	}
	builder.display.text = builder.text.String()
	oracle := &builder.display
	var pages []DocToolValuePage
	page := docToolValuePlanPage(source, "")
	page.Display.Runs = nil
	runes := 0
	for offset, character := range oracle.Text() {
		width := utf8.RuneLen(character)
		span, err := oracle.SourceRange(offset, width)
		if err != nil {
			test.Fatal(err)
		}
		page.Display.Text += oracle.Text()[offset : offset+width]
		run := DocDisplayRun{To: len(page.Display.Text), SourceFrom: span.From, SourceTo: span.To, Linear: span.To-span.From == width}
		if len(page.Display.Runs) > 0 && run.Linear && page.Display.Runs[len(page.Display.Runs)-1].Linear && page.Display.Runs[len(page.Display.Runs)-1].SourceTo == run.SourceFrom {
			last := &page.Display.Runs[len(page.Display.Runs)-1]
			last.To, last.SourceTo = run.To, run.SourceTo
		} else {
			page.Display.Runs = append(page.Display.Runs, run)
		}
		runes++
		if runes == DocToolValuePageMinRunes || offset+width == len(oracle.Text()) {
			page.Final = offset+width == len(oracle.Text())
			pages = append(pages, page)
			page = docToolValuePlanPage(source, "")
			page.Display.Runs = nil
			page.Display.Index, page.Display.From = len(pages), offset+width
			runes = 0
		}
	}
	fixture := docToolValuePlanDirectory(test, source, pages)
	fixture.oracle = oracle
	return fixture
}

func TestDocToolValueSourceCanonicalIdentity(test *testing.T) {
	source, _ := docToolValuePlanSource(test, "{\"nested\":[true,\"\ud55c\uae00🙂\",null]}")
	raw, err := source.Encode()
	hash, hashErr := source.Hash()
	decoded, decodeErr := DecodeDocToolValueSource(hash, raw)
	if err != nil || hashErr != nil || decodeErr != nil || !reflect.DeepEqual(source, decoded) || hash != HashContent(raw) || len(raw) > MaxDocDisplaySourceBytes {
		test.Fatal("source canonical round trip failed", err, hashErr, decodeErr)
	}
	for name, change := range map[string]func(*DocToolValueSource){
		"version":       func(source *DocToolValueSource) { source.Version++ },
		"profile":       func(source *DocToolValueSource) { source.Profile = DocToolStringProfile },
		"event":         func(source *DocToolValueSource) { source.Tree.EventHash = HashContent(nil) },
		"evidence":      func(source *DocToolValueSource) { source.Tree.Evidence = HashContent(nil) },
		"key-tree":      func(source *DocToolValueSource) { source.Keys.Tree = HashContent(nil) },
		"key-count":     func(source *DocToolValueSource) { source.Keys.Nodes++ },
		"order-tree":    func(source *DocToolValueSource) { source.Orders.Tree = HashContent(nil) },
		"order-keys":    func(source *DocToolValueSource) { source.Orders.Keys = HashContent(nil) },
		"order-count":   func(source *DocToolValueSource) { source.Orders.Nodes++ },
		"root-parent":   func(source *DocToolValueSource) { source.Root.Parent = 0 },
		"root-depth":    func(source *DocToolValueSource) { source.Root.Depth++ },
		"root-ordinal":  func(source *DocToolValueSource) { source.Root.Ordinal++ },
		"root-key":      func(source *DocToolValueSource) { source.Root.Key = &DocFindSpan{From: 0, To: 2} },
		"root-start":    func(source *DocToolValueSource) { source.Root.Value.From++ },
		"root-end":      func(source *DocToolValueSource) { source.Root.Value.To-- },
		"root-type":     func(source *DocToolValueSource) { source.Root.Type = DocToolValueArray },
		"root-children": func(source *DocToolValueSource) { source.Root.Children = math.MaxInt },
	} {
		test.Run(name, func(test *testing.T) {
			invalid := source
			change(&invalid)
			encoded, err := invalid.Encode()
			if !errors.Is(invalid.Validate(), ErrIntegrity) || !errors.Is(err, ErrIntegrity) || encoded != nil {
				test.Fatal("foreign or noncanonical identity accepted", err)
			}
		})
	}
	root := source.Root
	captured, err := NewDocToolValueSource(source.Tree, source.Keys, source.Orders, root)
	root.Value.To--
	if err != nil || !reflect.DeepEqual(captured, source) {
		test.Fatal("source retained caller-owned root", err)
	}
}

func TestDocToolValuePageStagingSealedAndBoundaries(test *testing.T) {
	source, _ := docToolValuePlanSource(test, `[]`)
	for _, scenario := range []struct {
		name   string
		text   string
		sealed bool
		final  bool
		valid  bool
	}{
		{"pending-short", "x", false, false, true},
		{"pending-empty", "", false, false, false},
		{"unsealed-final", "x", false, true, false},
		{"sealed-short", "x", true, false, false},
		{"sealed-before-min", strings.Repeat("x", 255), true, false, false},
		{"sealed-at-min", strings.Repeat("x", 256), true, false, true},
		{"sealed-short-multibyte", strings.Repeat("🙂", 255), true, false, false},
		{"sealed-min-multibyte", strings.Repeat("🙂", 256), true, false, true},
		{"final-short", "x", true, true, true},
		{"maximum", strings.Repeat("x", DocDisplayPageTextBytes), true, false, true},
		{"too-large", strings.Repeat("x", DocDisplayPageTextBytes+1), false, false, false},
		{"invalid-utf8", string([]byte{0xff}), true, true, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			page := docToolValuePlanPage(source, scenario.text)
			page.Sealed, page.Final = scenario.sealed, scenario.final
			raw, err := page.Encode(source)
			if (err == nil) != scenario.valid {
				test.Fatal("incorrect staging/sealing bound", err)
			}
			if scenario.valid {
				decoded, err := DecodeDocToolValuePage(source, HashContent(raw), raw)
				if err != nil || !reflect.DeepEqual(decoded, page) {
					test.Fatal("page round trip failed", err)
				}
			}
		})
	}
	valid := docToolValuePlanPage(source, "\ud55c🙂x")
	for name, change := range map[string]func(*DocToolValuePage){
		"version":        func(page *DocToolValuePage) { page.Version++ },
		"old-page-codec": func(page *DocToolValuePage) { page.Display.Version = 5 },
		"wrong-source":   func(page *DocToolValuePage) { page.Source = HashContent(nil) },
		"inner-source":   func(page *DocToolValuePage) { page.Display.Source = HashContent(nil) },
		"negative-index": func(page *DocToolValuePage) { page.Display.Index = -1 },
		"large-index":    func(page *DocToolValuePage) { page.Display.Index = MaxDocRangePages },
		"nonzero-first":  func(page *DocToolValuePage) { page.Display.From = 1 },
		"short-prefix":   func(page *DocToolValuePage) { page.Display.Index, page.Display.From = 1, 255 },
		"large-prefix":   func(page *DocToolValuePage) { page.Display.Index, page.Display.From = 1, DocDisplayPageTextBytes+1 },
		"negative-from":  func(page *DocToolValuePage) { page.Display.From = -1 },
		"overflow-from":  func(page *DocToolValuePage) { page.Display.From = math.MaxInt },
		"no-runs":        func(page *DocToolValuePage) { page.Display.Runs = nil },
		"missing-tail":   func(page *DocToolValuePage) { page.Display.Runs[0].To-- },
		"utf8-run-end": func(page *DocToolValuePage) {
			page.Display.Runs = []DocDisplayRun{{To: 1, SourceFrom: 0, SourceTo: 1}, {To: len(page.Display.Text), SourceFrom: 0, SourceTo: 1}}
		},
		"negative-source": func(page *DocToolValuePage) { page.Display.Runs[0].SourceFrom = -1 },
		"empty-source":    func(page *DocToolValuePage) { page.Display.Runs[0].SourceTo = page.Display.Runs[0].SourceFrom },
		"past-event":      func(page *DocToolValuePage) { page.Display.Runs[0].SourceTo = source.Tree.EventBytes + 1 },
		"linear-width":    func(page *DocToolValuePage) { page.Display.Runs[0].Linear = true },
		"before-root": func(page *DocToolValuePage) {
			page.Display.Runs[0].SourceFrom, page.Display.Runs[0].SourceTo = source.Root.Value.From-1, source.Root.Value.From
		},
		"after-root": func(page *DocToolValuePage) {
			page.Display.Runs[0].SourceFrom, page.Display.Runs[0].SourceTo = source.Root.Value.To, source.Root.Value.To+1
		},
		"cross-root-start": func(page *DocToolValuePage) { page.Display.Runs[0].SourceFrom = source.Root.Value.From - 1 },
		"cross-root-end":   func(page *DocToolValuePage) { page.Display.Runs[0].SourceTo = source.Root.Value.To + 1 },
	} {
		test.Run(name, func(test *testing.T) {
			page := valid
			page.Display.Runs = append([]DocDisplayRun(nil), valid.Display.Runs...)
			change(&page)
			if !errors.Is(page.Validate(source), ErrIntegrity) {
				test.Fatal("malformed position or mapping accepted")
			}
		})
	}
	for _, from := range []int{256, DocDisplayPageTextBytes} {
		page := valid
		page.Display.Index, page.Display.From = 1, from
		if page.Validate(source) != nil {
			test.Fatal("valid prior-page bound rejected", from)
		}
	}
	tooMany := docToolValuePlanPage(source, strings.Repeat("x", DocDisplayPageRuns+1))
	tooMany.Display.Runs = nil
	for index := 0; index <= DocDisplayPageRuns; index++ {
		tooMany.Display.Runs = append(tooMany.Display.Runs, DocDisplayRun{To: index + 1, SourceFrom: source.Root.Value.From, SourceTo: source.Root.Value.From + 1})
	}
	if tooMany.Validate(source) == nil {
		test.Fatal("too many mapped runs accepted")
	}
	expanded := docToolValuePlanPage(source, strings.Repeat("\x00", DocDisplayPageTextBytes))
	if raw, err := expanded.Encode(source); !errors.Is(err, ErrIntegrity) || raw != nil {
		test.Fatal("encoded-page byte bound ignored", err)
	}
}

func TestDocToolValueCodecsRejectNoncanonicalCorruption(test *testing.T) {
	fixture := newDocToolValuePlanFixture(test, `{"2":1,"1":2,"z":{"x":"\uD800🙂"},"2":3}`)
	source, page := fixture.plan.Source, fixture.pages[0]
	sourceRaw, _ := source.Encode()
	pageRaw, _ := page.Encode(source)
	planRaw, _ := fixture.plan.Encode()
	for _, codec := range []struct {
		name   string
		raw    []byte
		limit  int
		decode func(ContentHash, []byte) error
	}{
		{"source", sourceRaw, MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) error { _, err := DecodeDocToolValueSource(hash, raw); return err }},
		{"page", pageRaw, MaxDocDisplayPageBytes, func(hash ContentHash, raw []byte) error {
			_, err := DecodeDocToolValuePage(source, hash, raw)
			return err
		}},
		{"plan", planRaw, MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) error { _, err := DecodeDocToolValuePlan(hash, raw); return err }},
	} {
		test.Run(codec.name, func(test *testing.T) {
			if codec.decode(HashContent(codec.raw), codec.raw) != nil {
				test.Fatal("canonical codec failed")
			}
			for _, raw := range [][]byte{nil, []byte("{"), append(bytes.Clone(codec.raw), ' '), append([]byte(`{"unknown":1,`), codec.raw[1:]...), append([]byte(`{"version":1,`), codec.raw[1:]...), bytes.Repeat([]byte(" "), codec.limit+1)} {
				if !errors.Is(codec.decode(HashContent(raw), raw), ErrIntegrity) {
					test.Fatal("noncanonical or oversized object accepted")
				}
			}
			for _, hash := range []ContentHash{"", "bad", HashContent(nil)} {
				if !errors.Is(codec.decode(hash, codec.raw), ErrIntegrity) {
					test.Fatal("wrong content hash accepted")
				}
			}
		})
	}
	if _, err := page.Display.SourceRange(0, 1); !errors.Is(err, ErrIntegrity) {
		test.Fatal("version-6 page reinterpreted by old display codec", err)
	}
	for _, version := range []int{1, 2, 3, 4, 5, 7} {
		invalid := page
		invalid.Display.Version = version
		if _, err := invalid.SourceRange(0, 1); !errors.Is(err, ErrIntegrity) {
			test.Fatal("value wrapper accepted an old display format", version)
		}
	}
	unicode := docToolValuePlanPage(source, "\ud55c🙂x")
	for _, location := range [][2]int{{-1, 1}, {0, 0}, {0, -1}, {1, 1}, {0, 1}, {0, math.MaxInt}, {math.MaxInt, 1}} {
		if _, err := unicode.SourceRange(location[0], location[1]); !errors.Is(err, ErrValidation) {
			test.Fatal("invalid UTF-8 source lookup accepted", location, err)
		}
	}
	unicode.Source = HashContent(nil)
	if _, err := unicode.SourceRange(0, 3); !errors.Is(err, ErrIntegrity) {
		test.Fatal("source lookup ignored mismatched inner and outer identities", err)
	}
}

func TestDocToolValuePlanCountAndByteBounds(test *testing.T) {
	source, _ := docToolValuePlanSource(test, `[]`)
	for _, scenario := range []struct {
		count int
		text  int
		valid bool
	}{
		{0, 0, false}, {1, 0, false}, {1, 1, true}, {1, DocDisplayPageTextBytes, true}, {1, DocDisplayPageTextBytes + 1, false},
		{2, 256, false}, {2, 257, true}, {2, 2 * DocDisplayPageTextBytes, true}, {2, 2*DocDisplayPageTextBytes + 1, false},
		{MaxDocRangePages + 1, 1, false}, {1, MaxConversationDocumentBytes + 1, false}, {math.MaxInt, math.MaxInt, false}, {-1, -1, false},
		{MaxConversationDocumentBytes / DocDisplayPageTextBytes, MaxConversationDocumentBytes, true},
	} {
		plan := DocToolValuePlan{Version: 1, Source: source, Pages: DocRangeDirectoryRoot{Hash: HashContent(nil), Count: scenario.count}, TextBytes: scenario.text}
		raw, err := plan.Encode()
		if (err == nil) != scenario.valid {
			test.Fatal("incorrect plan bound", scenario, err)
		}
		if scenario.valid {
			decoded, err := DecodeDocToolValuePlan(HashContent(raw), raw)
			if err != nil || !reflect.DeepEqual(decoded, plan) || len(raw) > MaxDocDisplaySourceBytes {
				test.Fatal("plan round trip failed", err)
			}
		}
	}
}

func TestDocToolValueReadRejectsStagingAndInconsistentFinals(test *testing.T) {
	source, _ := docToolValuePlanSource(test, `[]`)
	valid := docToolValuePlanPage(source, strings.Repeat("x", 256))
	for name, change := range map[string]func(*DocToolValuePage, *DocToolValuePlan){
		"staging":       func(page *DocToolValuePage, _ *DocToolValuePlan) { page.Sealed, page.Final = false, false },
		"missing-final": func(page *DocToolValuePage, _ *DocToolValuePlan) { page.Final = false },
		"early-final":   func(_ *DocToolValuePage, plan *DocToolValuePlan) { plan.Pages.Count, plan.TextBytes = 2, 257 },
		"wrong-index": func(page *DocToolValuePage, plan *DocToolValuePlan) {
			page.Display.Index, page.Display.From, plan.TextBytes = 1, 256, 512
		},
		"wrong-end":    func(_ *DocToolValuePage, plan *DocToolValuePlan) { plan.TextBytes++ },
		"overflow-end": func(_ *DocToolValuePage, plan *DocToolValuePlan) { plan.TextBytes-- },
		"wrong-source": func(page *DocToolValuePage, _ *DocToolValuePlan) {
			page.Source, page.Display.Source = HashContent(nil), HashContent(nil)
		},
		"wrong-display": func(page *DocToolValuePage, _ *DocToolValuePlan) { page.Display.Version = 5 },
		"outside-root": func(page *DocToolValuePage, _ *DocToolValuePlan) {
			page.Display.Runs[0].SourceFrom, page.Display.Runs[0].SourceTo = source.Root.Value.To, source.Root.Value.To+1
		},
		"impossible-suffix": func(page *DocToolValuePage, plan *DocToolValuePlan) {
			page.Final, plan.Pages.Count, plan.TextBytes = false, 3, 513
			page.Display.Text += "x"
			page.Display.Runs[0].To++
		},
	} {
		test.Run(name, func(test *testing.T) {
			page := valid
			page.Display.Runs = append([]DocDisplayRun(nil), valid.Display.Runs...)
			plan := DocToolValuePlan{Version: 1, Source: source, Pages: DocRangeDirectoryRoot{Hash: HashContent(nil), Count: 1}, TextBytes: len(valid.Display.Text)}
			change(&page, &plan)
			raw, _ := canonicalJSON(page)
			hash := HashContent(raw)
			hashes := make([]ContentHash, plan.Pages.Count)
			for index := range hashes {
				hashes[index] = hash
			}
			var objects []DocRangeObject
			plan.Pages, _ = buildDocRangeDirectory(context.Background(), hashes, &objects)
			load := func(_ context.Context, selected ContentHash) ([]byte, error) {
				if selected == hash {
					return raw, nil
				}
				for _, object := range objects {
					if object.Hash == selected {
						return object.Bytes, nil
					}
				}
				return nil, ErrNotFound
			}
			result, err := ReadDocToolValuePage(context.Background(), plan, 0, load)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(result, DocToolValuePage{}) {
				test.Fatal("inconsistent page returned data", err)
			}
		})
	}
}

func TestDocToolValueLateReadOnlySelectedDirectoryPath(test *testing.T) {
	source, _ := docToolValuePlanSource(test, `[]`)
	for _, count := range []int{1, DocRangeDirectoryFanout + 1, DocRangeDirectoryFanout*DocRangeDirectoryFanout + 1} {
		hashes := make([]ContentHash, count)
		for index := range hashes {
			hashes[index] = HashContent([]byte(fmt.Sprintf("unavailable-prefix-%d", index)))
		}
		page := docToolValuePlanPage(source, "🙂")
		page.Display.Index, page.Display.From = count-1, (count-1)*DocToolValuePageMinRunes
		raw, err := page.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
		hashes[count-1] = HashContent(raw)
		var objects []DocRangeObject
		root, err := buildDocRangeDirectory(context.Background(), hashes, &objects)
		if err != nil {
			test.Fatal(err)
		}
		stored := map[ContentHash][]byte{hashes[count-1]: raw}
		for _, object := range objects {
			stored[object.Hash] = object.Bytes
		}
		plan := DocToolValuePlan{Version: 1, Source: source, Pages: root, TextBytes: page.Display.From + len(page.Display.Text)}
		loads, readBytes := 0, 0
		selected, err := ReadDocToolValuePage(context.Background(), plan, count-1, func(_ context.Context, hash ContentHash) ([]byte, error) {
			loads++
			body, found := stored[hash]
			if !found {
				test.Fatal("read raw event, metadata, or an unavailable prefix page", hash)
			}
			readBytes += len(body)
			return body, nil
		})
		level, _ := docRangeDirectoryLevel(count)
		if err != nil || !reflect.DeepEqual(selected, page) || loads != level+2 || readBytes > MaxDocDisplayPageBytes+(level+1)*MaxDocRangeDirectoryBytes {
			test.Fatal("late-page cost escaped selected path", count, loads, readBytes, err)
		}
		test.Logf("pages=%d selected_loads=%d selected_bytes=%d", count, loads, readBytes)
	}
}

func TestDocToolValueSharedAssemblyRestartAndSourceRangeParity(test *testing.T) {
	for _, value := range []string{
		`0`, `-0`, `1e99999`, `[]`, `{}`, `null`, `true`, `false`, `""`, "\"\ud55c\uae00🙂\\uD800\\n\"",
		`{"10":"first","2":[true,{"x":"\ud83d\ude42\uD800\n\t"}],"10":"last","z":1.2345678901234567e-8}`,
		`{"2":"` + strings.Repeat(`PUBLIC_BODY_MARKER\uD800\n🙂\"`, 180) + `","1":[1e1000,false,null]}`,
	} {
		fixture := newDocToolValuePlanFixture(test, value)
		input, err := NewDocToolValueAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		projection, err := fixture.plan.ProjectionSource()
		hash, _ := fixture.plan.Hash()
		if err != nil || projection != input.Source() || projection.Plan != hash || projection.EventHash != fixture.plan.Source.Tree.EventHash || projection.EventBytes != fixture.plan.Source.Tree.EventBytes || projection.TextBytes != fixture.plan.TextBytes {
			test.Fatal("projection lost full source identity", err)
		}
		for _, page := range fixture.pages {
			for offset, character := range page.Display.Text {
				span, err := page.SourceRange(offset, utf8.RuneLen(character))
				want, failure := fixture.oracle.SourceRange(page.Display.From+offset, utf8.RuneLen(character))
				if err != nil || failure != nil || span != want {
					test.Fatal("native value mapping changed", span, want, err, failure)
				}
			}
		}
		for _, limits := range [][2]int{{4, 1}, {37, 7}, {DocDisplayPageTextBytes, DocDisplayPageRuns}} {
			assertMappingReviewReplay(test, input, fixture.objects, fixture.oracle, limits[0], limits[1])
		}
		assembled := assembleDisplayFixture(test, input, fixture.objects)
		actual := assembledFixturePages(test, assembled, func(page DocDisplayPage) {
			for offset, character := range page.Text {
				for _, length := range []int{utf8.RuneLen(character), len(page.Text) - offset} {
					got, err := page.SourceRange(offset, length)
					want, failure := fixture.oracle.SourceRange(page.From+offset, length)
					if err != nil || failure != nil || got != want {
						test.Fatal("generic restarted assembly changed source coordinates", got, want, err, failure)
					}
				}
			}
		})
		if actual != fixture.oracle.Text() || assembled.maxState > MaxDocDisplayStateBytes || len(fixture.pages) > 2 && assembled.steps < 2 {
			test.Fatal("generic restarted assembly changed text or failed to restart")
		}
	}
}

func TestDocToolValueReceiptRejectsInactiveSelectedChildTransplants(test *testing.T) {
	for _, kind := range []string{"array-value", "object-key"} {
		for _, sealed := range []bool{false, true} {
			for _, whitespace := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/sealed=%t/whitespace=%t", kind, sealed, whitespace), func(test *testing.T) {
					var value strings.Builder
					phase, separator := "value", ","
					if whitespace {
						separator = " , "
					}
					if kind == "object-key" {
						value.WriteString(`{"a":[],"b":[]`)
						phase = "key"
					} else {
						value.WriteString("[0")
					}
					for index := 1; index <= 100; index++ {
						number := index
						if sealed {
							number += 1000000000
						}
						value.WriteString(separator)
						if kind == "object-key" {
							fmt.Fprintf(&value, `"k%d":%d`, index, number)
						} else {
							fmt.Fprint(&value, number)
						}
					}
					if kind == "object-key" {
						value.WriteString("}")
					} else {
						value.WriteString("]")
					}
					fixture := newDocToolValueFixture(test, value.String())
					fixture.compile(test)
					for _, saved := range fixture.states {
						if saved.Phase != phase || saved.Active != "" || saved.Frame == nil || saved.Frame.Child == nil || (saved.Pending == "") != sealed {
							continue
						}
						plan := fixture.plans[saved.Frame.Node.Value.From]
						if saved.Frame.Next+1 >= plan.Source.Count {
							continue
						}
						page, err := ReadDocToolOrderPage(context.Background(), fixture.source.Tree, fixture.source.Keys, plan, (saved.Frame.Next+1)/DocToolOrderPageEntries, fixture.order.keys.tree.load)
						if err != nil {
							test.Fatal(err)
						}
						forged := cloneDocToolValueState(saved)
						skipped := *saved.Frame.Child
						replacement := cloneDocToolTreeNode(page.Entries[(saved.Frame.Next+1)%DocToolOrderPageEntries].Node)
						forged.Frame.Next++
						forged.Frame.Previous, forged.Frame.Child = skipped.Value.To, &replacement
						if phase == "value" {
							selected := cloneDocToolTreeNode(replacement)
							forged.Value = &selected
						}
						if saved.Receipt == "" || forged.Receipt != saved.Receipt || forged.Validate(fixture.source) != nil {
							test.Fatal("probe did not preserve a structurally valid receipted selection")
						}
						encoded, err := forged.Encode(fixture.source)
						if err != nil {
							test.Fatal(err)
						}
						forged, err = DecodeDocToolValueState(fixture.source, HashContent(encoded), encoded)
						if err != nil {
							test.Fatal("forged control state did not exercise checkpoint receipt validation", err)
						}
						rawReads, orderReads, objectReads := 0, 0, 0
						step, err := StepDocToolValue(context.Background(), fixture.source, forged, func(context.Context, int, int) ([]byte, error) {
							rawReads++
							return nil, ErrNotFound
						}, func(ctx context.Context, hash ContentHash) ([]byte, error) {
							objectReads++
							if hash != saved.Receipt {
								test.Fatal("forged selection loaded dependencies before rejecting its receipt")
							}
							return fixture.order.keys.tree.load(ctx, hash)
						}, func(context.Context, int) (DocToolOrderPlan, error) {
							orderReads++
							return DocToolOrderPlan{}, ErrNotFound
						})
						if !errors.Is(err, ErrIntegrity) || rawReads != 0 || orderReads != 0 || objectReads != 1 || !reflect.DeepEqual(step, DocToolValueStep{}) {
							test.Fatal("inactive selected child bypassed its receipt or returned partial work", err, rawReads, orderReads, objectReads)
						}
						unchanged, err := forged.Encode(fixture.source)
						if err != nil || !bytes.Equal(unchanged, encoded) {
							test.Fatal("rejected checkpoint mutated its caller", err)
						}
						return
					}
					test.Fatal("fixture never reached the intended inactive selection and pending-page boundary")
				})
			}
		}
	}
}

func TestDocToolValueReceiptBindsWholeControlStateWithoutSourceCopies(test *testing.T) {
	fixture := newDocToolValueFixture(test, `{"long":"`+strings.Repeat("PUBLIC_BODY_MARKER\ud55c🙂\\n\\ud800", 1800)+`"}`)
	fixture.compile(test)
	var saved DocToolValueState
	for _, state := range fixture.states {
		if state.Receipt != "" && state.Frame != nil && state.Frame.Child != nil && state.Value != nil && state.Active != "" {
			saved = cloneDocToolValueState(state)
			break
		}
	}
	if saved.Receipt == "" {
		test.Fatal("fixture never suspended an active scalar")
	}
	raw, err := fixture.order.keys.tree.load(context.Background(), saved.Receipt)
	var receipt docToolValueReceipt
	digest, digestErr := saved.controlDigest()
	if err != nil || digestErr != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Version != 1 || receipt.Source != saved.Source || receipt.Digest != digest || HashContent(raw) != saved.Receipt || len(raw) > 512 || bytes.Contains(raw, []byte("PUBLIC_BODY_MARKER")) {
		test.Fatal("receipt did not bind the complete source-scoped control state compactly", err, digestErr, len(raw))
	}
	for name, change := range map[string]func(*DocToolValueState){
		"version":      func(state *DocToolValueState) { state.Version++ },
		"source":       func(state *DocToolValueState) { state.Source = HashContent(nil) },
		"phase":        func(state *DocToolValueState) { state.Phase = "value" },
		"value":        func(state *DocToolValueState) { state.Value.Value.To-- },
		"frame-node":   func(state *DocToolValueState) { state.Frame.Node.Children++ },
		"frame-plan":   func(state *DocToolValueState) { state.Frame.Plan = HashContent(nil) },
		"frame-parent": func(state *DocToolValueState) { state.Frame.Parent = HashContent(nil) },
		"frame-next":   func(state *DocToolValueState) { state.Frame.Next++ },
		"frame-prior":  func(state *DocToolValueState) { state.Frame.Previous++ },
		"frame-child":  func(state *DocToolValueState) { state.Frame.Child.Value.To-- },
		"active":       func(state *DocToolValueState) { state.Active = HashContent(nil) },
		"start":        func(state *DocToolValueState) { state.Start++ },
		"offset":       func(state *DocToolValueState) { state.Offset++ },
		"text":         func(state *DocToolValueState) { state.TextBytes++ },
		"pending":      func(state *DocToolValueState) { state.Pending = HashContent(nil) },
		"page-count":   func(state *DocToolValueState) { state.Directory.Count++ },
		"frontier": func(state *DocToolValueState) {
			state.Directory.Levels = append(state.Directory.Levels, []ContentHash{HashContent(nil)})
		},
	} {
		test.Run(name, func(test *testing.T) {
			changed := cloneDocToolValueState(saved)
			change(&changed)
			actual, err := changed.controlDigest()
			if err != nil || actual == digest {
				test.Fatal("receipt omitted control-state content", name, err)
			}
		})
	}
	changed := cloneDocToolValueState(saved)
	changed.Receipt = HashContent(nil)
	if actual, err := changed.controlDigest(); err != nil || actual != digest {
		test.Fatal("receipt hash entered its own digest", err)
	}
	initial, err := NewDocToolValueState(fixture.source)
	if err != nil || initial.Receipt != "" || initial.Validate(fixture.source) != nil {
		test.Fatal("exact initial state requires a receipt", err)
	}
	changed = cloneDocToolValueState(saved)
	changed.Receipt = ""
	if err := changed.Validate(fixture.source); !errors.Is(err, ErrIntegrity) {
		test.Fatal("advanced state discarded its receipt", err)
	}
	for _, state := range fixture.states {
		if state.Receipt == "" || state.Receipt == saved.Receipt {
			continue
		}
		changed = cloneDocToolValueState(saved)
		changed.Receipt = state.Receipt
		rawReads := 0
		step, err := StepDocToolValue(context.Background(), fixture.source, changed, func(context.Context, int, int) ([]byte, error) {
			rawReads++
			return nil, ErrNotFound
		}, fixture.order.keys.tree.load, fixture.loadOrder)
		if !errors.Is(err, ErrIntegrity) || rawReads != 0 || !reflect.DeepEqual(step, DocToolValueStep{}) {
			test.Fatal("receipt from another cursor was replayed", err, rawReads)
		}
		return
	}
	test.Fatal("fixture did not produce distinct receipts")
}

func TestDocToolValueFrameOperationBudgetIncludesMetadataBodies(test *testing.T) {
	fixture := newDocToolValueFixture(test, strings.Repeat("[", 96)+"0"+strings.Repeat("]", 96))
	fixture.compile(test)
	maxOperations, maxReads := 0, 0
	for _, prior := range fixture.states[:len(fixture.states)-1] {
		frameReads := 0
		step, err := StepDocToolValue(context.Background(), fixture.source, prior, fixture.order.keys.tree.read, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			raw, err := fixture.order.keys.tree.load(ctx, hash)
			if _, decodeErr := decodeDocToolValueFrame(fixture.source, hash, raw); decodeErr == nil {
				frameReads++
			}
			return raw, err
		}, fixture.loadOrder)
		if err != nil {
			test.Fatal(err)
		}
		frameWrites := 0
		for _, object := range step.Objects() {
			if _, err := decodeDocToolValueFrame(fixture.source, object.Hash, object.Bytes); err == nil {
				frameWrites++
			}
		}
		if step.FrameOperations() > DocToolTreeStepFrames || frameReads+frameWrites > step.FrameOperations() || step.State().TextBytes-prior.TextBytes > DocToolValueStepOutputBytes {
			test.Fatal("frame-body I/O bypassed the combined operation bound", frameReads, frameWrites, step.FrameOperations())
		}
		maxOperations, maxReads = max(maxOperations, step.FrameOperations()), max(maxReads, frameReads)
	}
	if maxOperations != DocToolTreeStepFrames || maxReads == 0 {
		test.Fatal("fixture did not exercise bounded ancestor validation and unwinding", maxOperations, maxReads)
	}
	test.Logf("max frame operations=%d max physical frame-body loads=%d", maxOperations, maxReads)
}

func TestDocToolValueCompiledAssemblyRestartAndProvenanceWithoutRawReads(test *testing.T) {
	for name, value := range map[string]string{
		"nested": `{"10":"first","2":[` + strings.TrimSuffix(strings.Repeat("{\"x\":[\"\ud55c🙂\\ud83d\\ude42\\ud800\\n\\t\\\"\\\\\",true,null],\"n\":9007199254740993},", 36), ",") + "],\"10\":\"last\",\"z\":{\"\u6df1\":[[],{},false,1e1000]}}",
		"long":   `{"outer":{"payload":"` + strings.Repeat("PUBLIC_BODY_MARKER\ud55c🙂\\uD83D\\uDE42\\uD800\\n\\t\\\"\\\\", 1800) + "\",\"tail\":[\"\ub05d\",false,1e1000]}}",
	} {
		test.Run(name, func(test *testing.T) {
			compiled := newDocToolValueFixture(test, value)
			plan := compiled.compile(test)
			wantText, preparedPages := readDocToolValueTestPages(test, compiled, plan)
			if len(preparedPages) < 2 || len(compiled.steps) < 2 {
				test.Fatal("actual compiler fixture did not exercise restarted multi-page work")
			}
			raw, err := plan.Encode()
			if err != nil {
				test.Fatal(err)
			}
			plan, err = DecodeDocToolValuePlan(HashContent(raw), raw)
			if err != nil {
				test.Fatal("compiled plan failed restart", err)
			}
			preparedObjects := make(map[ContentHash][]byte)
			for index, expected := range preparedPages {
				page, err := ReadDocToolValuePage(context.Background(), plan, index, func(ctx context.Context, hash ContentHash) ([]byte, error) {
					if hash == compiled.source.Tree.EventHash {
						test.Fatal("prepared page lookup requested the raw event")
					}
					body, err := compiled.order.keys.tree.load(ctx, hash)
					if err == nil {
						preparedObjects[hash] = bytes.Clone(body)
					}
					return body, err
				})
				if err != nil || !reflect.DeepEqual(page, expected) {
					test.Fatal("compiled mapped page changed during restart", index, err)
				}
			}
			if _, exists := preparedObjects[compiled.source.Tree.EventHash]; exists {
				test.Fatal("raw event escaped into assembly input objects")
			}
			input, err := NewDocToolValueAssemblyInput(plan)
			if err != nil {
				test.Fatal(err)
			}
			assembled := assembleDisplayFixture(test, input, preparedObjects)
			expectedRange := func(from, length int) DocFindSpan {
				span := DocFindSpan{From: math.MaxInt}
				covered := 0
				for _, page := range preparedPages {
					start := max(from, page.Display.From)
					end := min(from+length, page.Display.From+len(page.Display.Text))
					if start >= end {
						continue
					}
					part, err := page.SourceRange(start-page.Display.From, end-start)
					if err != nil {
						test.Fatal("compiled page source lookup failed", err)
					}
					span.From, span.To = min(span.From, part.From), max(span.To, part.To)
					covered += end - start
				}
				if covered != length {
					test.Fatal("compiled mapping has a gap", from, length, covered)
				}
				return span
			}
			preparedIndex := 0
			gotText := assembledFixturePages(test, assembled, func(page DocDisplayPage) {
				got, err := page.SourceRange(0, len(page.Text))
				if want := expectedRange(page.From, len(page.Text)); err != nil || got != want {
					test.Fatal("assembled cross-page source range changed", got, want, err)
				}
				for offset, character := range page.Text {
					position := page.From + offset
					for preparedIndex+1 < len(preparedPages) && position >= preparedPages[preparedIndex+1].Display.From {
						preparedIndex++
					}
					prepared := preparedPages[preparedIndex]
					width := utf8.RuneLen(character)
					want, expectedErr := prepared.SourceRange(position-prepared.Display.From, width)
					got, err := page.SourceRange(offset, width)
					if err != nil || expectedErr != nil || got != want {
						test.Fatal("actual compiled assembly changed rune provenance", position, got, want, err, expectedErr)
					}
				}
			})
			if gotText != wantText || assembled.steps < 2 || assembled.generation.Pages.Count < 2 || assembled.generation.Source != input.Source() {
				test.Fatal("actual compiled assembly changed text, identity, or restart coverage")
			}
			test.Logf("compiled_steps=%d value_pages=%d assembly_steps=%d projection_pages=%d max_assembly_loads=%d", len(compiled.steps), len(preparedPages), assembled.steps, assembled.generation.Pages.Count, assembled.maxLoads)
		})
	}
}

func TestDocToolValueAssemblyCapturesImmutablePlanAndStringCursorBytes(test *testing.T) {
	fixture := newDocToolValuePlanFixture(test, "{\"x\":[1,\"\ud55c🙂\",false]}")
	plan := fixture.plan
	input, err := NewDocToolValueAssemblyInput(plan)
	if err != nil {
		test.Fatal(err)
	}
	plan.Version, plan.TextBytes, plan.Pages.Count, plan.Source.Root.Depth = 9, 0, 0, 7
	plan.Source.Root.Key = &DocFindSpan{From: 0, To: 2}
	plan.Source.Tree.EventHash, plan.Source.Keys.Tree, plan.Source.Orders.Keys = HashContent(nil), HashContent(nil), HashContent(nil)
	page, next, err := input.read(context.Background(), input.initial, DocDisplayPageTextBytes, DocDisplayPageRuns, fixture.load)
	if err != nil || page.Text != fixture.oracle.Text() || len(next) == 0 {
		test.Fatal("assembly retained mutable caller plan", err)
	}
	stringFixture := prepareDocToolStringFixture(test, `"a🙂\n"`, DocToolStringQuoted)
	stringInput, err := NewDocToolStringAssemblyInput(stringFixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	want := fmt.Sprintf(`{"byte":0,"from":0,"page":0,"plan":%q,"version":1}`, stringInput.source.Plan)
	if string(stringInput.initial) != want {
		test.Fatal("serialized string cursor bytes changed", string(stringInput.initial), want)
	}
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return stringFixture.objects[hash], nil }
	first, cursor, err := stringInput.read(context.Background(), stringInput.initial, 4, 1, load)
	if err != nil {
		test.Fatal(err)
	}
	want = fmt.Sprintf(`{"byte":%d,"from":%d,"page":0,"plan":%q,"version":1}`, len(first.Text), len(first.Text), stringInput.source.Plan)
	if string(cursor) != want {
		test.Fatal("resumed string cursor bytes changed", string(cursor), want)
	}
	for _, token := range []string{`""`, `"a"`, `"🙂"`} {
		fixture := prepareDocToolStringFixture(test, token, DocToolStringQuoted)
		input, err := NewDocToolStringAssemblyInput(fixture.plan)
		if err != nil {
			test.Fatal(err)
		}
		assembled := assembleDisplayFixture(test, input, fixture.objects)
		if actual := assembledFixturePages(test, assembled, nil); actual != token {
			test.Fatal("short string final cursor failed shared assembly", actual, token)
		}
	}
}

func TestDocToolStringAssemblyShortFinalByteBudgets(test *testing.T) {
	for _, token := range []string{`""`, `"a"`} {
		test.Run(fmt.Sprint(len(token)), func(test *testing.T) {
			fixture := prepareDocToolStringFixture(test, token, DocToolStringQuoted)
			input, err := NewDocToolStringAssemblyInput(fixture.plan)
			if err != nil {
				test.Fatal(err)
			}
			load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
			page, next, err := input.read(context.Background(), input.initial, len(token), 1, load)
			if err != nil || page.Text != token || len(page.Runs) != 1 || page.From != 0 {
				test.Fatal("short final ASCII byte budget rejected", len(token), page, err)
			}
			position, err := input.cursor(next)
			want := fmt.Sprintf(`{"byte":0,"from":%d,"page":1,"plan":%q,"version":1}`, len(token), input.source.Plan)
			if err != nil || position != len(token) || string(next) != want {
				test.Fatal("short terminal string cursor changed", string(next), want, position, err)
			}
			for offset := range token {
				span, err := page.SourceRange(offset, 1)
				want := DocFindSpan{From: fixture.plan.Source.Token.From + offset, To: fixture.plan.Source.Token.From + offset + 1}
				if err != nil || span != want {
					test.Fatal("short ASCII source mapping changed", span, want, err)
				}
			}
		})
	}
}

func TestDocToolValueReadAndAssemblyErrorsDiscardPartialOutputs(test *testing.T) {
	fixture := newDocToolValuePlanFixture(test, `{"x":"`+strings.Repeat(`🙂\n`, 300)+`"}`)
	input, err := NewDocToolValueAssemblyInput(fixture.plan)
	if err != nil {
		test.Fatal(err)
	}
	for name, cursor := range map[string]docPreparedPageCursor{
		"version":       {Version: 2, Plan: input.source.Plan},
		"foreign-plan":  {Version: 1, Plan: HashContent(nil)},
		"negative-page": {Version: 1, Plan: input.source.Plan, Page: -1},
		"overflow-page": {Version: 1, Plan: input.source.Plan, Page: math.MaxInt},
		"zero-position": {Version: 1, Plan: input.source.Plan, Byte: 1},
		"negative-byte": {Version: 1, Plan: input.source.Plan, Byte: -1},
		"large-byte":    {Version: 1, Plan: input.source.Plan, Byte: DocDisplayPageTextBytes},
		"negative-from": {Version: 1, Plan: input.source.Plan, From: -1},
		"past-end":      {Version: 1, Plan: input.source.Plan, From: input.source.TextBytes + 1},
		"early-end":     {Version: 1, Plan: input.source.Plan, Page: fixture.plan.Pages.Count},
		"unfinished":    {Version: 1, Plan: input.source.Plan, From: input.source.TextBytes},
	} {
		test.Run(name, func(test *testing.T) {
			raw, _ := canonicalJSON(cursor)
			loads := 0
			page, next, err := input.read(context.Background(), raw, 4, 1, func(context.Context, ContentHash) ([]byte, error) { loads++; return nil, ErrNotFound })
			if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
				test.Fatal("invalid cursor read objects or returned partial output", loads, err)
			}
		})
	}
	for _, raw := range [][]byte{nil, append(bytes.Clone(input.initial), ' '), append([]byte(`{"unknown":1,`), input.initial[1:]...)} {
		if _, err := input.cursor(raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical cursor accepted", err)
		}
	}
	for _, index := range []int{-1, fixture.plan.Pages.Count, math.MaxInt} {
		loads := 0
		page, err := ReadDocToolValuePage(context.Background(), fixture.plan, index, func(context.Context, ContentHash) ([]byte, error) { loads++; return nil, ErrNotFound })
		if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocToolValuePage{}) {
			test.Fatal("invalid selected position read objects or returned data", loads, err)
		}
	}
	failure := errors.New("selected immutable object failed")
	for _, failAt := range []int{1, 2} {
		for _, corrupt := range []bool{false, true} {
			makeLoad := func() func(context.Context, ContentHash) ([]byte, error) {
				loads := 0
				return func(ctx context.Context, hash ContentHash) ([]byte, error) {
					loads++
					raw, err := fixture.load(ctx, hash)
					if loads == failAt {
						if corrupt {
							return append(raw, ' '), nil
						}
						return raw, failure
					}
					return raw, err
				}
			}
			page, err := ReadDocToolValuePage(context.Background(), fixture.plan, 0, makeLoad())
			fragment, next, readErr := input.read(context.Background(), input.initial, 4, 1, makeLoad())
			state, _ := NewDocDisplayAssemblyState(input)
			step, stepErr := StepDocDisplayAssembly(context.Background(), input, state, makeLoad())
			want := failure
			if corrupt {
				want = ErrIntegrity
			}
			if !errors.Is(err, want) || !errors.Is(readErr, want) || !errors.Is(stepErr, want) || !reflect.DeepEqual(page, DocToolValuePage{}) || !reflect.DeepEqual(fragment, DocDisplayPage{}) || next != nil || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) {
				test.Fatal("failed load retained partial output", failAt, corrupt, err, readErr, stepErr)
			}
		}
	}
	for _, cancelAt := range []int{0, 1, 2} {
		for _, operation := range []string{"page", "fragment", "assembly"} {
			ctx, cancel := context.WithCancel(context.Background())
			if cancelAt == 0 {
				cancel()
			}
			loads := 0
			load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
				loads++
				if loads == cancelAt {
					cancel()
				}
				return fixture.load(ctx, hash)
			}
			switch operation {
			case "page":
				page, err := ReadDocToolValuePage(ctx, fixture.plan, 0, load)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocToolValuePage{}) {
					test.Fatal("cancelled page retained output", cancelAt, err)
				}
			case "fragment":
				page, next, err := input.read(ctx, input.initial, 4, 1, load)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
					test.Fatal("cancelled fragment retained output", cancelAt, err)
				}
			case "assembly":
				state, _ := NewDocDisplayAssemblyState(input)
				step, err := StepDocDisplayAssembly(ctx, input, state, load)
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(step, DocDisplayAssemblyStep{}) {
					test.Fatal("cancelled assembly retained output", cancelAt, err)
				}
			}
			cancel()
		}
	}
}

func TestDocPreparedPageAssemblyRejectsInvalidFactoryAndBudgets(test *testing.T) {
	fixture := newDocToolValuePlanFixture(test, "\"\ud55c🙂x\"")
	input, _ := NewDocToolValueAssemblyInput(fixture.plan)
	read := func(ctx context.Context, index int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
		page, err := ReadDocToolValuePage(ctx, fixture.plan, index, load)
		return page.Display, err
	}
	for _, bounds := range [][2]int{{0, 256}, {-1, 256}, {MaxDocRangePages + 1, 256}, {1, 0}, {1, -1}, {1, DocDisplayPageTextBytes + 1}, {2, DocDisplayPageTextBytes}} {
		if _, err := newDocPreparedPageAssemblyInput(input.source, bounds[0], bounds[1], read); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid prepared-page bounds accepted", bounds, err)
		}
	}
	if _, err := newDocPreparedPageAssemblyInput(input.source, 1, 256, nil); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil typed reader accepted", err)
	}
	for _, limits := range [][2]int{{0, 1}, {-1, 1}, {DocDisplayPageTextBytes + 1, 1}, {4, 0}, {4, -1}, {4, DocDisplayPageRuns + 1}} {
		loads := 0
		page, next, err := input.read(context.Background(), input.initial, limits[0], limits[1], func(context.Context, ContentHash) ([]byte, error) { loads++; return nil, ErrNotFound })
		if !errors.Is(err, ErrIntegrity) || loads != 0 || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
			test.Fatal("invalid budget performed I/O or returned partial output", limits, loads, err)
		}
	}
	page, next, err := input.read(context.Background(), input.initial, 4, 1, nil)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("nil object loader accepted", err)
	}
	var cursor docPreparedPageCursor
	if json.Unmarshal(input.initial, &cursor) != nil {
		test.Fatal("invalid initial cursor")
	}
	cursor.Byte, cursor.From = 2, 2
	raw, _ := canonicalJSON(cursor)
	page, next, err = input.read(context.Background(), raw, 4, 1, fixture.load)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(page, DocDisplayPage{}) || next != nil {
		test.Fatal("cursor inside UTF-8 rune accepted", err)
	}
}
