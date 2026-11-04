package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

type docVisibleFixture struct {
	events       []CIREvent
	streamed     VerifiedDocRangeIndex
	materialized VerifiedDocRangeIndex
	entries      []DocRangeEntry
	sourceReads  *int
	sourceHashes map[ContentHash][]byte
}

func docVisibleTestFixture(test *testing.T, events []CIREvent) docVisibleFixture {
	test.Helper()
	events = append([]CIREvent(nil), events...)
	for position := range events {
		events[position].Seq = position
	}
	manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: events})
	if err != nil {
		test.Fatal(err)
	}
	sourceReads := 0
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		sourceReads++
		return manifestProofLoad(bodies)(ctx, hash)
	}
	var verifier CanonicalDocVerifier
	hash := manifestProofHash(test, manifest)
	streamed, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, load, &docRangeMemoryBuffer{})
	if err != nil {
		test.Fatal(err)
	}
	document, err := verifier.VerifyConversationManifestDoc(context.Background(), hash, manifest, load)
	if err != nil {
		test.Fatal(err)
	}
	materialized, err := BuildDocRangeIndex(context.Background(), document)
	if err != nil {
		test.Fatal(err)
	}
	header := streamed.Header()
	entries := make([]DocRangeEntry, 0, len(events))
	for page := range header.Pages {
		raw := docRangeTestPage(test, streamed, page)
		if !bytes.Equal(raw, docRangeTestPage(test, materialized, page)) {
			test.Fatalf("streamed and materialized metadata differ on page %d", page)
		}
		decoded, err := header.DecodePage(page, raw)
		if err != nil {
			test.Fatal(err)
		}
		entries = append(entries, decoded...)
	}
	return docVisibleFixture{events: events, streamed: streamed, materialized: materialized, entries: entries, sourceReads: &sourceReads, sourceHashes: bodies}
}

func docVisibleTestJSON(test testing.TB, value any) []byte {
	test.Helper()
	raw, err := canonicalJSON(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func docVisibleTestRangeProof(test *testing.T, verified VerifiedDocRangeIndex) VerifiedDocRangeGeneration {
	test.Helper()
	proof, _, err := BuildVerifiedDocRangeGeneration(context.Background(), verified)
	if err != nil {
		test.Fatal(err)
	}
	return proof
}

func docVisibleTestBuild(test *testing.T, verified VerifiedDocRangeIndex) (DocVisibleGeneration, map[ContentHash][]byte) {
	test.Helper()
	proof := docVisibleTestRangeProof(test, verified)
	raw, err := proof.Descriptor().Encode()
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[ContentHash][]byte)
	generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(ctx context.Context, object DocRangeObject) error {
		if HashContent(object.Bytes) != object.Hash || len(object.Bytes) == 0 || len(object.Bytes) > MaxDocVisibleObjectBytes {
			test.Fatalf("invalid emitted visible object: %s (%d bytes)", object.Hash, len(object.Bytes))
		}
		if previous, exists := objects[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
			test.Fatal("same visible object hash has different bytes")
		}
		objects[object.Hash] = bytes.Clone(object.Bytes)
		return ctx.Err()
	})
	if err != nil {
		test.Fatal(err)
	}
	if err := generation.Validate(verified.Header().Hash, HashContent(raw)); err != nil {
		test.Fatal(err)
	}
	encoded, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocVisibleGeneration(generation.Hash, generation.RangeGeneration, HashContent(encoded), encoded)
	if err != nil || !reflect.DeepEqual(decoded, generation) {
		test.Fatalf("visible descriptor round trip: %+v, %v", decoded, err)
	}
	return generation, objects
}

func docVisibleTestMessage(role Role, text string) CIREvent {
	return CIREvent{Kind: EventMessage, Role: role, Blocks: []ContentBlock{{Type: "text", Text: text}}}
}

func docVisibleTestRendered(event CIREvent) bool {
	return event.Kind != EventReasoning || strings.TrimFunc(event.RedactedSummary, func(character rune) bool {
		return unicode.IsSpace(character) || character == '\ufeff'
	}) != ""
}

func docVisibleTestPrompt(event CIREvent) bool {
	if event.Kind != EventMessage || event.Role != RoleUser || event.CompactSummary {
		return false
	}
	blocks := make([]string, len(event.Blocks))
	for position, block := range event.Blocks {
		blocks[position] = block.Text
	}
	text := strings.TrimLeftFunc(strings.Join(blocks, "\n"), func(character rune) bool {
		return unicode.IsSpace(character) || character == '\ufeff'
	})
	return !strings.HasPrefix(text, "[cxt seed] Branch-switch context:") && !strings.HasPrefix(text, "[cxt] This session was resumed from a branch context seed.")
}

func docVisibleReference(events []CIREvent, mode string) []DocVisibleUnit {
	ranks := make([]int, len(events)+1)
	turns := []int{-1}
	for position, event := range events {
		ranks[position+1] = ranks[position]
		if docVisibleTestRendered(event) {
			ranks[position+1]++
		}
		if docVisibleTestPrompt(event) {
			turns = append(turns, position)
		}
	}
	units := make([]DocVisibleUnit, 0)
	appendUnit := func(from, to, event, turn int) {
		if from == to {
			return
		}
		fold := ranks[to] - ranks[from]
		if event >= 0 {
			fold--
		}
		units = append(units, DocVisibleUnit{From: from, To: to, EventIndex: event, TurnIndex: turn, RenderedBefore: ranks[from], FoldRows: fold})
	}
	for ordinal, turn := range turns {
		from, to := max(0, turn), len(events)
		if ordinal+1 < len(turns) {
			to = turns[ordinal+1]
		}
		switch mode {
		case "all":
			for position := from; position < to; position++ {
				event := -1
				if docVisibleTestRendered(events[position]) {
					event = position
				}
				appendUnit(position, position+1, event, turn)
			}
		case "prompts":
			appendUnit(from, to, turn, turn)
		case "chat":
			if turn >= 0 {
				appendUnit(turn, turn+1, turn, turn)
				from++
			}
			for position := from; position < to; position++ {
				if events[position].Kind == EventMessage {
					appendUnit(from, position+1, position, turn)
					from = position + 1
				}
			}
			appendUnit(from, to, -1, turn)
		}
	}
	return units
}

func docVisibleCheckPartition(test *testing.T, fixture docVisibleFixture, generation DocVisibleGeneration, objects map[ContentHash][]byte) {
	test.Helper()
	rendered := 0
	for position, event := range fixture.events {
		if fixture.entries[position].RenderedBefore != rendered {
			test.Fatalf("event %d rendered rank = %d; want %d", position, fixture.entries[position].RenderedBefore, rendered)
		}
		if docVisibleTestRendered(event) {
			rendered++
		}
	}
	if generation.Events != len(fixture.events) || generation.Rendered != rendered {
		test.Fatalf("generation counts = %d/%d; want %d/%d", generation.Events, generation.Rendered, len(fixture.events), rendered)
	}
	for _, mode := range []string{"all", "chat", "prompts"} {
		want := docVisibleReference(fixture.events, mode)
		if generation.Views[mode].Units != len(want) {
			test.Fatalf("%s units = %d; want %d", mode, generation.Views[mode].Units, len(want))
		}
		next, rank := 0, 0
		for ordinal, expected := range want {
			unit, gotOrdinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, ordinal, false, docRangeGenerationLoad(objects))
			if err != nil || unit != expected || gotOrdinal != ordinal {
				test.Fatalf("%s unit %d: got %+v/%d, %v; want %+v", mode, ordinal, unit, gotOrdinal, err, expected)
			}
			if unit.From != next || unit.RenderedBefore != rank || unit.Validate(generation.Events, mode) != nil {
				test.Fatalf("%s unit %d breaks original-range/rank partition: %+v", mode, ordinal, unit)
			}
			for position := unit.From; position < unit.To; position++ {
				byEvent, rawOrdinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, position, true, docRangeGenerationLoad(objects))
				if err != nil || byEvent != unit || rawOrdinal != ordinal {
					test.Fatalf("%s raw event %d selected %+v/%d, %v; want %+v/%d", mode, position, byEvent, rawOrdinal, err, unit, ordinal)
				}
				clipped, err := byEvent.Clip(position, fixture.entries[position].RenderedBefore, generation.Events, mode)
				expectedClip := expected
				expectedClip.From = position
				expectedClip.RenderedBefore = fixture.entries[position].RenderedBefore
				if expectedClip.EventIndex < position {
					expectedClip.EventIndex = -1
				}
				expectedClip.FoldRows = 0
				for suffix := position; suffix < expected.To; suffix++ {
					if suffix != expectedClip.EventIndex && docVisibleTestRendered(fixture.events[suffix]) {
						expectedClip.FoldRows++
					}
				}
				if err != nil || clipped != expectedClip || clipped.RenderedEnd() != unit.RenderedEnd() {
					test.Fatalf("%s clip at event %d = %+v, %v; want %+v", mode, position, clipped, err, expectedClip)
				}
			}
			next, rank = unit.To, unit.RenderedEnd()
		}
		if next != generation.Events || rank != generation.Rendered {
			test.Fatalf("%s partition ends at %d/%d; want %d/%d", mode, next, rank, generation.Events, generation.Rendered)
		}
	}
	docVisibleCheckExpandedSuffix(test, fixture, generation, objects, 0)
}

