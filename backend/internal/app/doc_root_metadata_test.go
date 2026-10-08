package app

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestRootMetadataReadAndSearchParity(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	root, _ := seedRootReadDoc(t, dir, st, repo,
		domain.CIREvent{Kind: domain.EventTurn, Role: domain.RoleUser},
		historyMessage(domain.RoleUser, "needle prompt \uac80\uc0c9 \U0001f642"),
		historyMessage(domain.Role("custom-\uac80\uc0c9\t\"\\"), "needle custom reply"),
		domain.CIREvent{Kind: domain.EventToolCall, CallID: "c", ToolName: "synthetic", Input: map[string]any{"role": "user", "seq": 999, "text": "archival-only"}},
		domain.CIREvent{Kind: domain.EventToolResult, CallID: "c", Output: map[string]any{"role": "user", "seq": 999, "text": "archival-only"}},
		domain.CIREvent{Kind: domain.EventReasoning, RedactedSummary: "needle summary"},
		historyMessage(domain.RoleAssistant, "answer"),
		historyMessage(domain.RoleUser, "second prompt"),
		historyMessage(domain.RoleAssistant, "second answer"),
	)
	legacy := putHistoryDoc(t, st, repo, root.CIR.Envelope, root.CIR.Events...)
	spy := &rootReadSpy{FSStore: st}
	svc := NewService(st, spy, nil, nil, nil)
	full, err := domain.BuildDocReadIndex(legacy)
	if err != nil {
		t.Fatal(err)
	}
	full.Hash = root.Hash
	wantMetadata := full
	wantMetadata.Events = append([]domain.DocEventIndex{}, full.Events...)
	for i := range wantMetadata.Events {
		wantMetadata.Events[i].Text = ""
	}
	// Assert selection of the cheaper projection, not just equal HTTP results.
	for _, agent := range []bool{false, true} {
		var source docReadSource
		before := spy.reads
		if agent {
			source, err = svc.agentHistoryReadSource(ctx, repo, root.DocumentRef())
		} else {
			source, err = svc.documentReadSource(ctx, repo, root.Hash)
		}
		if err != nil || spy.reads != before+1 || !reflect.DeepEqual(source.index, wantMetadata) {
			t.Fatalf("metadata source agent=%v drifted or bypassed current proof: %v", agent, err)
		}
	}
	search, err := svc.rootReadSource(ctx, repo, root.Hash, docReadSearch)
	if err != nil || search == nil || !reflect.DeepEqual(search.index, full) {
		t.Fatal("search source lost full text", err)
	}
	for offset := 0; offset <= len(root.CIR.Events); offset++ {
		got, err := svc.ReadDocEvents(ctx, repo, root.Hash, "", offset, 1)
		if err != nil {
			t.Fatal(err)
		}
		want, err := svc.ReadDocEvents(ctx, repo, legacy.Hash, "", offset, 1)
		want.Hash = root.Hash
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("event page %d differs: %v", offset, err)
		}
	}
	for index, offset := 0, 0; index < len(root.CIR.Events); {
		got, err := svc.ReadDocFragments(ctx, repo, root.Hash, index, offset, 2, 37)
		if err != nil {
			t.Fatal(err)
		}
		want, err := svc.ReadDocFragments(ctx, repo, legacy.Hash, index, offset, 2, 37)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("fragment %d/%d differs: %v", index, offset, err)
		}
		if got.NextIndex == index && got.NextOffset <= offset {
			t.Fatal("fragment cursor made no progress")
		}
		index, offset = got.NextIndex, got.NextOffset
	}
	for _, query := range []string{"needle", "\uac80\uc0c9", "archival-only", "absent"} {
		got, err := svc.SearchDocEvents(ctx, repo, root.Hash, query, -1, 10)
		if err != nil {
			t.Fatal(err)
		}
		want, err := svc.SearchDocEvents(ctx, repo, legacy.Hash, query, -1, 10)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("event search %q differs: %v", query, err)
		}
		all, err := svc.Search(ctx, inbound.SearchInput{RepoID: repo, Query: query})
		if err != nil || len(all.Hits) != len(want) {
			t.Fatalf("repository search %q differs: %v", query, err)
		}
		for i, hit := range all.Hits {
			if hit.SnapshotID != root.Hash || hit.Kind != "event" || hit.Seq != want[i].Seq || hit.Role != want[i].Role || hit.Snippet != searchSnippet(want[i].Text, query) {
				t.Fatal("repository search metadata or text differs")
			}
		}
	}
	for _, projection := range []string{"", "omit"} {
		for _, before := range []int{0, -1, 7} {
			req := domain.AgentHistoryPageRequest{DocIdentity: root.Identity, Before: before, Limit: 2, MaxBytes: 64 << 10, IncompleteTail: projection}
			got, err := svc.ReadAgentHistoryPage(ctx, repo, root.Hash, req)
			if err != nil {
				t.Fatal(err)
			}
			req.DocIdentity = domain.DocumentIdentityLegacy
			want, err := svc.ReadAgentHistoryPage(ctx, repo, legacy.Hash, req)
			want.Hash, want.DocIdentity = root.Hash, root.Identity
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("agent page %q before=%d differs: %v", projection, before, err)
			}
		}
	}
}
