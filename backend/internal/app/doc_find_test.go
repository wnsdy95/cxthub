package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docFindFixture(test *testing.T, events []domain.CIREvent) (docViewSource, domain.DocFindSelector, *int, *int) {
	test.Helper()
	var bodies [][]byte
	var entries []domain.DocEventIndex
	offset := 0
	for index, event := range events {
		event.Seq = index
		raw, err := json.Marshal(event)
		if err != nil {
			test.Fatal(err)
		}
		bodies = append(bodies, raw)
		entries = append(entries, domain.DocEventIndex{Index: index, Seq: index, Offset: offset, Length: len(raw), Hash: domain.HashContent(raw)})
		offset += len(raw)
	}
	stream := strings.Join(func() []string {
		values := make([]string, len(bodies))
		for index, raw := range bodies {
			values[index] = string(raw)
		}
		return values
	}(), "")
	bytesRead, largestRead := 0, 0
	source := docViewSource{total: len(events), location: func(index int) (domain.DocEventIndex, error) {
		return entries[index], nil
	}, entry: func(index int) (domain.DocRangeEntry, error) {
		return domain.DocRangeEntry{DocEventIndex: entries[index], Kind: events[index].Kind,
			EmptyReasoning: events[index].Kind == domain.EventReasoning && strings.TrimSpace(events[index].RedactedSummary) == ""}, nil
	}, read: func(offset, length int) ([]byte, error) {
		bytesRead += length
		largestRead = max(largestRead, length)
		return []byte(stream[offset : offset+length]), nil
	}}
	selector := domain.DocFindSelector{Version: 1, Selection: domain.DocVisibleSelector{Version: 1, Purpose: "units",
		Repo: domain.HashContent([]byte("repo")), Hash: domain.HashContent([]byte("doc")), Generation: domain.HashContent([]byte("generation")),
		View: "all", Total: len(events), Limit: 20}, From: 0, To: len(events), QueryHash: domain.HashContent([]byte("needle"))}
	return source, selector, &bytesRead, &largestRead
}

func collectDocFind(test *testing.T, source docViewSource, selector domain.DocFindSelector, query string, bytesRead *int) []domain.DocFindMatch {
	test.Helper()
	var matches []domain.DocFindMatch
	for iteration := 0; iteration < 1000; iteration++ {
		before := *bytesRead
		page, err := scanDocFindPage(context.Background(), source, selector, query)
		if err != nil {
			test.Fatal(err)
		}
		if *bytesRead-before > docFindScanBytes {
			test.Fatal("page exceeded byte budget", *bytesRead-before)
		}
		matches = append(matches, page.Matches...)
		if page.NextCursor == nil {
			return matches
		}
		next, err := domain.DecodeDocFindSelector(*page.NextCursor)
		if err != nil || next == selector {
			test.Fatal("cursor failed to progress", next, selector, err)
		}
		selector = next
	}
	test.Fatal("find scan did not finish")
	return nil
}

func TestDocFindRegularFieldsAndCursorResume(test *testing.T) {
	events := []domain.CIREvent{
		{Kind: domain.EventToolCall, ToolName: "shell", Input: map[string]any{"command": "\uc548\ub155 <Needle>\nnext"}},
		{Kind: domain.EventToolResult, Output: strings.Repeat("banana ", 30)},
		{Kind: domain.EventReasoning, RedactedSummary: "VISIBLE summary", Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: "needle ciphertext"}},
	}
	source, selector, bytesRead, _ := docFindFixture(test, events)
	matches := collectDocFind(test, source, selector, "needle", bytesRead)
	if len(matches) != 1 || matches[0].Index != 0 || matches[0].Raw || matches[0].Offset != len("\uc548\ub155 <") {
		test.Fatal("decoded tool fields or locked-state exclusion failed", matches)
	}
	selector.QueryHash = domain.HashContent([]byte("ana"))
	matches = collectDocFind(test, source, selector, "ana", bytesRead)
	if len(matches) != 60 {
		test.Fatal("overlapping matches lost across result pages", len(matches))
	}
	for index, match := range matches {
		if match.Index != 1 || match.Raw || index > 0 && match.Offset <= matches[index-1].Offset {
			test.Fatal("duplicate or reordered match", index, match)
		}
	}
}

func TestDocFindDecodedMatchesLocateExactCanonicalBytes(test *testing.T) {
	events := []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("x", 9000) + "<needle🙂>"},
		{Kind: domain.EventToolCall, ToolName: "shell", Input: map[string]any{"needle key": "value"}},
		{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: "before needle after"}}},
	}
	source, selector, bytesRead, _ := docFindFixture(test, events)
	matches := collectDocFind(test, source, selector, "needle", bytesRead)
	if len(matches) != 3 {
		test.Fatal("unexpected matches", matches)
	}
	for _, match := range matches {
		if match.SourceRange == nil || match.Raw {
			test.Fatal("missing decoded source mapping", match)
		}
		entry, err := source.location(match.Index)
		if err != nil {
			test.Fatal(err)
		}
		raw, err := source.read(entry.Offset+match.SourceRange.From, match.SourceRange.To-match.SourceRange.From)
		if err != nil || string(raw) != "needle" {
			test.Fatal("source mapping changed field meaning", match, string(raw), err)
		}
	}
	if matches[0].SourceRange.From <= 8000 {
		test.Fatal("tool truncation fixture did not reach deep text", matches[0])
	}
}