func docVisibleCheckExpandedSuffix(test *testing.T, fixture docVisibleFixture, generation DocVisibleGeneration, objects map[ContentHash][]byte, start int) {
	test.Helper()
	wantOriginal, wantRendered := make([]int, 0), make([]int, 0)
	wantHashes := make([]ContentHash, 0)
	for position := start; position < len(fixture.events); position++ {
		wantOriginal = append(wantOriginal, position)
		wantHashes = append(wantHashes, fixture.entries[position].Hash)
		if docVisibleTestRendered(fixture.events[position]) {
			wantRendered = append(wantRendered, position)
		}
	}
	for _, mode := range []string{"all", "chat", "prompts"} {
		original, rendered := make([]int, 0), make([]int, 0)
		hashes := make([]ContentHash, 0)
		appendEvent := func(position int) bool {
			if position < start || position >= len(fixture.events) {
				test.Fatalf("%s expansion escaped suffix [%d,%d): %d", mode, start, len(fixture.events), position)
			}
			original = append(original, position)
			raw, err := json.Marshal(fixture.events[position])
			if err != nil {
				test.Fatal(err)
			}
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				test.Fatal(err)
			}
			hashes = append(hashes, HashContent(docVisibleTestJSON(test, value)))
			visible := docVisibleTestRendered(fixture.events[position])
			if visible {
				rendered = append(rendered, position)
			}
			return visible
		}
		if start < len(fixture.events) {
			first, firstOrdinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, start, true, docRangeGenerationLoad(objects))
			if err != nil {
				test.Fatal(err)
			}
			for ordinal := firstOrdinal; ordinal < generation.Views[mode].Units; ordinal++ {
				unit, gotOrdinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, ordinal, false, docRangeGenerationLoad(objects))
				if err != nil || ordinal != gotOrdinal {
					test.Fatalf("%s expansion unit %d: ordinal=%d, %v", mode, ordinal, gotOrdinal, err)
				}
				if ordinal == firstOrdinal {
					if unit != first {
						test.Fatal("event seek and unit seek disagree before expansion")
					}
					unit, err = unit.Clip(start, fixture.entries[start].RenderedBefore, generation.Events, mode)
					if err != nil || unit.TurnIndex != first.TurnIndex || unit.To != first.To || unit.RenderedEnd() != first.RenderedEnd() {
						test.Fatalf("%s inherited clip changed turn or remaining extent: %+v, %v", mode, unit, err)
					}
				}
				foldFrom, foldTo := unit.From, unit.To
				if unit.EventIndex >= 0 {
					switch mode {
					case "all", "prompts":
						if !appendEvent(unit.EventIndex) {
							test.Fatal("non-rendered event became an anchor")
						}
						foldFrom = unit.EventIndex + 1
					case "chat":
						foldTo = unit.EventIndex
					}
				}
				foldRows := 0
				for position := foldFrom; position < foldTo; position++ {
					if appendEvent(position) {
						foldRows++
					}
				}
				if mode == "chat" && unit.EventIndex >= 0 && !appendEvent(unit.EventIndex) {
					test.Fatal("non-rendered event became a chat anchor")
				}
				if foldRows != unit.FoldRows {
					test.Fatalf("%s unit [%d,%d) advertises %d fold rows, independently expanded %d", mode, unit.From, unit.To, unit.FoldRows, foldRows)
				}
			}
		}
		if !reflect.DeepEqual(original, wantOriginal) || !reflect.DeepEqual(hashes, wantHashes) || !reflect.DeepEqual(rendered, wantRendered) {
			test.Fatalf("%s expanding all folds from %d changed source order, identity, or rendering:\noriginal=%v\nwant=%v\nrendered=%v\nwant=%v", mode, start, original, wantOriginal, rendered, wantRendered)
		}
	}
}

func TestDocVisibleGenerationOriginalRangesAndClippedSuffixes(test *testing.T) {
	compact := docVisibleTestMessage(RoleUser, "provider compact summary")
	compact.CompactSummary = true
	seed := docVisibleTestMessage(RoleUser, "[cxt seed] Branch-switch context: inherited")
	seed.Blocks = append([]ContentBlock{{Type: "text", Text: " \t\n"}}, seed.Blocks...)
	compaction := CIREvent{Kind: EventCompaction, ReplacementComplete: true, Replacement: []CIREvent{docVisibleTestMessage(RoleUser, "nested prompt is not a turn")}}
	fixtures := map[string][]CIREvent{
		"empty":                {},
		"only-empty-reasoning": {{Kind: EventReasoning}, {Kind: EventReasoning, RedactedSummary: " \t\n\u3000"}},
		"leading-and-unanswered": {
			{Kind: EventToolResult, CallID: "lead", Output: "leading work"},
			{Kind: EventReasoning},
			docVisibleTestMessage(RoleAssistant, "leading answer"),
			docVisibleTestMessage(RoleUser, "unanswered prompt"),
			{Kind: EventReasoning, RedactedSummary: "visible work"},
			{Kind: EventToolCall, CallID: "tail", ToolName: "read"},
			{Kind: EventReasoning, RedactedSummary: "\t "},
		},
		"adjacent-prompts-multiple-assistants": {
			docVisibleTestMessage(RoleUser, "first"), docVisibleTestMessage(RoleUser, "second"),
			{Kind: EventReasoning}, {Kind: EventToolResult, Output: "work"},
			docVisibleTestMessage(RoleAssistant, "answer one"), docVisibleTestMessage(RoleAssistant, "answer two"),
			{Kind: EventReasoning}, docVisibleTestMessage(RoleUser, "last"),
		},
		"summaries-and-compaction": {
			seed, compact, compaction, docVisibleTestMessage(RoleUser, "real prompt"),
			{Kind: EventReasoning}, seed, compact,
			docVisibleTestMessage(RoleUser, " \n[cxt] This session was resumed from a branch context seed. inherited"),
			compaction, {Kind: EventReasoning, RedactedSummary: "visible"},
			docVisibleTestMessage(RoleAssistant, "answer"), docVisibleTestMessage(RoleUser, "next real prompt"),
		},
	}
	for name, events := range fixtures {
		test.Run(name, func(test *testing.T) {
			fixture := docVisibleTestFixture(test, events)
			generation, objects := docVisibleTestBuild(test, fixture.streamed)
			materialized, otherObjects := docVisibleTestBuild(test, fixture.materialized)
			repeated, repeatedObjects := docVisibleTestBuild(test, fixture.streamed)
			if !reflect.DeepEqual(generation, materialized) || !reflect.DeepEqual(objects, otherObjects) || !reflect.DeepEqual(generation, repeated) || !reflect.DeepEqual(objects, repeatedObjects) {
				test.Fatal("streaming, materialized, and repeated visible generations differ")
			}
			docVisibleCheckPartition(test, fixture, generation, objects)
			if len(events) == 0 && len(objects) != 0 {
				test.Fatal("empty generation emitted objects")
			}
		})
	}
}

