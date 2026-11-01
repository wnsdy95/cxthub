package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func seedLegacyFindDocument(test *testing.T, storage *store.FSStore, repo domain.ContentHash, events []domain.CIREvent) domain.SessionDoc {
	test.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex}, Events: events}}
	for index := range doc.CIR.Events {
		doc.CIR.Events[index].Seq = index
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		test.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := storage.PutDoc(systemTestContext(), repo, doc); err != nil {
		test.Fatal(err)
	}
	return doc
}

func TestDocFindLegacyPreservesIdentityAndExcludesProviderState(test *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	storage := store.NewFSStore(test.TempDir())
	doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{
		{Kind: domain.EventToolCall, ToolName: "Bash", Input: map[string]any{"command": "needle"}},
		{Kind: domain.EventReasoning, RedactedSummary: strings.Repeat("Needle🙂 ", 45), Locked: &domain.LockedBlob{
			Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("secret needle ", 100_000)}},
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, AgentMessage: true, Blocks: []domain.ContentBlock{{Type: "text", Text: "needle"}},
			Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("secret needle ", 50_000)}},
		{Kind: domain.EventReasoning, RedactedSummary: strings.Repeat(" \t", 90_000),
			Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: "secret needle"}},
		{Kind: domain.EventToolResult, Output: strings.Repeat("\ud55c\uae00", 80_000) + "needle"},
	})
	spy := &countedReadStore{FSStore: storage}
	service := NewService(storage, spy, nil, nil, nil)
	request := domain.DocFindPageRequest{Legacy: true, Query: "needle", To: len(doc.CIR.Events), Total: len(doc.CIR.Events)}
	var matches []domain.DocFindMatch
	for attempt := 0; attempt < 30; attempt++ {
		page, err := service.FindDocPage(ctx, repo, doc.Hash, request)
		if err != nil || page.Projection != domain.DocLegacyFindProjection || page.Generation != "" || page.Hash != doc.Hash {
			test.Fatal(page, err)
		}
		for _, match := range page.Matches {
			if match.RevealCursor != "" || match.Index == 3 {
				test.Fatal("legacy invented a visible selector or searched hidden reasoning", match)
			}
		}
		matches = append(matches, page.Matches...)
		if page.NextCursor == nil {
			break
		}
		request = domain.DocFindPageRequest{Legacy: true, Query: "needle", Cursor: *page.NextCursor}
	}
	if len(matches) != 48 || spy.whole != 0 {
		test.Fatal("lost/duplicated legacy fields or downloaded full archive", len(matches), spy.whole)
	}
	request = domain.DocFindPageRequest{Legacy: true, Query: "secret", To: len(doc.CIR.Events), Total: len(doc.CIR.Events)}
	for attempt := 0; attempt < 30; attempt++ {
		page, err := service.FindDocPage(ctx, repo, doc.Hash, request)
		if err != nil || len(page.Matches) != 0 {
			test.Fatal("provider state entered legacy search", page, err)
		}
		if page.NextCursor == nil {
			break
		}
		request = domain.DocFindPageRequest{Legacy: true, Query: "secret", Cursor: *page.NextCursor}
	}
	current, err := storage.GetDoc(ctx, repo, doc.Hash)
	if err != nil || current.Hash != doc.Hash || current.Identity != domain.DocumentIdentityLegacy {
		test.Fatal("legacy search rewrote archival identity", current.DocumentRef(), err)
	}
}

func TestDocFindLegacyChecksDeclaredContextAndFreshAuthority(test *testing.T) {
	ctx, repo := systemTestContext(), h('1')
	storage := store.NewFSStore(test.TempDir())
	base := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{historyMessage(domain.RoleUser, "shared needle")})
	doc := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{historyMessage(domain.RoleUser, "shared needle"), historyMessage(domain.RoleAssistant, "new needle")})
	service := NewService(storage, storage, nil, nil, nil)
	request := domain.DocFindPageRequest{Legacy: true, Query: "needle", Base: base.Hash, From: 1, To: 2, Total: 2, Inherited: 1}
	page, err := service.FindDocPage(ctx, repo, doc.Hash, request)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].Index != 1 {
		test.Fatal(page, err)
	}
	for _, changed := range []string{"total", "inherited"} {
		invalid := request
		if changed == "total" {
			invalid.Total++
		} else {
			invalid.Inherited = 0
		}
		page, err := service.FindDocPage(ctx, repo, doc.Hash, invalid)
		if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(page, domain.DocFindPage{}) {
			test.Fatal("declared legacy context was trusted", changed, page, err)
		}
	}
	authorizations := 0
	denied := inbound.WithDocumentReadAuthorizer(ctx, func(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
		authorizations++
		if authorizations > 1 {
			return nil, domain.ErrForbidden
		}
		return ctx, nil
	})
	if _, err := service.FindDocPage(denied, repo, doc.Hash, request); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("legacy read did not reauthorize", err)
	}
	if _, err := service.FindDocPage(outbound.WithDocReadOnly(ctx), repo, doc.Hash, request); err != nil {
		test.Fatal("warm legacy search mutated or required writable state", err)
	}
}

func TestDocFindLegacyDoesNotDowngradeRootAndKeepsEmptyContinuations(test *testing.T) {
	ctx, repo, directory := systemTestContext(), h('1'), test.TempDir()
	storage := store.NewFSStore(directory)
	root, _ := seedRootReadDoc(test, directory, storage, repo, historyMessage(domain.RoleUser, "needle"))
	service := NewService(storage, storage, nil, nil, nil)
	if _, err := service.FindDocPage(ctx, repo, root.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", To: 1, Total: 1}); !errors.Is(err, domain.ErrDocVisibleProjectionUnsupported) {
		test.Fatal("legacy mode accepted a root identity", err)
	}
	events := make([]domain.CIREvent, 300)
	for index := range events {
		events[index] = historyMessage(domain.RoleUser, fmt.Sprintf("source row %d", index))
	}
	doc := seedLegacyFindDocument(test, storage, repo, events)
	page, err := service.FindDocPage(ctx, repo, doc.Hash, domain.DocFindPageRequest{Legacy: true, Query: "absent", Total: 300, To: 300})
	if err != nil || len(page.Matches) != 0 || page.Index != docFindScanEvents || page.NextCursor == nil {
		test.Fatal("legacy empty scan falsely completed", page, err)
	}
	selector, err := domain.DecodeDocLegacyFindSelector(*page.NextCursor)
	if err != nil || selector.Hash != doc.Hash || selector.Index != page.Index {
		test.Fatal("legacy continuation lost its immutable source", selector, err)
	}
}
