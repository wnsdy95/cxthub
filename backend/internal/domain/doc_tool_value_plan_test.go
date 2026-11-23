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
)

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
