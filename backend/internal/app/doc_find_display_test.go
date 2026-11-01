package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func displayDocFindSelector(selector domain.DocFindSelector, query string) domain.DocFindSelector {
	selector.Version, selector.Representation, selector.QueryHash = 2, domain.DocFindDisplayRepresentation, domain.HashContent([]byte(query))
	return selector
}

func TestDocDisplayFindFormatsMessagesAndToolsWithStableContinuation(test *testing.T) {
	events := []domain.CIREvent{
		{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: "**NEEDLE** [label](https://hidden.example)"}}},
		{Kind: domain.EventToolCall, ToolName: "Bash", Input: map[string]any{"command": "printf needle"}},
		{Kind: domain.EventToolResult, Output: strings.Repeat("banana ", 40)},
		{Kind: domain.EventReasoning, RedactedSummary: "public note", Locked: &domain.LockedBlob{Blob: "needle secret"}},
	}
	source, selector, bytesRead, _ := docFindFixture(test, events)
	selector = displayDocFindSelector(selector, "ana")
	matches := collectDocFind(test, source, selector, "ana", bytesRead)
	if len(matches) != 80 {
		test.Fatal("overlapping matches missing", len(matches))
	}
	for index, match := range matches {
		if match.Index != 2 || match.Field != 0 || match.Raw || match.SourceRange == nil || index > 0 && match.Offset <= matches[index-1].Offset {
			test.Fatal(index, match)
		}
	}
	for _, scenario := range []struct {
		query string
		count int
	}{{"needle label", 1}, {"Bash(printf needle)\n$ printf needle", 1}, {"hidden.example", 0}, {"secret", 0}, {"**", 0}} {
		selector = displayDocFindSelector(selector, scenario.query)
		matches := collectDocFind(test, source, selector, scenario.query, bytesRead)
		if len(matches) != scenario.count {
			test.Fatal(scenario, matches)
		}
		for _, match := range matches {
			if match.SourceRange == nil || match.Raw {
				test.Fatal(match)
			}
		}
	}
}

func TestDocDisplayFindLargeLockedEventsReadOnlyPublicSpan(test *testing.T) {
	event := domain.CIREvent{Kind: domain.EventReasoning, RedactedSummary: "**Needle🙂** visible", Locked: &domain.LockedBlob{Blob: strings.Repeat("secret needle ", 100_000)}}
	source, selector, bytesRead, fields := docFindFieldsFixture(test, event)
	read := source.read
	source.read = func(offset, length int) ([]byte, error) {
		if offset < fields.Summary.From || offset+length > fields.Summary.To {
			test.Fatal("opaque read", offset, length)
		}
		return read(offset, length)
	}
	selector = displayDocFindSelector(selector, "needle🙂")
	matches := collectDocFind(test, source, selector, "needle🙂", bytesRead)
	if len(matches) != 1 || matches[0].Offset != 0 || matches[0].SourceRange == nil || *bytesRead > 128 {
		test.Fatal(matches, *bytesRead)
	}
	span := matches[0].SourceRange
	raw, err := source.read(span.From, span.To-span.From)
	if err != nil || string(raw) != "Needle🙂" {
		test.Fatal(string(raw), err)
	}
}

func TestDocDisplayFindRejectsUnpreparedLargeWithoutRawOrPartialFallback(test *testing.T) {
	for _, event := range []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("needle ", 30_000)},
		{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("needle ", 30_000)}}},
	} {
		source, selector, _, _ := docFindFixture(test, []domain.CIREvent{event, {Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: "needle"}}}})
		selector = displayDocFindSelector(selector, "needle")
		page, err := scanDocFindPage(context.Background(), source, selector, "needle")
		if !errors.Is(err, domain.ErrDocFindDisplayUnavailable) && !errors.Is(err, domain.ErrDocFindFieldsUnavailable) || page.Projection != "" || len(page.Matches) != 0 || page.NextCursor != nil {
			test.Fatal("partial or raw fallback", page, err)
		}
	}
}

func TestDocDisplayFindLegacyKeepsIdentityAndExplicitMode(test *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	storage := store.NewFSStore(test.TempDir())
	doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{{Kind: domain.EventMessage, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("**needle** ", 45)}}}})
	service := NewService(storage, storage, nil, nil, nil)
	request := domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 1, Total: 1, Representation: domain.DocFindDisplayRepresentation}
	count := 0
	for attempt := 0; attempt < 5; attempt++ {
		page, err := service.FindDocPage(ctx, repo, doc.Hash, request)
		if err != nil || page.Projection != domain.DocLegacyFindDisplayProjection || page.Hash != doc.Hash || page.Generation != "" {
			test.Fatal(page, err)
		}
		for _, match := range page.Matches {
			if match.SourceRange == nil || match.RevealCursor != "" || match.Raw || match.Field != 0 {
				test.Fatal(match)
			}
		}
		count += len(page.Matches)
		if page.NextCursor == nil {
			break
		}
		wrong := domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *page.NextCursor}
		if _, err := service.FindDocPage(ctx, repo, doc.Hash, wrong); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("mixed mode accepted", err)
		}
		request = wrong
		request.Representation = domain.DocFindDisplayRepresentation
	}
	if count != 45 {
		test.Fatal("legacy continuation lost results", count)
	}
	current, err := storage.GetDoc(ctx, repo, doc.Hash)
	if err != nil || current.DocumentRef() != doc.DocumentRef() {
		test.Fatal("source identity changed", current.DocumentRef(), err)
	}
}

func TestDocDisplayFindDefersConstructionWhenPageBudgetIsSpent(test *testing.T) {
	var output any = "needle"
	for depth := 0; depth < 480; depth++ {
		output = map[string]any{"value": output}
	}
	source, selector, bytesRead, _ := docFindFixture(test, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: output},
		{Kind: domain.EventToolCall, ToolName: "Bash", Input: map[string]any{"command": strings.Repeat("x", 50_000) + " needle"}},
	})
	selector = displayDocFindSelector(selector, "needle")
	first, err := scanDocFindPage(context.Background(), source, selector, "needle")
	if err != nil || first.Index != 1 || first.NextCursor == nil || len(first.Matches) != 1 {
		test.Fatal("second projection was not deferred", first, err)
	}
	next, err := domain.DecodeDocFindSelector(*first.NextCursor)
	if err != nil {
		test.Fatal(err)
	}
	remaining := collectDocFind(test, source, next, "needle", bytesRead)
	if len(remaining) != 2 || remaining[0].Index != 1 || remaining[1].Index != 1 {
		test.Fatal("budget continuation lost command header/body", remaining)
	}
}