func TestDocVisibleGenerationFixedSeedReferencePartition(test *testing.T) {
	random := rand.New(rand.NewSource(20261019))
	for trial := 0; trial < 20; trial++ {
		test.Run(fmt.Sprint(trial), func(test *testing.T) {
			events := make([]CIREvent, random.Intn(2*DocRangePageEvents+17))
			for position := range events {
				switch random.Intn(10) {
				case 0, 1:
					events[position] = docVisibleTestMessage(RoleUser, "prompt")
				case 2:
					events[position] = docVisibleTestMessage(RoleAssistant, "answer")
				case 3:
					events[position] = CIREvent{Kind: EventReasoning}
				case 4:
					events[position] = CIREvent{Kind: EventReasoning, RedactedSummary: " \t\n"}
				case 5:
					events[position] = CIREvent{Kind: EventReasoning, RedactedSummary: "visible work"}
				case 6:
					events[position] = CIREvent{Kind: EventToolResult, Output: "output"}
				case 7:
					events[position] = docVisibleTestMessage(RoleUser, "[cxt seed] Branch-switch context: inherited")
				case 8:
					events[position] = docVisibleTestMessage(RoleUser, "summary")
					events[position].CompactSummary = true
				case 9:
					events[position] = CIREvent{Kind: EventCompaction, Replacement: []CIREvent{}, ReplacementComplete: true}
				}
			}
			fixture := docVisibleTestFixture(test, events)
			generation, objects := docVisibleTestBuild(test, fixture.streamed)
			docVisibleCheckPartition(test, fixture, generation, objects)
		})
	}
}

func docVisibleRendererEvents() []CIREvent {
	return []CIREvent{
		{Kind: EventToolResult, Output: "leading tool"},
		{Kind: EventCompaction, Replacement: []CIREvent{}, ReplacementComplete: true},
		docVisibleTestMessage(RoleUser, "[cxt seed] Branch-switch context: inherited"),
		docVisibleTestMessage(RoleUser, "first prompt"),
		{Kind: EventToolResult, Output: "work"},
		{Kind: EventReasoning},
		docVisibleTestMessage(RoleAssistant, "first answer"),
		{Kind: EventToolResult, Output: "more work"},
		docVisibleTestMessage(RoleAssistant, "second answer"),
		{Kind: EventToolResult, Output: "unfinished work"},
		docVisibleTestMessage(RoleUser, "second prompt"),
		{Kind: EventReasoning, RedactedSummary: "visible reasoning"},
	}
}

func TestDocVisibleGenerationRendererReviewFixture(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRendererEvents())
	generation, objects := docVisibleTestBuild(test, fixture.streamed)
	expected := map[string][]DocVisibleUnit{
		"prompts": {
			{From: 0, To: 3, EventIndex: -1, TurnIndex: -1, RenderedBefore: 0, FoldRows: 3},
			{From: 3, To: 10, EventIndex: 3, TurnIndex: 3, RenderedBefore: 3, FoldRows: 5},
			{From: 10, To: 12, EventIndex: 10, TurnIndex: 10, RenderedBefore: 9, FoldRows: 1},
		},
		"chat": {
			{From: 0, To: 3, EventIndex: 2, TurnIndex: -1, RenderedBefore: 0, FoldRows: 2},
			{From: 3, To: 4, EventIndex: 3, TurnIndex: 3, RenderedBefore: 3},
			{From: 4, To: 7, EventIndex: 6, TurnIndex: 3, RenderedBefore: 4, FoldRows: 1},
			{From: 7, To: 9, EventIndex: 8, TurnIndex: 3, RenderedBefore: 6, FoldRows: 1},
			{From: 9, To: 10, EventIndex: -1, TurnIndex: 3, RenderedBefore: 8, FoldRows: 1},
			{From: 10, To: 11, EventIndex: 10, TurnIndex: 10, RenderedBefore: 9},
			{From: 11, To: 12, EventIndex: -1, TurnIndex: 10, RenderedBefore: 10, FoldRows: 1},
		},
	}
	for mode, units := range expected {
		if generation.Views[mode].Units != len(units) {
			test.Fatalf("%s renderer unit count = %d; want %d", mode, generation.Views[mode].Units, len(units))
		}
		for ordinal, want := range units {
			got, gotOrdinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, ordinal, false, docRangeGenerationLoad(objects))
			if err != nil || got != want || gotOrdinal != ordinal {
				test.Fatalf("%s renderer unit %d: %+v/%d, %v; want %+v", mode, ordinal, got, gotOrdinal, err, want)
			}
		}
	}
	for mode, want := range map[string]DocVisibleUnit{
		"prompts": {From: 5, To: 10, EventIndex: -1, TurnIndex: 3, RenderedBefore: 5, FoldRows: 4},
		"chat":    {From: 5, To: 7, EventIndex: 6, TurnIndex: 3, RenderedBefore: 5},
	} {
		unit, _, err := ReadDocVisibleUnit(context.Background(), generation, mode, 5, true, docRangeGenerationLoad(objects))
		if err != nil {
			test.Fatal(err)
		}
		clipped, err := unit.Clip(5, fixture.entries[5].RenderedBefore, generation.Events, mode)
		if err != nil || clipped != want {
			test.Fatalf("%s renderer suffix: %+v, %v; want %+v", mode, clipped, err, want)
		}
	}
	docVisibleCheckPartition(test, fixture, generation, objects)
}

func docVisiblePrefixSource(verified VerifiedDocRangeIndex) DocRangePrefixSource {
	header := verified.Header()
	return DocRangePrefixSource{
		Total: int(header.Manifest.EventCount),
		PageHash: func(ctx context.Context, page int) (ContentHash, error) {
			return header.Pages[page], ctx.Err()
		},
		ReadPage: func(ctx context.Context, page int) ([]DocRangeEntry, error) {
			raw, err := verified.Page(ctx, page)
			if err != nil {
				return nil, err
			}
			return header.DecodePage(page, raw)
		},
	}
}

func TestDocVisibleGenerationInheritedPrefixClippingReconstructsOnlyNewEvents(test *testing.T) {
	events := make([]CIREvent, 0)
	for len(events) < 2*DocRangePageEvents+1 {
		events = append(events, docVisibleRendererEvents()...)
	}
	fixture := docVisibleTestFixture(test, events)
	generation, objects := docVisibleTestBuild(test, fixture.streamed)
	for _, boundary := range []int{0, 1, 2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 126, 127, 128, 129, 255, 256, len(events)} {
		for _, branch := range []string{"strict-prefix", "diverged-sibling"} {
			if branch == "diverged-sibling" && boundary == len(events) {
				continue
			}
			test.Run(fmt.Sprintf("%s/inherited-%d", branch, boundary), func(test *testing.T) {
				baseEvents := append([]CIREvent(nil), events[:boundary]...)
				if branch == "diverged-sibling" {
					baseEvents = append([]CIREvent(nil), events...)
					baseEvents[boundary] = CIREvent{Kind: EventToolResult, Output: "different branch body"}
				}
				base := docVisibleTestFixture(test, baseEvents)
				progress := DocRangePrefixProgress{}
				for !progress.Complete {
					var err error
					progress, err = CompareDocRangePrefix(context.Background(), docVisiblePrefixSource(base.streamed), docVisiblePrefixSource(fixture.streamed), progress.Events, 1)
					if err != nil {
						test.Fatal(err)
					}
				}
				if progress.Events != boundary {
					test.Fatalf("verified root-pair prefix = %d; want %d", progress.Events, boundary)
				}
				docVisibleCheckExpandedSuffix(test, fixture, generation, objects, progress.Events)
				if boundary == len(events) {
					for _, mode := range []string{"all", "chat", "prompts"} {
						loads := 0
						_, _, err := ReadDocVisibleUnit(context.Background(), generation, mode, boundary, true, func(context.Context, ContentHash) ([]byte, error) {
							loads++
							return nil, ErrNotFound
						})
						if !errors.Is(err, ErrValidation) || loads != 0 {
							test.Fatalf("fully inherited suffix read %d objects: %v", loads, err)
						}
					}
				}
			})
		}
	}
}

