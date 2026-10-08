package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func diffDoc(t *testing.T, session string, events ...Event) SessionDoc {
	t.Helper()
	doc := SessionDoc{CIR: CIRDocument{Envelope: Envelope{SourceProvider: ProviderCodex, SessionOriginID: session, CIRVersion: CIRVersionV2}, Events: events}}
	raw, err := CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = HashContent(raw)
	return doc
}
func TestContextDiffExactEventsNotTextDedup(t *testing.T) {
	a := Event{Kind: EventMessage, Role: "user", Seq: 0, ID: "first", Blocks: []ContentBlock{{Type: "text", Text: "private-token-do-not-print"}}}
	b := a
	b.Seq = 1
	b.ID = "second"
	before := diffDoc(t, "one", a)
	after := diffDoc(t, "one", a, b)
	got, err := CompareContextDocuments(context.Background(), &before, after)
	if err != nil || got.State != "extended" || !got.CountsKnown || got.Added.Start != 1 || got.Added.End != 2 || got.Added.Kinds[EventMessage] != 1 {
		t.Fatal(got, err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "private-token") {
		t.Fatal("diagnostic leaked source text")
	}
	other := diffDoc(t, "two", a, b)
	if _, err = CompareContextDocuments(context.Background(), &before, other); !errors.Is(err, ErrHashMismatch) {
		t.Fatal("cross-session equality claimed coverage", err)
	}
	// Same visible text with a different provider event ID is a rewrite.
	rewritten := a
	rewritten.ID = "replacement"
	got, err = CompareContextDocuments(context.Background(), &before, diffDoc(t, "one", rewritten, b))
	if err != nil || got.State != "replacement" || got.Removed.End != 1 || got.Added.End != 2 {
		t.Fatal(got, err)
	}
	got, err = CompareContextDocuments(context.Background(), &after, before)
	if err != nil || got.State != "older_observation" {
		t.Fatal(got, err)
	}
}
func TestContextDiffUsesCanonicalOrderAndAuthenticatesBody(t *testing.T) {
	a := Event{Kind: EventTurn, Role: "user", Seq: 1}
	b := Event{Kind: EventTurn, Role: "assistant", Seq: 2}
	before := diffDoc(t, "one", a, b)
	after := diffDoc(t, "one", b, a)
	got, err := CompareContextDocuments(context.Background(), &before, after)
	if err != nil || got.State != "unchanged" {
		t.Fatal(got, err)
	}
	after.CIR.Events[0].Role = "user"
	if _, err = CompareContextDocuments(context.Background(), &before, after); !errors.Is(err, ErrHashMismatch) {
		t.Fatal(err)
	}
}
