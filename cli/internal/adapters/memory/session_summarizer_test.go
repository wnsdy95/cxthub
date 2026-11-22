package memory

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type summarizerSourceFake struct {
	folders []string
	native  domain.NativeMemory
	found   bool
	err     error
}

func (f *summarizerSourceFake) Provider() domain.ProviderKind { return domain.ProviderClaude }

func (f *summarizerSourceFake) ReadNative(_ context.Context, folder, _ string) (domain.NativeMemory, bool, error) {
	f.folders = append(f.folders, folder)
	return f.native, f.found, f.err
}

type summarizerDistillerFake struct {
	events []int
	native []string
	err    error
}

func (f *summarizerDistillerFake) Distill(_ context.Context, cir domain.CIRDocument, native *domain.NativeMemory) (domain.MemoryDigest, error) {
	f.events = append(f.events, len(cir.Events))
	text := "none"
	if native != nil {
		text = native.Text
	}
	f.native = append(f.native, text)
	return domain.MemoryDigest{Summary: "digest"}, f.err
}

func summarizerConversation(events int) domain.CIRDocument {
	doc := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1, SourceProvider: domain.ProviderClaude, SessionOriginID: "session-1"}}
	for index := 0; index < events; index++ {
		doc.Events = append(doc.Events, domain.Event{Kind: domain.EventMessage, Seq: index, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "said"}}})
	}
	return doc
}

func TestSessionSummarizerDistillsTheConversationUpToEachPoint(t *testing.T) {
	source := &summarizerSourceFake{native: domain.NativeMemory{Provider: domain.ProviderClaude, Text: "secret notes"}, found: true}
	distiller := &summarizerDistillerFake{}
	mask := func(_ string, native domain.NativeMemory) domain.NativeMemory {
		native.Text = "masked notes"
		return native
	}
	summarizer := NewSessionSummarizer(map[domain.ProviderKind]outbound.MemorySource{domain.ProviderClaude: source}, distiller, mask)
	conversation := summarizerConversation(5)
	digests, err := summarizer.Summarize(context.Background(), "/work/app", domain.ProviderClaude, "session-1", conversation, []int{2, 5})
	if err != nil || len(digests) != 2 {
		t.Fatalf("digests: %+v %v", digests, err)
	}
	// The tool's memory is read once, for the session's folder, and masked.
	if !reflect.DeepEqual(source.folders, []string{"/work/app"}) || !reflect.DeepEqual(distiller.events, []int{2, 5}) || !reflect.DeepEqual(distiller.native, []string{"masked notes", "masked notes"}) {
		t.Fatalf("read for %v, distilled %v with %v", source.folders, distiller.events, distiller.native)
	}
	if len(conversation.Events) != 5 {
		t.Fatal("the conversation was changed")
	}
}

func TestSessionSummarizerGoesOnWithoutTheToolsMemory(t *testing.T) {
	for name, source := range map[string]*summarizerSourceFake{
		"it is not there":   {found: false},
		"it cannot be read": {err: errors.New("unreadable")},
	} {
		distiller := &summarizerDistillerFake{}
		summarizer := NewSessionSummarizer(map[domain.ProviderKind]outbound.MemorySource{domain.ProviderClaude: source}, distiller, func(_ string, native domain.NativeMemory) domain.NativeMemory { return native })
		if _, err := summarizer.Summarize(context.Background(), "/work/app", domain.ProviderClaude, "session-1", summarizerConversation(3), []int{3}); err != nil || !reflect.DeepEqual(distiller.native, []string{"none"}) {
			t.Fatalf("%s: %v with %v", name, err, distiller.native)
		}
	}
	// A tool with no memory source, and a session with no folder, are distilled from the conversation alone.
	distiller := &summarizerDistillerFake{}
	summarizer := NewSessionSummarizer(nil, distiller, func(_ string, native domain.NativeMemory) domain.NativeMemory { return native })
	if _, err := summarizer.Summarize(context.Background(), "", domain.ProviderCodex, "thread-1", summarizerConversation(1), []int{1}); err != nil || !reflect.DeepEqual(distiller.native, []string{"none"}) {
		t.Fatalf("no source: %v with %v", err, distiller.native)
	}
}

func TestSessionSummarizerRefusesWhatItCannotWriteDown(t *testing.T) {
	keep := func(_ string, native domain.NativeMemory) domain.NativeMemory { return native }
	for name, ends := range map[string][]int{"a point before the conversation": {0}, "a point after it": {4}} {
		if _, err := NewSessionSummarizer(nil, &summarizerDistillerFake{}, keep).Summarize(context.Background(), "/work/app", domain.ProviderClaude, "session-1", summarizerConversation(3), ends); err == nil {
			t.Errorf("%s was written down", name)
		}
	}
	failing := &summarizerDistillerFake{err: errors.New("cannot distill")}
	if _, err := NewSessionSummarizer(nil, failing, keep).Summarize(context.Background(), "/work/app", domain.ProviderClaude, "session-1", summarizerConversation(3), []int{3}); err == nil {
		t.Fatal("a failed distillation gave a summary")
	}
	if _, err := NewSessionSummarizer(nil, nil, keep).Summarize(context.Background(), "/work/app", domain.ProviderClaude, "session-1", summarizerConversation(3), []int{3}); err == nil {
		t.Fatal("a summarizer without a distiller gave a summary")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSessionSummarizer(nil, &summarizerDistillerFake{}, keep).Summarize(cancelled, "/work/app", domain.ProviderClaude, "session-1", summarizerConversation(3), []int{3}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled run: %v", err)
	}
}