func docVisibleCheckTree(test *testing.T, root DocVisibleRoot, objects map[ContentHash][]byte) {
	test.Helper()
	pageCount, directoryCount, unitCount := 0, 0, 0
	var walk func(ContentHash, int)
	walk = func(hash ContentHash, level int) {
		if level < 0 {
			var page docVisiblePage
			if err := json.Unmarshal(objects[hash], &page); err != nil {
				test.Fatal(err)
			}
			want := min(DocVisiblePageUnits, root.Units-unitCount)
			if page.Start != unitCount || len(page.Units) != want {
				test.Fatalf("page at %d contains %d units; want start %d and count %d", page.Start, len(page.Units), unitCount, want)
			}
			pageCount++
			unitCount += len(page.Units)
			return
		}
		var directory docVisibleDirectory
		if err := json.Unmarshal(objects[hash], &directory); err != nil {
			test.Fatal(err)
		}
		if directory.Level != level || len(directory.Children) == 0 || len(directory.Children) > DocRangeDirectoryFanout {
			test.Fatalf("bad generated directory level/count: %+v", directory)
		}
		directoryCount++
		for _, child := range directory.Children {
			before := unitCount
			walk(child.Hash, level-1)
			if child.First != before || child.Limit != unitCount {
				test.Fatalf("directory claims [%d,%d), actual descendants cover [%d,%d)", child.First, child.Limit, before, unitCount)
			}
		}
	}
	wantPages := (root.Units + DocVisiblePageUnits - 1) / DocVisiblePageUnits
	wantDirectories, level := 0, -1
	for children := wantPages; children > 0; {
		parents := (children + DocRangeDirectoryFanout - 1) / DocRangeDirectoryFanout
		wantDirectories += parents
		level++
		if parents == 1 {
			break
		}
		children = parents
	}
	walk(root.Hash, level)
	if pageCount != wantPages || directoryCount != wantDirectories || unitCount != root.Units {
		test.Fatalf("actual tree = %d pages/%d directories/%d units; want %d/%d/%d", pageCount, directoryCount, unitCount, wantPages, wantDirectories, root.Units)
	}
}

func TestDocVisibleGenerationPageAndDirectoryEdges(test *testing.T) {
	for _, count := range []int{DocVisiblePageUnits - 1, DocVisiblePageUnits, DocVisiblePageUnits + 1, DocVisiblePageUnits * DocRangeDirectoryFanout, DocVisiblePageUnits*DocRangeDirectoryFanout + 1} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			events := make([]CIREvent, count)
			for position := range events {
				events[position] = docVisibleTestMessage(RoleUser, "prompt")
			}
			fixture := docVisibleTestFixture(test, events)
			generation, objects := docVisibleTestBuild(test, fixture.streamed)
			for _, mode := range []string{"all", "chat", "prompts"} {
				if generation.Views[mode].Units != count {
					test.Fatalf("%s lost prompt units", mode)
				}
				docVisibleCheckTree(test, generation.Views[mode], objects)
				for _, position := range []int{0, 1, 126, 127, 128, count / 2, count - 2, count - 1} {
					if position < 0 || position >= count {
						continue
					}
					for _, byEvent := range []bool{false, true} {
						loads := 0
						unit, ordinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, position, byEvent, func(ctx context.Context, hash ContentHash) ([]byte, error) {
							loads++
							return docRangeGenerationLoad(objects)(ctx, hash)
						})
						want := DocVisibleUnit{From: position, To: position + 1, EventIndex: position, TurnIndex: position, RenderedBefore: position}
						wantLoads := 2
						if count > DocVisiblePageUnits*DocRangeDirectoryFanout {
							wantLoads++
						}
						if err != nil || unit != want || ordinal != position || loads != wantLoads {
							test.Fatalf("%s position %d byEvent=%t: %+v/%d, %v, %d loads; want %+v, %d loads", mode, position, byEvent, unit, ordinal, err, loads, want, wantLoads)
						}
					}
				}
			}
		})
	}
}

func TestDocVisibleGenerationStreamingEmissionOwnsObjectBytes(test *testing.T) {
	events := make([]CIREvent, 2*DocVisiblePageUnits+1)
	for position := range events {
		events[position] = docVisibleTestMessage(RoleUser, "prompt")
	}
	fixture := docVisibleTestFixture(test, events)
	want, wantObjects := docVisibleTestBuild(test, fixture.streamed)
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	for _, mutateInCallback := range []bool{false, true} {
		test.Run(fmt.Sprintf("mutate-in-callback=%t", mutateInCallback), func(test *testing.T) {
			var retained []DocRangeObject
			var snapshots [][]byte
			got, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
			if err != nil || !reflect.DeepEqual(got, want) {
				test.Fatalf("discarding emitted bytes changed generation: %+v, %v", got, err)
			}
			got, err = BuildDocVisibleGeneration(context.Background(), proof, func(_ context.Context, object DocRangeObject) error {
				retained = append(retained, object)
				snapshots = append(snapshots, bytes.Clone(object.Bytes))
				if mutateInCallback {
					for position := range object.Bytes {
						object.Bytes[position] = 'x'
					}
				}
				return nil
			})
			if err != nil || !reflect.DeepEqual(got, want) || len(retained) < 6 {
				test.Fatalf("retained callback buffers changed generation: %d objects, %+v, %v", len(retained), got, err)
			}
			for position, object := range retained {
				if !bytes.Equal(snapshots[position], wantObjects[object.Hash]) || HashContent(snapshots[position]) != object.Hash {
					test.Fatalf("emission %d differs from independent build", position)
				}
				if !mutateInCallback && !bytes.Equal(object.Bytes, snapshots[position]) {
					test.Fatalf("emission %d aliases a later page/directory buffer", position)
				}
			}
			for _, object := range retained {
				object.Bytes[0] = 'x'
			}
			rebuilt, rebuiltObjects := docVisibleTestBuild(test, fixture.streamed)
			if !reflect.DeepEqual(rebuilt, want) || !reflect.DeepEqual(rebuiltObjects, wantObjects) {
				test.Fatal("caller-owned emitted bytes changed verified metadata or later builds")
			}
		})
	}
}

func docVisibleTestClone(generation DocVisibleGeneration) DocVisibleGeneration {
	views := make(map[string]DocVisibleRoot, len(generation.Views))
	for mode, root := range generation.Views {
		views[mode] = root
	}
	generation.Views = views
	return generation
}

func TestDocVisibleGenerationRejectsMalformedDescriptors(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), {Kind: EventReasoning}})
	generation, _ := docVisibleTestBuild(test, fixture.streamed)
	mutations := map[string]func(*DocVisibleGeneration){
		"version":               func(value *DocVisibleGeneration) { value.Version++ },
		"algorithm":             func(value *DocVisibleGeneration) { value.Algorithm++ },
		"document-binding":      func(value *DocVisibleGeneration) { value.Hash = HashContent([]byte("other document")) },
		"range-binding":         func(value *DocVisibleGeneration) { value.RangeGeneration = HashContent([]byte("other ranges")) },
		"invalid-document-hash": func(value *DocVisibleGeneration) { value.Hash = "bad" },
		"invalid-range-hash":    func(value *DocVisibleGeneration) { value.RangeGeneration = "bad" },
		"negative-events":       func(value *DocVisibleGeneration) { value.Events = -1 },
		"too-many-events":       func(value *DocVisibleGeneration) { value.Events = MaxConversationDocumentBytes + 1 },
		"negative-rendered":     func(value *DocVisibleGeneration) { value.Rendered = -1 },
		"too-many-rendered":     func(value *DocVisibleGeneration) { value.Rendered = value.Events + 1 },
		"missing-view":          func(value *DocVisibleGeneration) { delete(value.Views, "chat") },
		"unknown-view":          func(value *DocVisibleGeneration) { value.Views["unknown"] = value.Views["chat"] },
		"substituted-view": func(value *DocVisibleGeneration) {
			value.Views["unknown"] = value.Views["chat"]
			delete(value.Views, "chat")
		},
		"nil-views":           func(value *DocVisibleGeneration) { value.Views = nil },
		"empty-nonempty-root": func(value *DocVisibleGeneration) { value.Views["chat"] = DocVisibleRoot{} },
		"invalid-root-hash":   func(value *DocVisibleGeneration) { value.Views["chat"] = DocVisibleRoot{Hash: "bad", Units: 1} },
		"negative-units": func(value *DocVisibleGeneration) {
			root := value.Views["chat"]
			root.Units = -1
			value.Views["chat"] = root
		},
		"too-many-units": func(value *DocVisibleGeneration) {
			root := value.Views["chat"]
			root.Units = value.Events + 1
			value.Views["chat"] = root
		},
		"all-not-event-count": func(value *DocVisibleGeneration) { root := value.Views["all"]; root.Units--; value.Views["all"] = root },
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			candidate := docVisibleTestClone(generation)
			mutate(&candidate)
			if !errors.Is(candidate.Validate(generation.Hash, generation.RangeGeneration), ErrIntegrity) {
				test.Fatal("invalid descriptor validated")
			}
			raw := docVisibleTestJSON(test, candidate)
			decoded, err := DecodeDocVisibleGeneration(generation.Hash, generation.RangeGeneration, HashContent(raw), raw)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocVisibleGeneration{}) {
				test.Fatalf("invalid descriptor returned a generation: %+v, %v", decoded, err)
			}
		})
	}
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	invalidBytes := map[string][]byte{
		"empty":                   nil,
		"oversized":               bytes.Repeat([]byte(" "), MaxDocVisibleGenerationBytes+1),
		"invalid-json":            []byte("{"),
		"null":                    []byte("null"),
		"noncanonical-whitespace": append([]byte(" "), raw...),
		"unknown-field":           append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"unknown":0}`)...),
		"duplicate-field":         append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"version":1}`)...),
	}
	for name, input := range invalidBytes {
		test.Run(name, func(test *testing.T) {
			decoded, err := DecodeDocVisibleGeneration(generation.Hash, generation.RangeGeneration, HashContent(input), input)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocVisibleGeneration{}) {
				test.Fatalf("noncanonical descriptor accepted: %+v, %v", decoded, err)
			}
		})
	}
	if _, err := DecodeDocVisibleGeneration(generation.Hash, generation.RangeGeneration, HashContent([]byte("wrong descriptor")), raw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("descriptor hash mismatch accepted", err)
	}
}

