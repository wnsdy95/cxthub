package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRootRangesKeepAgentMetadataAndSearchSeparate(t *testing.T) {
	ctx, repo, dir := systemTestContext(), h('1'), t.TempDir()
	st := store.NewFSStore(dir)
	root, _ := seedRootReadDoc(t, dir, st, repo,
		historyMessage(domain.RoleUser, "needle prompt"),
		historyMessage(domain.RoleAssistant, strings.Repeat("payload \uac80\uc0c9 \U0001f642 ", 1<<10)),
		domain.CIREvent{Kind: domain.EventToolCall, CallID: "c", ToolName: "synthetic", Input: map[string]any{"role": "user", "seq": 999}},
		domain.CIREvent{Kind: domain.EventToolResult, CallID: "c", Output: map[string]any{"role": "user", "seq": 999}},
		historyMessage(domain.RoleUser, "second prompt"),
		historyMessage(domain.RoleAssistant, "second answer"),
	)
	legacy := putHistoryDoc(t, st, repo, root.CIR.Envelope, root.CIR.Events...)
	full, err := domain.BuildDocReadIndex(legacy)
	if err != nil {
		t.Fatal(err)
	}
	full.Hash = root.Hash
	spy := &rootReadSpy{FSStore: st}
	svc := NewService(st, spy, nil, nil, nil)
	for _, mode := range []string{"ranges", "agent", "search"} {
		before := spy.reads
		var source docReadSource
		switch mode {
		case "ranges":
			source, err = svc.documentReadSource(ctx, repo, root.Hash)
		case "agent":
			source, err = svc.agentHistoryReadSource(ctx, repo, root.DocumentRef())
		case "search":
			var result *docReadSource
			result, err = svc.rootReadSource(ctx, repo, root.Hash, docReadSearch)
			if result != nil {
				source = *result
			}
		}
		want := full
		want.Events = append([]domain.DocEventIndex{}, full.Events...)
		for i := range want.Events {
			if mode != "search" {
				want.Events[i].Text = ""
			}
			if mode == "ranges" {
				want.Events[i].Role = ""
			}
		}
		if err != nil || spy.reads != before+1 || !reflect.DeepEqual(source.index, want) {
			t.Fatalf("%s projection drifted or bypassed current proof: %v", mode, err)
		}
	}
	for offset := 0; offset <= len(root.CIR.Events); offset++ {
		got, err := svc.ReadDocEvents(ctx, repo, root.Hash, "", offset, 1)
		if err != nil {
			t.Fatal(err)
		}
		want, err := svc.ReadDocEvents(ctx, repo, legacy.Hash, "", offset, 1)
		want.Hash = root.Hash
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("range event page %d differs: %v", offset, err)
		}
	}
	// A fragment can end inside a large event, but never inside its UTF-8 rune.
	for _, position := range [][2]int{{0, 0}, {1, 0}, {5, 0}, {6, 0}} {
		got, err := svc.ReadDocFragments(ctx, repo, root.Hash, position[0], position[1], 2, 37)
		if err != nil {
			t.Fatal(err)
		}
		want, err := svc.ReadDocFragments(ctx, repo, legacy.Hash, position[0], position[1], 2, 37)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("range fragment %v differs: %v", position, err)
		}
	}
	for _, before := range []int{-1, 4} {
		req := domain.AgentHistoryPageRequest{DocIdentity: root.Identity, Before: before, Limit: 1, MaxBytes: 64 << 10}
		got, err := svc.ReadAgentHistoryPage(ctx, repo, root.Hash, req)
		if err != nil {
			t.Fatal(err)
		}
		req.DocIdentity = domain.DocumentIdentityLegacy
		want, err := svc.ReadAgentHistoryPage(ctx, repo, legacy.Hash, req)
		want.Hash, want.DocIdentity = root.Hash, root.Identity
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("agent role boundaries before=%d differ: %v", before, err)
		}
	}
}
