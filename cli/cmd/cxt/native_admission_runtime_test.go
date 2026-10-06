package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/nativeclaude"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// This composes the existing runtime/source fixture and admission runner. Only
// server responses and native summaries are synthetic; preparation, selection,
// budget accounting, delivery validation and admission are production code.
type nativeAdmissionFixture struct {
	*agentSelectionFixture
	view          *domain.HistoryQueryResult
	reader        nativeClaudeContextReader
	current       nativeclaude.ContextSummary
	question      domain.AgentInitialPrompt
	pageReads     atomic.Int64
	memoryChanged atomic.Bool
}

func nativeAdmissionRuntime(t *testing.T, window int64, turns, turnBytes int) *nativeAdmissionFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	f, view := latestMainRuntime(t)
	n := &nativeAdmissionFixture{agentSelectionFixture: f, view: view, question: domain.NewAgentInitialPrompt("Review.\n")}
	n.current = nativeclaude.ContextSummary{SessionID: claudeContextTestSession, Model: "claude-fixture", TotalTokens: 5000, MaxTokens: 2000000, RawMaxTokens: window, Measurement: "local_estimate"}
	n.reader = nativeClaudeContextReader{baseline: n.current, host: "2.1.287", scope: domain.HashContent([]byte("owned offline admission")), read: func(context.Context) (nativeclaude.ContextSummary, error) { return n.current, nil }}
	f.runtime.capabilities = n.reader
	view.Snapshots[0].Provider = domain.ProviderClaude
	page := domain.AgentHistoryPage{Version: domain.AgentHistoryProjectionVersion, Hash: view.Snapshots[0].DocHash, Provider: domain.ProviderClaude, SessionID: "synthetic-history", Total: 2 * turns, Before: 2 * turns, NextBefore: -1, Turns: []domain.AgentHistoryTurn{}}
	for i := turns - 1; i >= 0; i-- {
		events := []domain.Event{
			{Kind: domain.EventMessage, Role: "user", ID: fmt.Sprintf("user-%d", i), Seq: 2 * i, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("main-turn-%02d ", i) + strings.Repeat("x", turnBytes)}}},
			{Kind: domain.EventMessage, Role: "assistant", ID: fmt.Sprintf("reply-%d", i), Seq: 2*i + 1, Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic answer"}}},
		}
		raw, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		page.Turns = append(page.Turns, domain.AgentHistoryTurn{Start: 2 * i, End: 2*i + 2, Hash: domain.HashContent(raw), Events: events})
	}
	// Preserve the existing memory fixture rather than inventing another memory
	// selector. The extra loopback handler supplies one immutable history page.
	memory := f.runtime.remote
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var response any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+f.repo+"/effective-memory":
			limit, err := strconv.Atoi(q.Get("limit"))
			if err != nil {
				t.Error(err)
			}
			p, err := memory.QueryEffectiveMemory(r.Context(), f.repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: domain.ContentHash(q.Get("snapshot_id")), MemoryHash: domain.ContentHash(q.Get("memory_hash")), Branch: q.Get("branch"), CodeCommit: q.Get("code_commit")}, Content: q.Get("content"), Limit: limit, Cursor: q.Get("cursor")})
			if err != nil {
				t.Error(err)
				http.Error(w, "synthetic memory read failed", http.StatusInternalServerError)
				return
			}
			if n.memoryChanged.Load() {
				p.StateHash = domain.HashContent([]byte("changed main memory"))
			}
			response = p
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+f.repo+"/docs/"+string(page.Hash)+"/turns":
			n.pageReads.Add(1)
			if q.Get("before") != "-1" || q.Get("limit") != "16" || q.Get("max_bytes") != "4194304" || q.Get("incomplete_tail") != "omit" || q.Get("covered_by") != "" {
				t.Errorf("unexpected history page request: %v", q)
			}
			response = page
		default:
			t.Errorf("unexpected read outside declared main sources: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	f.runtime.remote = backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "fixture-token" }, domain.TeamIdentity{})
	return n
}