func TestDocVisibleGenerationRejectsMalformedDirectoriesAndPages(test *testing.T) {
	events := make([]CIREvent, DocVisiblePageUnits+1)
	for position := range events {
		events[position] = docVisibleTestMessage(RoleUser, "prompt")
	}
	fixture := docVisibleTestFixture(test, events)
	generation, objects := docVisibleTestBuild(test, fixture.streamed)
	directoryMutations := map[string]func(*docVisibleDirectory){
		"level":             func(value *docVisibleDirectory) { value.Level++ },
		"negative-level":    func(value *docVisibleDirectory) { value.Level = -1 },
		"empty":             func(value *docVisibleDirectory) { value.Children = nil },
		"missing-child":     func(value *docVisibleDirectory) { value.Children = value.Children[:1] },
		"extra-child":       func(value *docVisibleDirectory) { value.Children = append(value.Children, value.Children[1]) },
		"bad-hash":          func(value *docVisibleDirectory) { value.Children[1].Hash = "bad" },
		"first-gap":         func(value *docVisibleDirectory) { value.Children[1].First++ },
		"first-overlap":     func(value *docVisibleDirectory) { value.Children[1].First-- },
		"limit":             func(value *docVisibleDirectory) { value.Children[0].Limit-- },
		"range-gap":         func(value *docVisibleDirectory) { value.Children[1].From++ },
		"range-overlap":     func(value *docVisibleDirectory) { value.Children[1].From-- },
		"range-end":         func(value *docVisibleDirectory) { value.Children[1].To++ },
		"empty-range":       func(value *docVisibleDirectory) { value.Children[1].To = value.Children[1].From },
		"rank-gap":          func(value *docVisibleDirectory) { value.Children[1].Rank++ },
		"rank-overlap":      func(value *docVisibleDirectory) { value.Children[1].Rank-- },
		"rendered-end":      func(value *docVisibleDirectory) { value.Children[1].Rendered-- },
		"rendered-overflow": func(value *docVisibleDirectory) { value.Children[0].Rendered = generation.Events + 1 },
	}
	pageMutations := map[string]func(*docVisiblePage){
		"start":                  func(value *docVisiblePage) { value.Start++ },
		"empty":                  func(value *docVisiblePage) { value.Units = nil },
		"missing-unit":           func(value *docVisiblePage) { value.Units = value.Units[:len(value.Units)-1] },
		"extra-unit":             func(value *docVisiblePage) { value.Units = append(value.Units, value.Units[0]) },
		"range-gap":              func(value *docVisiblePage) { value.Units[1].From++ },
		"range-overlap":          func(value *docVisiblePage) { value.Units[1].From-- },
		"empty-range":            func(value *docVisiblePage) { value.Units[1].To = value.Units[1].From },
		"past-document":          func(value *docVisiblePage) { value.Units[1].To = generation.Events + 1 },
		"event-outside-unit":     func(value *docVisiblePage) { value.Units[1].EventIndex++ },
		"invalid-event-sentinel": func(value *docVisiblePage) { value.Units[1].EventIndex = -2 },
		"future-turn":            func(value *docVisiblePage) { value.Units[1].TurnIndex++ },
		"invalid-turn-sentinel":  func(value *docVisiblePage) { value.Units[1].TurnIndex = -2 },
		"negative-rank":          func(value *docVisiblePage) { value.Units[1].RenderedBefore = -1 },
		"rank-discontinuity":     func(value *docVisiblePage) { value.Units[1].RenderedBefore-- },
		"negative-fold":          func(value *docVisiblePage) { value.Units[1].FoldRows = -1 },
		"fold-overflow":          func(value *docVisiblePage) { value.Units[1].FoldRows = 2 },
	}
	for _, mode := range []string{"all", "chat", "prompts"} {
		for name, mutate := range directoryMutations {
			test.Run(mode+"/directory/"+name, func(test *testing.T) {
				var directory docVisibleDirectory
				if err := json.Unmarshal(objects[generation.Views[mode].Hash], &directory); err != nil {
					test.Fatal(err)
				}
				mutate(&directory)
				raw := docVisibleTestJSON(test, directory)
				candidate := docVisibleTestClone(generation)
				root := candidate.Views[mode]
				root.Hash = HashContent(raw)
				candidate.Views[mode] = root
				load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
					if hash == root.Hash {
						return raw, nil
					}
					return docRangeGenerationLoad(objects)(ctx, hash)
				}
				unit, ordinal, err := ReadDocVisibleUnit(context.Background(), candidate, mode, 0, false, load)
				if !errors.Is(err, ErrIntegrity) || unit != (DocVisibleUnit{}) || ordinal != 0 {
					test.Fatalf("malformed directory accepted: %+v/%d, %v", unit, ordinal, err)
				}
			})
		}
		for name, mutate := range pageMutations {
			test.Run(mode+"/page/"+name, func(test *testing.T) {
				var directory docVisibleDirectory
				if err := json.Unmarshal(objects[generation.Views[mode].Hash], &directory); err != nil {
					test.Fatal(err)
				}
				var page docVisiblePage
				if err := json.Unmarshal(objects[directory.Children[0].Hash], &page); err != nil {
					test.Fatal(err)
				}
				mutate(&page)
				rawPage := docVisibleTestJSON(test, page)
				directory.Children[0].Hash = HashContent(rawPage)
				rawDirectory := docVisibleTestJSON(test, directory)
				candidate := docVisibleTestClone(generation)
				root := candidate.Views[mode]
				root.Hash = HashContent(rawDirectory)
				candidate.Views[mode] = root
				load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
					switch hash {
					case root.Hash:
						return rawDirectory, nil
					case directory.Children[0].Hash:
						return rawPage, nil
					default:
						return docRangeGenerationLoad(objects)(ctx, hash)
					}
				}
				unit, ordinal, err := ReadDocVisibleUnit(context.Background(), candidate, mode, 0, false, load)
				if !errors.Is(err, ErrIntegrity) || unit != (DocVisibleUnit{}) || ordinal != 0 {
					test.Fatalf("malformed page accepted: %+v/%d, %v", unit, ordinal, err)
				}
			})
		}
	}
}

