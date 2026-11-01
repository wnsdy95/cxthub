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

func docFindFieldsFixture(test *testing.T, event domain.CIREvent) (docViewSource, domain.DocFindSelector, *int, domain.DocFindFields) {
	test.Helper()
	source, selector, bytesRead, _ := docFindFixture(test, []domain.CIREvent{event})
	event.Seq = 0
	raw, err := json.Marshal(event)
	if err != nil {
		test.Fatal(err)
	}
	fields, err := domain.BuildDocFindFields(domain.HashContent(raw), raw)
	if err != nil {
		test.Fatal(err)
	}
	source.findFields = func(domain.DocEventIndex) (domain.DocFindFields, error) { return fields, nil }
	return source, selector, bytesRead, fields
}

func TestDocFindFieldsLargeOpaqueStateOnlyReadsDecodedSummary(test *testing.T) {
	event := domain.CIREvent{Kind: domain.EventReasoning, RedactedSummary: "visible \"Needle🙂\"\nonly", Locked: &domain.LockedBlob{
		Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("needle secret ", 100_000)}}
	source, selector, bytesRead, fields := docFindFieldsFixture(test, event)
	read := source.read
	source.read = func(offset, length int) ([]byte, error) {
		if offset < fields.Summary.From || offset+length > fields.Summary.To {
			test.Fatal("find read outside public summary", offset, length, fields.Summary)
		}
		return read(offset, length)
	}
	matches := collectDocFind(test, source, selector, "needle🙂", bytesRead)
	if len(matches) != 1 || matches[0].Raw || matches[0].Offset != len("visible \"") || matches[0].Length != len("Needle🙂") || *bytesRead > 128 {
		test.Fatal("large opaque state changed decoded summary semantics", matches, *bytesRead)
	}
	if matches[0].SourceRange == nil || matches[0].SourceRange.From <= fields.Summary.From || matches[0].SourceRange.To >= fields.Summary.To {
		test.Fatal("decoded public field lost event-relative source position", matches[0], fields.Summary)
	}
	located := *matches[0].SourceRange
	raw, err := source.read(located.From, located.To-located.From)
	if err != nil || string(raw) != "Needle🙂" {
		test.Fatal("decoded public source mapping entered opaque bytes", string(raw), err)
	}
	matches = collectDocFind(test, source, selector, "secret", bytesRead)
	if len(matches) != 0 {
		test.Fatal("encrypted state was searched", matches)
	}
}

func TestDocFindFieldsLargeSummaryResumesWithinPublicSpan(test *testing.T) {
	event := domain.CIREvent{Kind: domain.EventReasoning, RedactedSummary: strings.Repeat("x", docFindFragmentBytes-5) + "Needle🙂" + strings.Repeat("끝", 100_000) + "NEEDLE🙂",
		Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: "needle🙂 must never match"}}
	source, selector, bytesRead, fields := docFindFieldsFixture(test, event)
	read := source.read
	source.read = func(offset, length int) ([]byte, error) {
		if offset < fields.Summary.From+1 || offset+length > fields.Summary.To-1 || length > docFindFragmentBytes {
			test.Fatal("find escaped public fragment", offset, length, fields.Summary)
		}
		return read(offset, length)
	}
	matches := collectDocFind(test, source, selector, "needle🙂", bytesRead)
	if len(matches) != 2 || !matches[0].Raw || !matches[1].Raw || matches[0].Offset != fields.Summary.From+1+docFindFragmentBytes-5 || matches[1].Offset+matches[1].Length != fields.Summary.To-1 {
		test.Fatal("fragment crossing or final summary match lost", matches)
	}
	selector.Byte = fields.Summary.From - 1
	page, err := scanDocFindPage(context.Background(), source, selector, "needle🙂")
	if !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
		test.Fatal("forged cursor searched locked state", page, err)
	}
}

func TestDocFindFieldsBindingFailureReturnsNoPartialMatches(test *testing.T) {
	event := domain.CIREvent{Kind: domain.EventReasoning, RedactedSummary: "needle", Locked: &domain.LockedBlob{
		Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("x", 200_000)}}
	source, selector, bytesRead, fields := docFindFieldsFixture(test, event)
	for _, mutation := range []func(*domain.DocFindFields){
		func(fields *domain.DocFindFields) { fields.EventHash = domain.HashContent([]byte("another event")) },
		func(fields *domain.DocFindFields) { fields.EventBytes++ },
		func(fields *domain.DocFindFields) { fields.Summary = nil },
		func(fields *domain.DocFindFields) { fields.Summary = &domain.DocFindSpan{From: -1, To: 3} },
	} {
		changed := fields
		mutation(&changed)
		source.findFields = func(domain.DocEventIndex) (domain.DocFindFields, error) { return changed, nil }
		page, err := scanDocFindPage(context.Background(), source, selector, "needle")
		if !errors.Is(err, domain.ErrIntegrity) || *bytesRead != 0 || !reflect.DeepEqual(page, domain.DocFindPage{}) {
			test.Fatal("invalid field projection reached source bytes", page, err, *bytesRead)
		}
	}
}

func TestDocFindFieldsLargeAgentMessageNeverSearchesOpaqueFields(test *testing.T) {
	for _, largePublic := range []bool{false, true} {
		test.Run(map[bool]string{false: "decoded", true: "raw blocks"}[largePublic], func(test *testing.T) {
			text := "public \"Needle🙂\"\nonly"
			if largePublic {
				text = strings.Repeat("끝", 70_000) + text
			}
			event := domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, AgentMessage: true,
				Blocks: []domain.ContentBlock{{Type: "text", Text: text}},
				Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("secret needle🙂 ", 30_000)}}
			source, selector, bytesRead, fields := docFindFieldsFixture(test, event)
			if fields.Blocks == nil || fields.Summary != nil {
				test.Fatal("message projection did not isolate public blocks", fields)
			}
			read := source.read
			source.read = func(offset, length int) ([]byte, error) {
				if offset < fields.Blocks.From || offset+length > fields.Blocks.To {
					test.Fatal("message matcher read locked fields", offset, length, fields)
				}
				return read(offset, length)
			}
			matches := collectDocFind(test, source, selector, "needle🙂", bytesRead)
			if len(matches) != 1 || matches[0].Raw != largePublic {
				test.Fatal("opaque agent message produced matches", matches)
			}
			if matches := collectDocFind(test, source, selector, "secret", bytesRead); len(matches) != 0 {
				test.Fatal("locked text leaked through agent message preview", matches)
			}
		})
	}
	event := domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, AgentMessage: true,
		Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("needle ", 30_000)}}
	source, selector, bytesRead, _ := docFindFieldsFixture(test, event)
	if matches := collectDocFind(test, source, selector, "needle", bytesRead); len(matches) != 0 || *bytesRead > 2 {
		test.Fatal("empty agent message searched provider state", matches, *bytesRead)
	}
}
