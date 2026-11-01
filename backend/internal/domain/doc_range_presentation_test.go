package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDocRangePresentationClassification(t *testing.T) {
	for _, test := range []struct {
		name           string
		body           string
		seed           bool
		emptyReasoning bool
		compact        bool
	}{
		{name: "branch seed", body: `{"kind":"message","blocks":[{"type":"text","text":"[cxt seed] Branch-switch context: details"}]}`, seed: true},
		{name: "resumed seed", body: `{"kind":"message","blocks":[{"text":"[cxt] This session was resumed from a branch context seed. more"}]}`, seed: true},
		{name: "whitespace across blocks", body: `{"kind":"message","blocks":[{}, {"text":null}, {"text":" \t\r\n\u00a0\u2009"}, {"text":"\ufeff\u2028\u2029\u3000[cxt seed] Branch-switch context:"}]}`, seed: true},
		{name: "escaped keys and prefix", body: `{"k\u0069nd":"message","bl\u006fcks":[{"te\u0078t":"\u005bcxt\u0020seed\u005d Branch-switch context\u003a"}]}`, seed: true},
		{name: "short text key ignored", body: `{"kind":"message","blocks":[{"tex":"[cxt seed] Branch-switch context:"}]}`},
		{name: "escaped near-match text key ignored", body: `{"kind":"message","blocks":[{"te\u0078ts":"[cxt seed] Branch-switch context:","text":"ordinary"}]}`},
		{name: "text key with null suffix ignored", body: `{"kind":"message","blocks":[{"text\u0000":"[cxt seed] Branch-switch context:"}]}`},
		{name: "near-match key does not override text", body: `{"kind":"message","blocks":[{"text":"[cxt seed] Branch-switch context:","texts":"ordinary"}]}`, seed: true},
		{name: "raw unicode whitespace", body: "{\"kind\":\"message\",\"blocks\":[{\"text\":\"\u00a0\u1680\u2000\u202f\u205f\ufeff[cxt seed] Branch-switch context:\"}]}", seed: true},
		{name: "prefix split by block separator", body: `{"kind":"message","blocks":[{"text":"[cxt seed] Branch-"},{"text":"switch context:"}]}`},
		{name: "prefix split at space", body: `{"kind":"message","blocks":[{"text":"[cxt seed]"},{"text":" Branch-switch context:"}]}`},
		{name: "later seed is not a prefix", body: `{"kind":"message","blocks":[{"text":"first"},{"text":"[cxt seed] Branch-switch context:"}]}`},
		{name: "incomplete prefix", body: `{"kind":"message","blocks":[{"text":"[cxt seed] Branch-switch context"}]}`},
		{name: "escaped literal is not whitespace", body: `{"kind":"message","blocks":[{"text":"\\n[cxt seed] Branch-switch context:"}]}`},
		{name: "unicode next line is not JS whitespace", body: `{"kind":"message","blocks":[{"text":"\u0085[cxt seed] Branch-switch context:"}]}`},
		{name: "zero width is not whitespace", body: `{"kind":"message","blocks":[{"text":"\u200b[cxt seed] Branch-switch context:"}]}`},
		{name: "surrogate pair is not whitespace", body: `{"kind":"message","blocks":[{"text":"\ud83d\ude42[cxt seed] Branch-switch context:"}]}`},
		{name: "unpaired surrogate is not whitespace", body: `{"kind":"message","blocks":[{"text":"\ud800[cxt seed] Branch-switch context:"}]}`},
		{name: "nested text ignored", body: `{"kind":"message","blocks":[{"other":{"text":"[cxt seed] Branch-switch context:"},"text":"ordinary"}]}`},
		{name: "nested braces and quotes", body: `{"kind":"message","blocks":[{"other":[{"text":"escaped \" } ] \\"}],"text":"[cxt seed] Branch-switch context:"}]}`, seed: true},
		{name: "compact without seed", body: `{"kind":"message","compact_summary":true,"blocks":[{"text":"ordinary summary"}]}`, compact: true},
		{name: "compact seed", body: `{"kind":"message","compact_summary":true,"blocks":[{"text":"[cxt seed] Branch-switch context:"}]}`, compact: true, seed: true},
		{name: "no blocks", body: `{"kind":"message"}`},
		{name: "null blocks", body: `{"kind":"message","blocks":null}`},
		{name: "empty blocks", body: `{"kind":"message","blocks":[]}`},
		{name: "all empty blocks", body: `{"kind":"message","blocks":[{"text":""},{"text":" \n\ufeff"}]}`},
		{name: "reasoning omitted", body: `{"kind":"reasoning"}`, emptyReasoning: true},
		{name: "reasoning null", body: `{"kind":"reasoning","redacted_summary":null}`, emptyReasoning: true},
		{name: "reasoning empty", body: `{"kind":"reasoning","redacted_summary":""}`, emptyReasoning: true},
		{name: "reasoning unicode whitespace", body: `{"kind":"reasoning","redacted_summary":" \t\n\r\f\u000b\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"}`, emptyReasoning: true},
		{name: "reasoning visible", body: `{"kind":"reasoning","redacted_summary":" \ufeffvisible \n"}`},
		{name: "reasoning next line visible", body: `{"kind":"reasoning","redacted_summary":"\u0085"}`},
		{name: "reasoning escaped quote visible", body: `{"kind":"reasoning","redacted_summary":"\""}`},
		{name: "reasoning backspace visible", body: `{"kind":"reasoning","redacted_summary":"\b"}`},
		{name: "reasoning escaped slash visible", body: `{"kind":"reasoning","redacted_summary":"\/"}`},
		{name: "reasoning emoji visible", body: `{"kind":"reasoning","redacted_summary":"\ud83d\ude42"}`},
		{name: "reasoning unicode visible", body: "{\"kind\":\"reasoning\",\"redacted_summary\":\"\ud55c\uae00\"}"},
		{name: "tool seed is not message", body: `{"kind":"tool_result","blocks":[{"text":"[cxt seed] Branch-switch context:"}],"redacted_summary":""}`},
		{name: "nested replacement seed ignored", body: `{"kind":"compaction","replacement":[{"kind":"message","blocks":[{"text":"[cxt seed] Branch-switch context:"}]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			before := bytes.Clone(body)
			entry, err := docRangeEntry(3, 11, body, 17)
			if err != nil {
				t.Fatal(err)
			}
			if entry.SeedSummary != test.seed || entry.EmptyReasoning != test.emptyReasoning || entry.CompactSummary != test.compact {
				t.Fatalf("unexpected presentation: %+v", entry)
			}
			if !bytes.Equal(body, before) || entry.Hash != HashContent(before) || entry.Length != len(body) || entry.Seq != 17 || entry.Index != 3 || entry.Offset != 11 {
				t.Fatal("classification changed archive identity or coordinates")
			}
		})
	}
}

func TestDocRangePresentationRejectsUnverifiedFieldShapes(t *testing.T) {
	message, err := canonicalJSON(CIREvent{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "[cxt seed] Branch-switch context:"}}})
	if err != nil {
		t.Fatal(err)
	}
	reasoning, err := canonicalJSON(CIREvent{Kind: EventReasoning, RedactedSummary: "visible"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := conversationTestEnvelope(t, false)
	for _, body := range [][]byte{message, reasoning} {
		manifest, bodies := conversationTestPlan(t, envelope, body, 1)
		var verifier CanonicalDocVerifier
		index, err := verifier.VerifyConversationRangeIndex(context.Background(), manifestProofHash(t, manifest), manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
		if err != nil || index.presentationAlgorithm != 1 {
			t.Fatal("canonical control did not seal", err)
		}
	}
	for _, test := range []struct {
		name      string
		body      []byte
		old       string
		changed   string
		malformed bool
	}{
		{name: "duplicate text", body: message, old: `"text":`, changed: `"text":"ordinary","text":`},
		{name: "escaped duplicate text", body: message, old: `"text":`, changed: `"text":"ordinary","te\u0078t":`},
		{name: "duplicate blocks", body: message, old: `"blocks":`, changed: `"blocks":[],"blocks":`},
		{name: "escaped duplicate blocks", body: message, old: `"blocks":`, changed: `"blocks":[],"bl\u006fcks":`},
		{name: "duplicate summary", body: reasoning, old: `"redacted_summary":"visible"`, changed: `"redacted_summary":"visible","redacted_summary":""`},
		{name: "escaped duplicate summary", body: reasoning, old: `"redacted_summary":"visible"`, changed: `"redacted_summary":"visible","redacted\u005fsummary":""`},
		{name: "near-match text key", body: message, old: `"text":`, changed: `"texts":`},
		{name: "escaped near-match text key", body: message, old: `"text":`, changed: `"te\u0078ts":`},
		{name: "near-match summary key", body: reasoning, old: `"redacted_summary":`, changed: `"redacted_summary_extra":`},
		{name: "blocks object", body: message, old: `[{"text":"[cxt seed] Branch-switch context:","type":"text"}]`, changed: `{}`},
		{name: "null block", body: message, old: `{"text":"[cxt seed] Branch-switch context:","type":"text"}`, changed: `null`},
		{name: "text number", body: message, old: `"text":"[cxt seed] Branch-switch context:"`, changed: `"text":0`},
		{name: "text object", body: message, old: `"text":"[cxt seed] Branch-switch context:"`, changed: `"text":{}`},
		{name: "text null", body: message, old: `"text":"[cxt seed] Branch-switch context:"`, changed: `"text":null`},
		{name: "summary array", body: reasoning, old: `"redacted_summary":"visible"`, changed: `"redacted_summary":[]`},
		{name: "summary bool", body: reasoning, old: `"redacted_summary":"visible"`, changed: `"redacted_summary":false`},
		{name: "summary null", body: reasoning, old: `"redacted_summary":"visible"`, changed: `"redacted_summary":null`},
		{name: "malformed text escape", body: message, old: `[cxt seed] Branch-switch context:`, changed: `\q`, malformed: true},
		{name: "malformed text unicode escape", body: message, old: `[cxt seed] Branch-switch context:`, changed: `\u00x0`, malformed: true},
		{name: "truncated summary unicode escape", body: reasoning, old: `visible`, changed: `\u12`, malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := bytes.Replace(test.body, []byte(test.old), []byte(test.changed), 1)
			if bytes.Equal(body, test.body) || json.Valid(body) == test.malformed {
				t.Fatal("invalid mutation fixture")
			}
			before := bytes.Clone(body)
			if test.malformed {
				if _, err := docRangeEntry(0, 0, body, 0); err == nil {
					t.Fatal("malformed escape reached presentation scanning")
				}
			}
			manifest, bodies := conversationTestPlan(t, envelope, body, 1)
			hash := manifestProofHash(t, manifest)
			buffer := &docRangeMemoryBuffer{}
			var verifier CanonicalDocVerifier
			index, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, manifestProofLoad(bodies), buffer)
			if err == nil || index.presentationAlgorithm != 0 || index.read != nil || index.Header().Hash != "" || len(buffer.pages) != 0 || len(verifier.events) != 0 {
				t.Fatal("noncanonical or wrongly typed event obtained a presentation proof", err)
			}
			document, err := verifier.VerifyConversationManifestDoc(context.Background(), hash, manifest, manifestProofLoad(bodies))
			if err == nil || document.Valid() || len(verifier.events) != 0 {
				t.Fatal("noncanonical or wrongly typed event obtained a document proof", err)
			}
			if !bytes.Equal(body, before) {
				t.Fatal("rejected event bytes were mutated")
			}
		})
	}
}

func TestDocRangePresentationLargeFieldsHaveBoundedAllocations(t *testing.T) {
	for _, test := range []struct {
		name           string
		body           []byte
		seed           bool
		emptyReasoning bool
	}{
		{name: "large seed payload", body: []byte(`{"kind":"message","blocks":[{"text":"[cxt seed] Branch-switch context:` + strings.Repeat("x", 4<<20) + `"}]}`), seed: true},
		{name: "large leading whitespace", body: []byte(`{"kind":"message","blocks":[{"text":"` + strings.Repeat(`\u2000\ufeff `, 256<<10) + `[cxt seed] Branch-switch context:"}]}`), seed: true},
		{name: "large nonseed payload", body: []byte(`{"kind":"message","blocks":[{"text":"` + strings.Repeat("x", 4<<20) + `"}]}`)},
		{name: "large empty reasoning", body: []byte(`{"kind":"reasoning","redacted_summary":"` + strings.Repeat(`\u2000\ufeff `, 256<<10) + `"}`), emptyReasoning: true},
		{name: "large visible reasoning", body: []byte(`{"kind":"reasoning","redacted_summary":"` + strings.Repeat(" ", 4<<20) + `visible"}`)},
		{name: "many whitespace blocks", body: []byte(`{"kind":"message","blocks":[` + strings.Repeat(`{"te\u0078t":"\ufeff "},`, 32768) + `{"text":"[cxt seed] Branch-switch context:"}]}`), seed: true},
		{name: "large ignored field", body: []byte(`{"kind":"message","blocks":[{"unknown":"` + strings.Repeat("x", 4<<20) + `","text":"[cxt seed] Branch-switch context:"}]}`), seed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := func() {
				t.Helper()
				entry, err := docRangeEntry(0, 0, test.body, 0)
				if err != nil || entry.SeedSummary != test.seed || entry.EmptyReasoning != test.emptyReasoning {
					t.Fatalf("incorrect large-field classification: %+v, %v", entry, err)
				}
			}
			check()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for repeat := 0; repeat < 3; repeat++ {
				check()
			}
			runtime.ReadMemStats(&after)
			if allocated := (after.TotalAlloc - before.TotalAlloc) / 3; allocated > 64<<10 {
				t.Fatalf("classification allocated %d bytes per event", allocated)
			}
		})
	}
}

func TestDocRangePresentationRanksAndArchiveParity(t *testing.T) {
	events := make([]CIREvent, 2*DocRangePageEvents+5)
	for position := range events {
		event := CIREvent{Kind: EventMessage, Seq: position, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "ordinary"}}}
		switch position % 7 {
		case 0:
			event = CIREvent{Kind: EventReasoning, Seq: position, RedactedSummary: "\ufeff\u2028 \t"}
		case 1:
			event.Blocks = []ContentBlock{{Type: "text", Text: " \u00a0"}, {Type: "text", Text: "[cxt seed] Branch-switch context:"}}
		case 2:
			event = CIREvent{Kind: EventReasoning, Seq: position, RedactedSummary: "summary"}
		case 3:
			event.CompactSummary = true
		case 4:
			event = CIREvent{Kind: EventTurn, Seq: position, Role: RoleAssistant}
		case 5:
			event = CIREvent{Kind: EventToolCall, Seq: position, CallID: "call", ToolName: "test"}
		case 6:
			event = CIREvent{Kind: EventCompaction, Seq: position, Replacement: []CIREvent{}, ReplacementComplete: true}
		}
		if position == DocRangePageEvents-1 || position == DocRangePageEvents || position == 2*DocRangePageEvents-1 || position == 2*DocRangePageEvents {
			event = CIREvent{Kind: EventReasoning, Seq: position}
		}
		events[position] = event
	}
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: events}
	archiveBefore, err := CanonicalBytes(document)
	if err != nil {
		t.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(document)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	hash := manifestProofHash(t, manifest)
	var verifier CanonicalDocVerifier
	proof, err := verifier.VerifyConversationManifestDoc(context.Background(), hash, manifest, manifestProofLoad(bodies))
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := BuildDocRangeIndex(context.Background(), proof)
	if err != nil {
		t.Fatal(err)
	}
	streamed, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	if materialized.presentationAlgorithm != 1 || streamed.presentationAlgorithm != 1 || !reflect.DeepEqual(materialized.Header(), streamed.Header()) {
		t.Fatal("builders produced different presentation proofs")
	}
	header := streamed.Header()
	wantRanks := make([]int, len(events))
	rank := 0
	for position, event := range events {
		wantRanks[position] = rank
		if event.Kind != EventReasoning || event.RedactedSummary == "summary" {
			rank++
		}
	}
	for _, page := range []int{2, 0, 1, 2, 1} {
		raw := docRangeTestPage(t, streamed, page)
		if !bytes.Equal(raw, docRangeTestPage(t, materialized, page)) {
			t.Fatalf("page %d differs between streamed and materialized proofs", page)
		}
		entries, err := header.DecodePage(page, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			event := events[entry.Index]
			wantEmpty := event.Kind == EventReasoning && event.RedactedSummary != "summary"
			wantSeed := event.Kind == EventMessage && entry.Index%7 == 1
			if entry.RenderedBefore != wantRanks[entry.Index] || entry.EmptyReasoning != wantEmpty || entry.SeedSummary != wantSeed || entry.CompactSummary != event.CompactSummary {
				t.Fatalf("incorrect rank or flags: %+v", entry)
			}
			body, err := proof.EventStreamRange(context.Background(), entry.Offset, entry.Length)
			if err != nil || entry.Hash != HashContent(body) || bytes.Contains(body, []byte("seed_summary")) || bytes.Contains(body, []byte("empty_reasoning")) || bytes.Contains(body, []byte("rendered_before")) {
				t.Fatal("presentation metadata changed event identity", err)
			}
		}
	}
	archiveAfter, err := CanonicalBytes(document)
	if err != nil {
		t.Fatal(err)
	}
	manifestAfter, err := CanonicalConversationManifest(manifest)
	if err != nil || !bytes.Equal(manifestBefore, manifestAfter) || !bytes.Equal(archiveBefore, archiveAfter) || !bytes.Equal(proof.Bytes(), archiveBefore) || proof.Hash() != hash {
		t.Fatal("indexing changed canonical archive or manifest identity", err)
	}
	for chunkHash, body := range bodies {
		if HashContent(body) != chunkHash {
			t.Fatal("indexing mutated source chunks")
		}
	}
}

func TestDocRangePresentationPageLocalRanks(t *testing.T) {
	events := canonicalAdmissionEvents(0, 3*DocRangePageEvents)
	for position := range events {
		if position%3 == 0 {
			events[position] = CIREvent{Kind: EventReasoning, Seq: position}
		}
	}
	manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	doc, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(t, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		t.Fatal(err)
	}
	pages := &verifiedDocRangePages{doc: doc}
	builder := docRangeBuilder{ctx: context.Background(), buffer: pages, result: VerifiedDocRangeIndex{index: DocRangeIndex{Version: 1, Hash: doc.Hash(), Manifest: manifest, Pages: []ContentHash{}}}}
	for position, event := range doc.chunks.events {
		body, err := doc.EventStreamRange(context.Background(), event.span.offset, event.span.length)
		if err != nil {
			t.Fatal(err)
		}
		if err := builder.add(int64(position), body, event.seq); err != nil {
			t.Fatal(err)
		}
	}
	index, err := builder.seal()
	if err != nil || !reflect.DeepEqual(pages.renderedBefore, []int{0, 85, 170}) {
		t.Fatal("page rank checkpoints were not retained", pages.renderedBefore, err)
	}
	pages.doc.chunks.events[0].span.offset = -1
	if _, err := index.Page(context.Background(), 2); err != nil {
		t.Fatal("reading a later page rescanned earlier events", err)
	}
	if _, err := index.Page(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
		t.Fatal("test did not invalidate the earlier page", err)
	}
}

func TestDocRangePresentationDecodeChecksAndLegacyCompatibility(t *testing.T) {
	base := []DocRangeEntry{
		{DocEventIndex: DocEventIndex{Index: 0, Offset: 0, Length: 10, Hash: HashContent([]byte("first"))}, Kind: EventMessage},
		{DocEventIndex: DocEventIndex{Index: 1, Offset: 11, Length: 10, Hash: HashContent([]byte("second"))}, Kind: EventReasoning},
	}
	for _, test := range []struct {
		name   string
		change func([]DocRangeEntry)
		valid  bool
	}{
		{name: "legacy missing flags and ranks", valid: true},
		{name: "new flags and ranks", change: func(entries []DocRangeEntry) {
			entries[0].SeedSummary = true
			entries[1].EmptyReasoning = true
			entries[1].RenderedBefore = 1
		}, valid: true},
		{name: "visible reasoning", change: func(entries []DocRangeEntry) { entries[1].RenderedBefore = 1 }, valid: true},
		{name: "negative rank", change: func(entries []DocRangeEntry) { entries[1].RenderedBefore = -1 }},
		{name: "excessive rank", change: func(entries []DocRangeEntry) { entries[1].RenderedBefore = 2 }},
		{name: "nonzero first rank", change: func(entries []DocRangeEntry) { entries[0].RenderedBefore = 1 }},
		{name: "message cannot be empty reasoning", change: func(entries []DocRangeEntry) { entries[0].EmptyReasoning = true }},
		{name: "reasoning cannot be seed", change: func(entries []DocRangeEntry) { entries[1].SeedSummary = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			entries := append([]DocRangeEntry(nil), base...)
			if test.change != nil {
				test.change(entries)
			}
			raw, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			index := DocRangeIndex{Version: 1, Manifest: ConversationManifest{EventCount: 2, StreamBytes: 21}, Pages: []ContentHash{HashContent(raw)}}
			decoded, err := index.DecodePage(0, raw)
			if test.valid {
				if err != nil || !reflect.DeepEqual(decoded, entries) {
					t.Fatal("valid or legacy presentation metadata rejected", err)
				}
			} else if !errors.Is(err, ErrIntegrity) {
				t.Fatal("invalid presentation metadata accepted", err)
			}
		})
	}
}

func TestDocRangePresentationSealAuthority(t *testing.T) {
	manifest, bodies, _ := rootEventsFixture(t)
	hash := manifestProofHash(t, manifest)
	var verifier CanonicalDocVerifier
	failed := errors.New("page append failed")
	index, err := verifier.VerifyConversationRangeIndex(context.Background(), hash, manifest, manifestProofLoad(bodies), &docRangeFailingBuffer{err: failed})
	if !errors.Is(err, failed) || index.presentationAlgorithm != 0 || index.read != nil {
		t.Fatal("failed seal granted presentation authority", err)
	}
	index, err = BuildDocRangeIndex(context.Background(), VerifiedSessionDoc{})
	if !errors.Is(err, ErrUnsupportedDocumentIdentity) || index.presentationAlgorithm != 0 {
		t.Fatal("unverified document granted presentation authority", err)
	}
	for _, count := range []int{0, 1, DocRangePageEvents, DocRangePageEvents + 1} {
		events := make([]CIREvent, count)
		for position := range events {
			events[position] = CIREvent{Kind: EventReasoning, Seq: position}
		}
		manifest, bodies, err := ConversationManifestForCIR(CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: events})
		if err != nil {
			t.Fatal(err)
		}
		index, err := verifier.VerifyConversationRangeIndex(context.Background(), manifestProofHash(t, manifest), manifest, manifestProofLoad(bodies), &docRangeMemoryBuffer{})
		if err != nil || index.presentationAlgorithm != 1 {
			t.Fatal("successful seal lacks presentation authority", err)
		}
		for page := range index.index.Pages {
			entries, err := index.index.DecodePage(page, docRangeTestPage(t, index, page))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !entry.EmptyReasoning || entry.RenderedBefore != 0 {
					t.Fatal("empty reasoning advanced rendered rank")
				}
			}
		}
		var header DocRangeIndex
		raw, err := json.Marshal(index.Header())
		if err != nil || json.Unmarshal(raw, &header) != nil || (VerifiedDocRangeIndex{index: header}).presentationAlgorithm != 0 {
			t.Fatal("serialized header granted presentation authority", err)
		}
	}
}