func TestDocVisibleGenerationObjectAuthenticationAndReadFailures(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), docVisibleTestMessage(RoleAssistant, "answer")})
	generation, objects := docVisibleTestBuild(test, fixture.streamed)
	for _, target := range []string{"directory", "page"} {
		var directory docVisibleDirectory
		if err := json.Unmarshal(objects[generation.Views["chat"].Hash], &directory); err != nil {
			test.Fatal(err)
		}
		targetHash := generation.Views["chat"].Hash
		if target == "page" {
			targetHash = directory.Children[0].Hash
		}
		for _, damage := range []string{"missing", "load-error", "hash-mismatch", "noncanonical", "unknown-field", "duplicate-field", "empty", "oversized", "invalid-json"} {
			test.Run(target+"/"+damage, func(test *testing.T) {
				candidate := docVisibleTestClone(generation)
				changed := bytes.Clone(objects[targetHash])
				wantError := ErrIntegrity
				rehash := true
				switch damage {
				case "missing":
					wantError, rehash = ErrNotFound, false
				case "load-error":
					wantError, rehash = errors.New("visible object load failed"), false
				case "hash-mismatch":
					changed, rehash = []byte("{}"), false
				case "noncanonical":
					changed = append([]byte(" "), changed...)
				case "unknown-field":
					changed = append(changed[:len(changed)-1], []byte(`,"unknown":0}`)...)
				case "duplicate-field":
					field := `,"level":0}`
					if target == "page" {
						field = `,"start":0}`
					}
					changed = append(changed[:len(changed)-1], []byte(field)...)
				case "empty":
					changed = nil
				case "oversized":
					changed = bytes.Repeat([]byte(" "), MaxDocVisibleObjectBytes+1)
				case "invalid-json":
					changed = []byte("{")
				}
				replacementHash := targetHash
				var replacementDirectory []byte
				if rehash {
					replacementHash = HashContent(changed)
					root := candidate.Views["chat"]
					if target == "directory" {
						root.Hash = replacementHash
					} else {
						var parent docVisibleDirectory
						if err := json.Unmarshal(objects[root.Hash], &parent); err != nil {
							test.Fatal(err)
						}
						parent.Children[0].Hash = replacementHash
						replacementDirectory = docVisibleTestJSON(test, parent)
						root.Hash = HashContent(replacementDirectory)
					}
					candidate.Views["chat"] = root
				}
				load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
					if hash == replacementHash {
						if damage == "missing" || damage == "load-error" {
							return nil, wantError
						}
						return changed, nil
					}
					if replacementDirectory != nil && hash == candidate.Views["chat"].Hash {
						return replacementDirectory, nil
					}
					return docRangeGenerationLoad(objects)(ctx, hash)
				}
				unit, ordinal, err := ReadDocVisibleUnit(context.Background(), candidate, "chat", 1, true, load)
				if !errors.Is(err, wantError) || unit != (DocVisibleUnit{}) || ordinal != 0 {
					test.Fatalf("damaged object accepted: %+v/%d, %v; want %v", unit, ordinal, err, wantError)
				}
			})
		}
	}
	for _, mode := range []string{"all", "chat", "prompts", "unknown"} {
		for _, byEvent := range []bool{false, true} {
			for _, position := range []int{-1, generation.Events, generation.Events + 1} {
				loads := 0
				_, _, err := ReadDocVisibleUnit(context.Background(), generation, mode, position, byEvent, func(context.Context, ContentHash) ([]byte, error) {
					loads++
					return nil, ErrNotFound
				})
				if !errors.Is(err, ErrValidation) || loads != 0 {
					test.Fatalf("invalid seek %s/%d/%t loaded %d objects: %v", mode, position, byEvent, loads, err)
				}
			}
		}
	}
	if _, _, err := ReadDocVisibleUnit(context.Background(), generation, "chat", 0, false, nil); !errors.Is(err, ErrValidation) {
		test.Fatal("nil loader accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	loads := 0
	_, _, err := ReadDocVisibleUnit(ctx, generation, "chat", 0, false, func(context.Context, ContentHash) ([]byte, error) {
		loads++
		cancel()
		return objects[generation.Views["chat"].Hash], nil
	})
	if !errors.Is(err, context.Canceled) || loads != 1 {
		test.Fatalf("canceled object load continued: %d loads, %v", loads, err)
	}
	_, _, err = ReadDocVisibleUnit(ctx, generation, "chat", 0, false, func(context.Context, ContentHash) ([]byte, error) {
		loads++
		return nil, ErrNotFound
	})
	if !errors.Is(err, context.Canceled) || loads != 1 {
		test.Fatalf("pre-canceled seek loaded objects: %d, %v", loads, err)
	}
}

func TestDocVisibleGenerationRequiresVerifiedPresentationMetadata(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), {Kind: EventReasoning}, docVisibleTestMessage(RoleAssistant, "answer")})
	missingPresentation := fixture.streamed
	missingPresentation.presentationAlgorithm = 0
	futurePresentation := fixture.streamed
	futurePresentation.presentationAlgorithm = DocVisibleAlgorithm + 1
	missingReader := fixture.streamed
	missingReader.read = nil
	for name, verified := range map[string]VerifiedDocRangeIndex{"zero": {}, "header-only": {index: fixture.streamed.Header()}, "legacy": missingPresentation, "future": futurePresentation, "missing-reader": missingReader} {
		test.Run(name, func(test *testing.T) {
			proof, objects, err := BuildVerifiedDocRangeGeneration(context.Background(), verified)
			if err != nil {
				if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(proof, VerifiedDocRangeGeneration{}) || len(objects) != 0 {
					test.Fatalf("invalid index factory returned proof or objects: %+v, %d objects, %v", proof.Descriptor(), len(objects), err)
				}
				return
			}
			emitted := 0
			generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error {
				emitted++
				return nil
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || emitted != 0 {
				test.Fatalf("unverified presentation emitted %d objects: %+v, %v", emitted, generation, err)
			}
		})
	}
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	invalidHash := proof
	invalidHash.hash = "bad"
	for _, invalid := range []struct {
		name  string
		proof VerifiedDocRangeGeneration
		emit  func(context.Context, DocRangeObject) error
	}{
		{name: "nil-emitter", proof: proof},
		{name: "invalid-ranges", proof: invalidHash, emit: func(context.Context, DocRangeObject) error {
			test.Fatal("invalid range binding emitted an object")
			return nil
		}},
	} {
		test.Run(invalid.name, func(test *testing.T) {
			generation, err := BuildDocVisibleGeneration(context.Background(), invalid.proof, invalid.emit)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) {
				test.Fatalf("invalid builder arguments returned generation: %+v, %v", generation, err)
			}
		})
	}
	for _, failure := range []string{"page-load", "page-hash", "rendered-rank"} {
		test.Run(failure, func(test *testing.T) {
			verified := fixture.streamed
			verified.index = fixture.streamed.Header()
			raw := docRangeTestPage(test, fixture.streamed, 0)
			want := ErrIntegrity
			switch failure {
			case "page-load":
				want = errors.New("source metadata page unavailable")
			case "page-hash":
				raw[0] = 'x'
			case "rendered-rank":
				entries := append([]DocRangeEntry(nil), fixture.entries...)
				entries[2].RenderedBefore = 0
				raw = docVisibleTestJSON(test, entries)
				verified.index.Pages[0] = HashContent(raw)
			}
			verified.read = func(context.Context, int) ([]byte, error) {
				if failure == "page-load" {
					return nil, want
				}
				return raw, nil
			}
			proof := docVisibleTestRangeProof(test, verified)
			generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
			if !errors.Is(err, want) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) {
				test.Fatalf("bad metadata returned a generation: %+v, %v", generation, err)
			}
		})
	}
}