func (n *nativeAdmissionFixture) prepare(t *testing.T, request string) domain.AgentContextPackage {
	t.Helper()
	budget, err := domain.ParseHistoryBudget(request)
	if err != nil {
		t.Fatal(err)
	}
	// An explicit stale local source/pin must not override fresh injection policy.
	p, err := n.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: n.cwd, Provider: domain.ProviderClaude, Model: n.current.Model,
		Branch: "feature/older-code", SnapshotID: n.position.Snapshot, MemoryPin: &domain.AgentMemoryPin{SnapshotID: n.position.Snapshot, MemoryHash: n.position.MemoryHash},
		Policy: domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: budget, Source: "explicit_cli"}, InitialPrompt: n.question})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (n *nativeAdmissionFixture) bind(t *testing.T, p domain.AgentContextPackage, runner *claudeContextRunner, records *[]delivcli.ProviderLaunchReceipt) nativeClaudePreparedInput {
	t.Helper()
	prepared, err := n.reader.prepareRun(context.Background(), runner, p, n.question,
		func(ctx context.Context) error {
			return n.runtime.validateAgentDelivery(ctx, n.cwd, p.Content.Selection)
		},
		func(_ context.Context, rec delivcli.ProviderLaunchReceipt) error {
			*records = append(*records, rec.Clone())
			return nil
		},
		func(_ context.Context, stored domain.AgentContextPackage) error {
			if stored.ID != p.ID {
				t.Fatal("persisted a different package")
			}
			return stored.ValidateIdentity()
		})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestNativeAdmissionRuntimeBudgets(t *testing.T) {
	for _, tc := range []struct {
		request                  string
		window                   int64
		requested, effective     int
		limit, reserve, selected int
		turns, turnBytes         int
	}{
		{"100k", 3000000, 100000, 100000, 1600000, 100000, 1, 16, 55000},
		{"200k", 3000000, 200000, 200000, 1600000, 100000, 3, 16, 55000},
		{"300k", 3000000, 300000, 300000, 1600000, 100000, 5, 16, 55000},
		{"400k", 3000000, 400000, 400000, 1600000, 100000, 7, 16, 55000},
		{"500k", 3000000, 500000, 500000, 1600000, 100000, 8, 16, 55000},
		{"600k", 3000000, 600000, 600000, 1600000, 100000, 10, 16, 55000},
		{"700k", 3000000, 700000, 700000, 1600000, 100000, 12, 16, 55000},
		{"800k", 3000000, 800000, 800000, 1600000, 100000, 14, 16, 55000},
		{"full", 3000000, 800000, 800000, 1600000, 100000, 14, 16, 55000},
		{"", 3000000, 800000, 800000, 1600000, 100000, 14, 16, 55000},
		// A full request under a 1M runtime window legitimately clips the package.
		{"full", 1000000, 800000, 744943, 800000, 50000, 13, 16, 55000},
		// floor(.8 * 200003) - 5000 baseline - 49 framing - 8 question - 16000 reserve.
		{"full", 200003, 800000, 138945, 160002, 16000, 2, 16, 55000},
		// Sparse history is not padded to the requested allowance.
		{"full", 200003, 800000, 138945, 160002, 16000, 2, 2, 32},
	} {
		t.Run(fmt.Sprintf("%s/window%d/turns%d", tc.request, tc.window, tc.turns), func(t *testing.T) {
			n := nativeAdmissionRuntime(t, tc.window, tc.turns, tc.turnBytes)
			before, err := n.runtime.store.ReadCheckoutState(context.Background(), n.repo)
			if err != nil {
				t.Fatal(err)
			}
			p := n.prepare(t, tc.request)
			b, s := p.Budget, p.Content.Selection
			if b == nil || b.RequestedTokens != tc.requested || b.EffectiveTokens != tc.effective || b.InitialInputLimit != tc.limit || b.OverheadAllowanceTokens != tc.reserve || b.InitialPromptTokens != 8 || b.ContextWindow != int(min(2000000, tc.window)) {
				t.Fatalf("wrong budget: %+v", b)
			}
			if (b.AdjustmentReason != "") != (tc.effective < tc.requested) || p.ArtifactOnly || p.Capability != "verified_for_preparation" {
				t.Fatal("incorrect preparation status", b.AdjustmentReason, p.Capability)
			}
			if s.SourcePolicy != domain.AgentSourceLatestMain || s.Branch != "main" || s.SnapshotID != n.view.Position || s.CodeCommit != n.view.Selection.CodeCommit || s.MemoryPin != nil || s.WorkingPosition == nil || s.DeliveryBranch() != "feature/older-code" || s.DeliveryCodeCommit() != n.code || s.CodeCommit == n.code {
				t.Fatal("main source and working position were conflated", s)
			}
			prompt, err := p.Prompt()
			if err != nil || p.Usage.Tokens != len(prompt) || p.Usage.Exact || p.Usage.Tokenizer != domain.UTF8ByteBoundCounter || p.Usage.Tokens >= tc.effective || len(p.Content.History) != tc.selected || n.pageReads.Load() != 1 {
				t.Fatalf("wrong selected units/history: usage=%+v turns=%d pages=%d err=%v", p.Usage, len(p.Content.History), n.pageReads.Load(), err)
			}
			t.Logf("requested=%d effective=%d selected_utf8_allowance=%d runtime_window=%d input_limit=%d complete_turns=%d", tc.requested, tc.effective, p.Usage.Tokens, b.ContextWindow, tc.limit, tc.selected)
			for i, segment := range p.Content.History {
				turn := tc.turns - tc.selected + i
				if segment.Source.SnapshotID != s.SnapshotID || segment.Source.StartEvent != 2*turn || segment.Source.EndEvent != 2*turn+2 || len(segment.Events) != 2 || !strings.HasPrefix(segment.Events[0].Blocks[0].Text, fmt.Sprintf("main-turn-%02d ", turn)) {
					t.Fatal("history is not a complete latest-main tail", segment.Source)
				}
			}
			runner := &claudeContextRunner{summary: n.current}
			// Synthetic native estimate at the exact whole-input admission limit.
			// Baseline/reference must not be charged again at this boundary.
			runner.summary.TotalTokens = int64(tc.limit - tc.reserve - 8)
			var records []delivcli.ProviderLaunchReceipt
			prepared := n.bind(t, p, runner, &records)
			if len(records) != 1 || records[0].State != "package_prepared" || records[0].Acceptance != "unknown" || records[0].NativeInputEstimate != nil || runner.appends != 0 || runner.queries != 0 {
				t.Fatal("preparation claimed admission or ran the question")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := prepared.run(ctx)
			if err != nil || result.ReceiptError != nil || !result.Completed || runner.appends != 1 || runner.queries != 1 || runner.closes != 1 || len(records) != 3 {
				t.Fatal("synthetic admission failed", err, result.ReceiptError, len(records))
			}
			if records[1].State != "injected_ready" || records[1].Acceptance != "unknown" || records[1].NativeInputEstimate == nil || records[1].NativeInputEstimate.TotalTokens != int(runner.summary.TotalTokens) || records[2].State != "first_turn_observed" || records[2].Acceptance != "first_turn_completed" {
				t.Fatal("incorrect receipt transitions")
			}
			for _, rec := range records {
				if rec.RequestedBudget != tc.requested || rec.SelectedTokens != p.Usage.Tokens || rec.TokenMeasurement != "utf8_byte_allowance" || rec.Budget == nil || *rec.Budget != *b || rec.PackageHash != p.ID || rec.CodeCommit != n.code || rec.SourceRevision != string(s.ContextStateHash) {
					t.Fatal("receipt lost accounting/source identity")
				}
			}
			if _, err = prepared.run(ctx); err == nil || runner.appends != 1 || runner.queries != 1 || runner.closes != 1 {
				t.Fatal("one-shot admission replayed")
			}
			n.mu.Lock()
			for _, req := range n.requests {
				if req.Branch != "main" || req.SnapshotID != s.SnapshotID || req.CodeCommit != s.CodeCommit || req.MemoryHash != "" {
					t.Error("memory came from the working cursor", req)
				}
			}
			n.mu.Unlock()
			after, err := n.runtime.store.ReadCheckoutState(context.Background(), n.repo)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("admission moved the local cursor", err)
			}
		})
	}
}

func TestNativeAdmissionRuntimeRejectsStaleEvidence(t *testing.T) {
	for _, change := range []string{"main_before_append", "main_before_question", "memory_before_question", "window_before_append", "window_before_question", "compaction_before_question", "one_token_over_limit"} {
		t.Run(change, func(t *testing.T) {
			n := nativeAdmissionRuntime(t, 200003, 2, 32)
			p := n.prepare(t, "full")
			runner := &claudeContextRunner{summary: n.current}
			runner.summary.TotalTokens = 143994 // 160002 limit - 16000 reserve - 8 question.
			changeMain := func() { n.view.StateHash = domain.HashContent([]byte("new main source")) }
			runner.mutate = func(e *nativeclaude.FirstQuestionEvidence) {
				switch change {
				case "main_before_question":
					changeMain()
				case "memory_before_question":
					n.memoryChanged.Store(true)
				case "window_before_question":
					e.Summary.RawMaxTokens--
				case "compaction_before_question":
					threshold := int64(150000)
					e.Summary.AutoCompactEnabled, e.Summary.AutoCompactThreshold = true, &threshold
				case "one_token_over_limit":
					e.Summary.TotalTokens++
				}
			}
			var records []delivcli.ProviderLaunchReceipt
			prepared := n.bind(t, p, runner, &records)
			wantAppend, wantRecords := 1, 1
			cause := domain.ErrHashMismatch
			switch change {
			case "main_before_append":
				changeMain()
				wantAppend, cause = 0, domain.ErrSelectionChanged
			case "window_before_append":
				n.current.RawMaxTokens--
				wantAppend, cause = 0, domain.ErrProviderCapabilityUnknown
			case "main_before_question", "memory_before_question":
				wantRecords, cause = 2, domain.ErrSelectionChanged
			case "one_token_over_limit":
				cause = domain.ErrContextBudgetExceeded
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := prepared.run(ctx)
			if !errors.Is(err, cause) || result.Completed || runner.appends != wantAppend || runner.queries != 0 || runner.closes != 1 || len(records) != wantRecords {
				t.Fatalf("stale evidence escaped: err=%v append=%d query=%d close=%d receipts=%d", err, runner.appends, runner.queries, runner.closes, len(records))
			}
			for _, rec := range records {
				if rec.Acceptance != "unknown" || rec.State == "first_turn_observed" {
					t.Fatal("failure claimed a completed first turn")
				}
			}
			if _, err = prepared.run(ctx); err == nil || runner.queries != 0 || runner.appends != wantAppend || runner.closes != 1 {
				t.Fatal("failed admission retried")
			}
			if n.pageReads.Load() != 1 {
				t.Fatal("failure silently reselected history")
			}
		})
	}
}