func TestDocFindLargeRawFieldsCrossFragmentBoundariesWithinBudget(test *testing.T) {
	prefix, err := json.Marshal(domain.CIREvent{Kind: domain.EventToolResult, Output: ""})
	if err != nil {
		test.Fatal(err)
	}
	payloadStart := strings.Index(string(prefix), `"output":"`) + len(`"output":"`)
	output := strings.Repeat("x", docFindFragmentBytes-payloadStart-4) + "Needle🙂" + strings.Repeat("x", 200_000) + "NEEDLE🙂" + strings.Repeat("\ub05d", 25_000)
	events := []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}, {Kind: domain.EventToolResult, Output: strings.Repeat("x", 120_000)}}
	source, selector, bytesRead, largestRead := docFindFixture(test, events)
	matches := collectDocFind(test, source, selector, "needle🙂", bytesRead)
	if len(matches) != 2 || !matches[0].Raw || !matches[1].Raw || *largestRead > docViewBytes {
		test.Fatal("raw matches or bounded reads failed", matches, *largestRead)
	}
	for _, match := range matches {
		if match.SourceRange == nil || match.SourceRange.From != match.Offset || match.SourceRange.To != match.Offset+match.Length {
			test.Fatal("raw source span differs from raw match", match)
		}
		raw, err := source.read(match.Offset, match.Length)
		if err != nil || !strings.EqualFold(string(raw), "needle🙂") {
			test.Fatal("raw byte coordinates wrong", match, string(raw), err)
		}
	}
	for _, query := range []string{"absent", " ", "<", "\ud55c"} {
		selector.QueryHash = domain.HashContent([]byte(query))
		collectDocFind(test, source, selector, query, bytesRead)
	}
}

func TestDocFindHiddenOpaqueEventsAreNotSearched(test *testing.T) {
	opaque := &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("A", 140_000) + "needle"}
	events := []domain.CIREvent{{Kind: domain.EventReasoning, Locked: opaque}, {Kind: domain.EventCompaction, Locked: opaque},
		{Kind: domain.EventToolResult, Output: "needle"}}
	source, selector, bytesRead, _ := docFindFixture(test, events)
	matches := collectDocFind(test, source, selector, "needle", bytesRead)
	if len(matches) != 1 || matches[0].Index != 2 || *bytesRead > 4096 {
		test.Fatal("hidden opaque data reached find", matches, *bytesRead)
	}
	events[0].RedactedSummary = "searchable explanation needs a bounded field projection"
	source, selector, _, _ = docFindFixture(test, events)
	page, err := scanDocFindPage(context.Background(), source, selector, "needle")
	if !errors.Is(err, domain.ErrDocFindFieldsUnavailable) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
		test.Fatal("unsupported field projection searched locked bytes or returned partial data", page, err)
	}
}

func TestDocFindRawUTF8BoundaryAndInteriorCorruption(test *testing.T) {
	prefix, err := json.Marshal(domain.CIREvent{Kind: domain.EventToolResult, Output: ""})
	if err != nil {
		test.Fatal(err)
	}
	payloadStart := strings.Index(string(prefix), `"output":"`) + len(`"output":"`)
	events := []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("x", docFindFragmentBytes-payloadStart-1) + "🙂" + strings.Repeat("x", 140_000)}}
	source, selector, bytesRead, _ := docFindFixture(test, events)
	matches := collectDocFind(test, source, selector, "🙂", bytesRead)
	if len(matches) != 1 || matches[0].Offset != docFindFragmentBytes-1 || matches[0].Length != 4 || !matches[0].Raw {
		test.Fatal("UTF-8 boundary lost a raw match", matches)
	}
	read := source.read
	source.read = func(offset, length int) ([]byte, error) {
		raw, err := read(offset, length)
		if err == nil && len(raw) > 1024 {
			raw[1024] = 0xff
		}
		return raw, err
	}
	page, err := scanDocFindPage(context.Background(), source, selector, "🙂")
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
		test.Fatal("interior UTF-8 corruption was trimmed away", page, err)
	}
}

func TestDocFindTwentiethRawMatchAtEOFIsTerminal(test *testing.T) {
	events := []domain.CIREvent{{Kind: domain.EventToolResult, Output: strings.Repeat("x", 140_000) + strings.Repeat("}", 19)}}
	source, selector, bytesRead, _ := docFindFixture(test, events)
	matches := collectDocFind(test, source, selector, "}", bytesRead)
	if len(matches) != 20 || !matches[19].Raw {
		test.Fatal("terminal hit page lost matches", matches)
	}
}

func TestDocFindSelectionRejectsMixedBindingsBeforeReads(test *testing.T) {
	_, selector, _, _ := docFindFixture(test, []domain.CIREvent{{Kind: domain.EventToolResult, Output: "needle"}})
	selection, err := selector.Selection.Encode()
	if err != nil {
		test.Fatal(err)
	}
	valid := domain.DocFindPageRequest{Selection: selection, Query: "needle", To: 1}
	got, err := docFindSelection(selector.Selection.Repo, selector.Selection.Hash, valid)
	if err != nil || !reflect.DeepEqual(got, selector) {
		test.Fatal(got, selector, err)
	}
	cursor, _ := selector.Encode()
	for _, request := range []domain.DocFindPageRequest{
		{Selection: selection, Cursor: cursor, Query: "needle"}, {Cursor: cursor, Query: "changed"},
		{Cursor: cursor, Query: "needle", To: 1}, {Selection: selection, Query: "needle", To: 2},
		{Selection: selection, Query: ""}, {Selection: selection, Query: "needle", From: -1},
	} {
		if _, err := docFindSelection(selector.Selection.Repo, selector.Selection.Hash, request); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("invalid find request accepted", request, err)
		}
	}
}