func TestDocVisibleGenerationSealedRangeBindingOwnsDescriptor(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRendererEvents())
	proof, objects, err := BuildVerifiedDocRangeGeneration(context.Background(), fixture.streamed)
	if err != nil {
		test.Fatal(err)
	}
	want, wantObjects, err := BuildDocRangeGeneration(context.Background(), fixture.streamed)
	if err != nil || !reflect.DeepEqual(proof.Descriptor(), want) || !reflect.DeepEqual(objects, wantObjects) {
		test.Fatalf("sealed factory changed existing range generation or objects: %v", err)
	}
	raw, err := want.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocRangeGeneration(want.Hash, HashContent(raw), raw)
	if err != nil || !reflect.DeepEqual(decoded, want) {
		test.Fatalf("sealed descriptor does not match authenticated range descriptor: %v", err)
	}
	copy := proof.Descriptor()
	copy.Envelope[0] = 'x'
	copy.EventPages.Hash = HashContent([]byte("caller replacement event directory"))
	copy.ChunkPages.Hash = HashContent([]byte("caller replacement chunk directory"))
	copy.Hash = HashContent([]byte("caller replacement document"))
	for _, object := range objects {
		object.Bytes[0] = 'x'
	}
	if !reflect.DeepEqual(proof.Descriptor(), want) {
		test.Fatal("caller-owned descriptor or emitted bytes changed sealed range generation")
	}
	generation, err := BuildDocVisibleGeneration(context.Background(), proof, func(context.Context, DocRangeObject) error { return nil })
	if err != nil || generation.Hash != want.Hash || generation.RangeGeneration != HashContent(raw) {
		test.Fatalf("visible generation lost sealed index/descriptor binding: %+v, %v", generation, err)
	}
	otherFixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "different document")})
	otherProof := docVisibleTestRangeProof(test, otherFixture.streamed)
	otherRaw, err := otherProof.Descriptor().Encode()
	if err != nil {
		test.Fatal(err)
	}
	other, err := BuildDocVisibleGeneration(context.Background(), otherProof, func(context.Context, DocRangeObject) error { return nil })
	if err != nil || other.Hash == generation.Hash || other.RangeGeneration == generation.RangeGeneration || other.RangeGeneration != HashContent(otherRaw) {
		test.Fatalf("distinct sealed documents shared a range binding: %+v, %v", other, err)
	}
}

func TestDocVisibleGenerationRejectsUnverifiedAndDecodedWrappers(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt")})
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	descriptor := proof.Descriptor()
	raw, err := descriptor.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocRangeGeneration(descriptor.Hash, HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
	encodedProof, err := json.Marshal(proof)
	if err != nil {
		test.Fatal(err)
	}
	wrappers := map[string]VerifiedDocRangeGeneration{
		"zero":                          {},
		"unverified-descriptor":         {generation: descriptor},
		"decoded-descriptor-with-hash":  {generation: decoded, hash: HashContent(raw)},
		"index-only":                    {index: fixture.streamed},
		"unsealed-index-and-descriptor": {index: fixture.streamed, generation: decoded},
	}
	for name, input := range map[string][]byte{"json-proof": encodedProof, "json-descriptor": raw, "json-empty": []byte("{}"), "json-null": []byte("null")} {
		var wrapper VerifiedDocRangeGeneration
		if err := json.Unmarshal(input, &wrapper); err != nil {
			test.Fatal(err)
		}
		if !reflect.DeepEqual(wrapper, VerifiedDocRangeGeneration{}) {
			test.Fatalf("%s created private verification state", name)
		}
		wrappers[name] = wrapper
	}
	for name, wrapper := range wrappers {
		test.Run(name, func(test *testing.T) {
			emitted := 0
			generation, err := BuildDocVisibleGeneration(context.Background(), wrapper, func(context.Context, DocRangeObject) error {
				emitted++
				return nil
			})
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || emitted != 0 {
				test.Fatalf("unverified wrapper emitted %d objects or returned a generation: %+v, %v", emitted, generation, err)
			}
		})
	}
}

func TestDocVisibleGenerationSealedFactoryCancellationReturnsNoProof(test *testing.T) {
	fixture := docVisibleTestFixture(test, docVisibleRendererEvents())
	checks := 0
	_, _, err := BuildVerifiedDocRangeGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, fixture.streamed)
	if err != nil || checks < 2 {
		test.Fatalf("sealed factory cancellation checkpoints: %d, %v", checks, err)
	}
	for cancelAt := 1; cancelAt <= checks; cancelAt++ {
		test.Run(fmt.Sprint(cancelAt), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := 0
			proof, objects, err := BuildVerifiedDocRangeGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
				seen++
				if seen == cancelAt {
					cancel()
				}
			}}, fixture.streamed)
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(proof, VerifiedDocRangeGeneration{}) || len(objects) != 0 {
				test.Fatalf("canceled factory returned proof or objects: %+v, %d objects, %v", proof.Descriptor(), len(objects), err)
			}
		})
	}
}

func TestDocVisibleUnitRejectsMissingAnchorAtTurnStart(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "first prompt"), docVisibleTestMessage(RoleUser, "second prompt")})
	generation, objects := docVisibleTestBuild(test, fixture.streamed)
	for _, mode := range []string{"all", "chat", "prompts"} {
		test.Run(mode, func(test *testing.T) {
			for _, position := range []int{0, 1} {
				unit, _, err := ReadDocVisibleUnit(context.Background(), generation, mode, position, true, docRangeGenerationLoad(objects))
				if err != nil {
					test.Fatal(err)
				}
				unit.EventIndex = -1
				if unit.TurnIndex != unit.From || !errors.Is(unit.Validate(generation.Events, mode), ErrIntegrity) {
					test.Fatalf("turn-start anchor removal validated: %+v", unit)
				}
				clipped, err := unit.Clip(unit.From, unit.RenderedBefore, generation.Events, mode)
				if !errors.Is(err, ErrIntegrity) || clipped != (DocVisibleUnit{}) {
					test.Fatalf("turn-start anchor removal clipped successfully: %+v, %v", clipped, err)
				}
			}
			var directory docVisibleDirectory
			if err := json.Unmarshal(objects[generation.Views[mode].Hash], &directory); err != nil {
				test.Fatal(err)
			}
			var page docVisiblePage
			if err := json.Unmarshal(objects[directory.Children[0].Hash], &page); err != nil {
				test.Fatal(err)
			}
			page.Units[1].EventIndex = -1
			rawPage := docVisibleTestJSON(test, page)
			directory.Children[0].Hash = HashContent(rawPage)
			directory.Children[0].Rendered--
			rawDirectory := docVisibleTestJSON(test, directory)
			candidate := docVisibleTestClone(generation)
			candidate.Rendered--
			root := candidate.Views[mode]
			root.Hash = HashContent(rawDirectory)
			candidate.Views[mode] = root
			for _, byEvent := range []bool{false, true} {
				unit, ordinal, err := ReadDocVisibleUnit(context.Background(), candidate, mode, 1, byEvent, func(_ context.Context, hash ContentHash) ([]byte, error) {
					switch hash {
					case root.Hash:
						return rawDirectory, nil
					case directory.Children[0].Hash:
						return rawPage, nil
					default:
						return nil, ErrNotFound
					}
				})
				if !errors.Is(err, ErrIntegrity) || unit != (DocVisibleUnit{}) || ordinal != 0 {
					test.Fatalf("authenticated page missing turn anchor accepted: %+v/%d, %v", unit, ordinal, err)
				}
			}
		})
	}
}

func TestDocVisibleUnitReadCancellationReturnsEmptyResults(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "first prompt"), docVisibleTestMessage(RoleUser, "second prompt"), docVisibleTestMessage(RoleAssistant, "answer")})
	generation, objects := docVisibleTestBuild(test, fixture.streamed)
	for _, mode := range []string{"all", "chat", "prompts"} {
		for _, byEvent := range []bool{false, true} {
			position := generation.Views[mode].Units - 1
			if byEvent {
				position = generation.Events - 1
			}
			checks := 0
			unit, ordinal, err := ReadDocVisibleUnit(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, generation, mode, position, byEvent, docRangeGenerationLoad(objects))
			if err != nil || checks < 2 || ordinal == 0 || unit == (DocVisibleUnit{}) {
				test.Fatalf("read cancellation baseline: %+v/%d, %d checkpoints, %v", unit, ordinal, checks, err)
			}
			for cancelAt := 1; cancelAt <= checks; cancelAt++ {
				test.Run(fmt.Sprintf("%s/by-event-%t/checkpoint-%d-of-%d", mode, byEvent, cancelAt, checks), func(test *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					seen := 0
					unit, ordinal, err := ReadDocVisibleUnit(canonicalAdmissionContext{Context: ctx, check: func() {
						seen++
						if seen == cancelAt {
							cancel()
						}
					}}, generation, mode, position, byEvent, docRangeGenerationLoad(objects))
					if !errors.Is(err, context.Canceled) || unit != (DocVisibleUnit{}) || ordinal != 0 {
						test.Fatalf("canceled read returned visible unit or ordinal: %+v/%d, %v", unit, ordinal, err)
					}
				})
			}
		}
	}
}

func TestDocVisibleGenerationCancellationAndEmitFailuresReturnNoGeneration(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), {Kind: EventReasoning}, {Kind: EventToolResult, Output: "work"}, docVisibleTestMessage(RoleAssistant, "answer")})
	proof := docVisibleTestRangeProof(test, fixture.streamed)
	checks, objects := 0, 0
	_, err := BuildDocVisibleGeneration(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, proof, func(context.Context, DocRangeObject) error {
		objects++
		return nil
	})
	if err != nil || checks < 2 || objects < 2 {
		test.Fatalf("baseline build: %d checks/%d objects, %v", checks, objects, err)
	}
	for cancelAt := 1; cancelAt <= checks; cancelAt++ {
		test.Run(fmt.Sprintf("checkpoint-%d", cancelAt), func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := 0
			generation, err := BuildDocVisibleGeneration(canonicalAdmissionContext{Context: ctx, check: func() {
				seen++
				if seen == cancelAt {
					cancel()
				}
			}}, proof, func(context.Context, DocRangeObject) error { return nil })
			if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) {
				test.Fatalf("checkpoint %d returned a generation: %+v, %v", cancelAt, generation, err)
			}
		})
	}
	for _, failure := range []string{"emit-error", "emit-cancellation"} {
		for failAt := 1; failAt <= objects; failAt++ {
			test.Run(fmt.Sprintf("%s-%d", failure, failAt), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				seen := 0
				want := errors.New("object storage failed")
				if failure == "emit-cancellation" {
					want = context.Canceled
				}
				generation, err := BuildDocVisibleGeneration(ctx, proof, func(context.Context, DocRangeObject) error {
					seen++
					if seen == failAt {
						if failure == "emit-cancellation" {
							cancel()
							return nil
						}
						return want
					}
					return nil
				})
				if !errors.Is(err, want) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || seen != failAt {
					test.Fatalf("failed emitter continued or returned a generation: %d/%d, %+v, %v", seen, failAt, generation, err)
				}
			})
		}
	}
}

func TestDocVisibleGenerationStreamingPageFailureStopsBeforeNextSourcePage(test *testing.T) {
	events := make([]CIREvent, DocRangePageEvents+1)
	for position := range events {
		events[position] = docVisibleTestMessage(RoleUser, "prompt")
	}
	fixture := docVisibleTestFixture(test, events)
	for _, failure := range []string{"emit-error", "emit-cancellation"} {
		test.Run(failure, func(test *testing.T) {
			verified := fixture.streamed
			read := verified.read
			pages, emitted := 0, 0
			verified.read = func(ctx context.Context, position int) ([]byte, error) {
				pages++
				return read(ctx, position)
			}
			proof := docVisibleTestRangeProof(test, verified)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("streaming page emit failed")
			if failure == "emit-cancellation" {
				want = context.Canceled
			}
			generation, err := BuildDocVisibleGeneration(ctx, proof, func(context.Context, DocRangeObject) error {
				emitted++
				if failure == "emit-cancellation" {
					cancel()
					return nil
				}
				return want
			})
			if !errors.Is(err, want) || !reflect.DeepEqual(generation, DocVisibleGeneration{}) || emitted != 1 || pages != 1 {
				test.Fatalf("streaming failure continued or returned generation: pages=%d emitted=%d, %+v, %v", pages, emitted, generation, err)
			}
		})
	}
}

func TestDocVisibleUnitRejectsInvalidClipping(test *testing.T) {
	fixture := docVisibleTestFixture(test, []CIREvent{docVisibleTestMessage(RoleUser, "prompt"), {Kind: EventReasoning}, {Kind: EventToolResult, Output: "work"}, docVisibleTestMessage(RoleAssistant, "answer")})
	generation, objects := docVisibleTestBuild(test, fixture.streamed)
	unit, _, err := ReadDocVisibleUnit(context.Background(), generation, "prompts", 0, false, docRangeGenerationLoad(objects))
	if err != nil {
		test.Fatal(err)
	}
	for _, invalid := range []struct {
		start int
		rank  int
	}{
		{start: -1, rank: 0}, {start: unit.To, rank: unit.RenderedEnd()},
		{start: unit.From, rank: unit.RenderedBefore + 1}, {start: 1, rank: -1},
		{start: 1, rank: unit.RenderedEnd() + 1}, {start: 1, rank: 2},
		{start: 1, rank: unit.RenderedBefore},
	} {
		clipped, err := unit.Clip(invalid.start, invalid.rank, generation.Events, "prompts")
		if !errors.Is(err, ErrIntegrity) || clipped != (DocVisibleUnit{}) {
			test.Fatalf("invalid clipping %d/%d accepted: %+v, %v", invalid.start, invalid.rank, clipped, err)
		}
	}
	if _, err := unit.Clip(0, 0, generation.Events, "unknown"); !errors.Is(err, ErrValidation) {
		test.Fatal("unknown clip mode accepted", err)
	}
}

func TestDocVisibleGenerationHiddenGrowthHasBoundedBodyFreeSeeks(test *testing.T) {
	loadCounts := make(map[int]map[string]int)
	for _, hidden := range []int{2000, 20000} {
		test.Run(fmt.Sprint(hidden), func(test *testing.T) {
			const privateBody = "private-source-body-must-not-appear-in-visible-index"
			events := []CIREvent{docVisibleTestMessage(RoleUser, "prompt")}
			for position := 0; position < hidden; position++ {
				if position%7 == 0 {
					events = append(events, CIREvent{Kind: EventReasoning, RedactedSummary: " \t "})
				} else {
					events = append(events, CIREvent{Kind: EventToolResult, CallID: "tool", Output: privateBody})
				}
			}
			events = append(events, docVisibleTestMessage(RoleAssistant, "answer"), docVisibleTestMessage(RoleUser, "next prompt"))
			fixture := docVisibleTestFixture(test, events)
			sourceReads := *fixture.sourceReads
			generation, objects := docVisibleTestBuild(test, fixture.streamed)
			for hash, raw := range objects {
				if bytes.Contains(raw, []byte(privateBody)) || fixture.sourceHashes[hash] != nil {
					test.Fatal("visible generation contains source bodies")
				}
			}
			loadCounts[hidden] = make(map[string]int)
			for _, mode := range []string{"all", "chat", "prompts"} {
				want := docVisibleReference(fixture.events, mode)
				for _, position := range []int{0, 1, hidden / 2, hidden, hidden + 1, hidden + 2} {
					ordinal := 0
					for want[ordinal].To <= position {
						ordinal++
					}
					for _, byEvent := range []bool{false, true} {
						seek := ordinal
						if byEvent {
							seek = position
						}
						loads := 0
						got, gotOrdinal, err := ReadDocVisibleUnit(context.Background(), generation, mode, seek, byEvent, func(ctx context.Context, hash ContentHash) ([]byte, error) {
							loads++
							if fixture.sourceHashes[hash] != nil {
								test.Fatal("visible seek attempted to load source body")
							}
							return docRangeGenerationLoad(objects)(ctx, hash)
						})
						bound := 2
						if mode == "all" && hidden > DocVisiblePageUnits*DocRangeDirectoryFanout {
							bound++
						}
						if err != nil || got != want[ordinal] || gotOrdinal != ordinal || loads != bound {
							test.Fatalf("%s seek %d/%t: %+v/%d, %v, %d loads; want %+v/%d with %d loads", mode, seek, byEvent, got, gotOrdinal, err, loads, want[ordinal], ordinal, bound)
						}
						loadCounts[hidden][mode] = max(loadCounts[hidden][mode], loads)
					}
				}
			}
			if *fixture.sourceReads != sourceReads {
				test.Fatalf("building/seeking visible metadata loaded %d source bodies", *fixture.sourceReads-sourceReads)
			}
		})
	}
	for _, mode := range []string{"chat", "prompts"} {
		if loadCounts[2000][mode] != 2 || loadCounts[20000][mode] != loadCounts[2000][mode] {
			test.Fatalf("10x hidden growth increased %s seek cost: %v", mode, loadCounts)
		}
	}
	if loadCounts[20000]["all"] > loadCounts[2000]["all"]+1 {
		test.Fatalf("all-mode seek cost exceeded logarithmic growth: %v", loadCounts)
	}
}
