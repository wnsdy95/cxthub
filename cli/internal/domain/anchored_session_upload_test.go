package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func anchoredUploadReport() (SegmentReport, []RecordedCommit) {
	first, second, third := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	report := SegmentReport{
		Sessions: []SegmentedSession{
			{
				Provider: ProviderCodex, ID: "parent", Scope: SessionScopeFolder, StartedAt: "2031-01-02T03:04:05.000Z", EndedAt: "2031-01-02T03:09:05.000Z",
				Records: 7, Commands: 5, CopiedRecords: 2, CopiedFrom: []string{"older"},
				Segments: []SessionSegment{
					{SegmentRange: SegmentRange{First: "t1/i1", Last: "t1/i3", Records: 3, Commands: 2, StartedAt: "2031-01-02T03:04:05.000Z", EndedAt: "2031-01-02T03:05:05.000Z"},
						Commits: []SegmentCommitLink{{SHA: first, Rule: SegmentLinkPrintedNumber}, {SHA: second, Rule: SegmentLinkPrintedNumber}}},
					{SegmentRange: SegmentRange{First: "t2/i1", Last: "t2/i2", Records: 2, Commands: 2}, ClosesTurn: true,
						Commits: []SegmentCommitLink{{SHA: third, Rule: SegmentLinkFolderAndTime}}},
				},
				Uncommitted: &UncommittedPart{SegmentRange: SegmentRange{First: "t3/i1", Last: "t3/i2", Records: 2, Commands: 1}, Places: []SessionPlace{
					{Worktree: "/work/app", Branch: "main", Rule: SessionPlaceHeadLog},
					{Worktree: "/elsewhere/app", Branch: "main", Rule: SessionPlaceHeadLog},
					{Worktree: "/work/app/.worktrees/fix", Rule: SessionPlaceCurrentBranch},
					{Worktree: "/gone/old", Removed: true, Rule: SessionPlaceRemovedFolder},
				}},
			},
			{Provider: ProviderCodex, ID: "child", Sub: true, Parent: "parent", Scope: SessionScopeOriginURL, Records: 0, Segments: []SessionSegment{}},
			{Provider: ProviderClaude, ID: "orphan", Sub: true, Parent: "not-read", Scope: SessionScopeFolder, Records: 1,
				Segments: []SessionSegment{}, Uncommitted: &UncommittedPart{SegmentRange: SegmentRange{First: "m1", Last: "m1", Records: 1}, Unplaced: true}},
		},
		Spawns: []SessionSpawn{
			{Provider: ProviderCodex, Parent: "parent", Child: "child", ParentInScope: true, AtRecord: "t1/i2", ParentSegment: 1, Rule: SpawnPointToolCall},
			{Provider: ProviderClaude, Parent: "not-read", Child: "orphan"},
		},
		AgentCalls: []SessionAgentCall{
			{Provider: ProviderCodex, Tool: "send_input", From: "parent", To: []string{"child"}, Status: "completed", At: "2031-01-02T03:04:30.000Z", Record: "t1/i2"},
			{Provider: ProviderCodex, Tool: "send_input", From: "another-repository", To: []string{"child"}},
		},
	}
	commits := []RecordedCommit{
		{SHA: first, CreatedSec: 1924916700, AuthoredSec: 1924916600, AuthorEmail: "dev@example.test", Subject: "feat: first"},
		{SHA: third, CreatedSec: 1924916800, AuthoredSec: 1924916800, AuthorEmail: "dev@example.test", Subject: "fix: third"},
	}
	return report, commits
}

func TestAnchoredUploadPlanCarriesTheReportWithoutAddingToIt(t *testing.T) {
	report, commits := anchoredUploadReport()
	plan := BuildAnchoredUploadPlan(report, commits, "laptop")
	if len(plan.Uploads) != 3 || plan.CallsWithoutSender != 1 {
		t.Fatalf("uploads=%d calls without sender=%d", len(plan.Uploads), plan.CallsWithoutSender)
	}
	parent := plan.Uploads[0]
	if parent.Provider != ProviderCodex || parent.SessionID != "parent" {
		t.Fatalf("first upload: %s %s", parent.Provider, parent.SessionID)
	}
	body := parent.Body
	if body.RuleVersion != AnchoredSessionRuleVersion || body.Machine != "laptop" || body.Records != 7 || body.Commands != 5 || body.CopiedRecords != 2 ||
		!reflect.DeepEqual(body.CopiedFrom, []string{"older"}) || body.Spawn != nil || body.Sub || body.Scope != SessionScopeFolder {
		t.Fatalf("head: %+v", body)
	}
	// Ordinals are what the record counts imply: contiguous from zero.
	var ordinals [][2]int
	for _, segment := range body.Segments {
		ordinals = append(ordinals, [2]int{segment.FirstOrdinal, segment.LastOrdinal})
	}
	ordinals = append(ordinals, [2]int{body.Uncommitted.FirstOrdinal, body.Uncommitted.LastOrdinal})
	if !reflect.DeepEqual(ordinals, [][2]int{{0, 2}, {3, 4}, {5, 6}}) {
		t.Fatalf("ordinals: %v", ordinals)
	}
	// A commit carries what Git recorded for it; one Git no longer has carries its number alone.
	if got := body.Segments[0].Commits; len(got) != 2 || got[0].CreatedAt != "2030-12-31T03:05:00Z" || got[0].AuthoredAt != "2030-12-31T03:03:20Z" ||
		got[0].AuthorEmail != "dev@example.test" || got[0].Subject != "feat: first" || got[1] != (AnchoredCommit{SHA: strings.Repeat("b", 40), Rule: SegmentLinkPrintedNumber}) {
		t.Fatalf("commits: %+v", got)
	}
	if body.Segments[0].ClosesTurn || !body.Segments[1].ClosesTurn || body.Segments[1].Commits[0].Rule != SegmentLinkFolderAndTime {
		t.Fatalf("segments: %+v", body.Segments)
	}
	// Worktrees are names, and two folders with one name are one place.
	want := []AnchoredPlace{
		{Worktree: "app", Branch: "main", Rule: SessionPlaceHeadLog},
		{Worktree: "fix", Rule: SessionPlaceCurrentBranch},
		{Worktree: "old", Removed: true, Rule: SessionPlaceRemovedFolder},
	}
	if !reflect.DeepEqual(body.Uncommitted.Places, want) || body.Uncommitted.Unplaced {
		t.Fatalf("places: %+v", body.Uncommitted.Places)
	}
	if len(body.AgentCalls) != 1 || body.AgentCalls[0].Record != "t1/i2" || !reflect.DeepEqual(body.AgentCalls[0].To, []string{"child"}) {
		t.Fatalf("agent calls: %+v", body.AgentCalls)
	}

	child := plan.Uploads[1].Body
	if !child.Sub || child.Parent != "parent" || child.Spawn == nil || *child.Spawn != (AnchoredSpawn{AtRecord: "t1/i2", Rule: SpawnPointToolCall}) ||
		child.Scope != SessionScopeOriginURL || child.Records != 0 || child.Uncommitted != nil || len(child.Segments) != 0 {
		t.Fatalf("child: %+v", child)
	}
	// A parent that was not read gives no spawn point; nothing is inferred.
	orphan := plan.Uploads[2].Body
	if orphan.Parent != "not-read" || orphan.Spawn != nil || !orphan.Uncommitted.Unplaced || len(orphan.Uncommitted.Places) != 0 {
		t.Fatalf("orphan: %+v", orphan)
	}
}

func TestAnchoredUploadBodyNeverCarriesAPath(t *testing.T) {
	report, commits := anchoredUploadReport()
	for _, upload := range BuildAnchoredUploadPlan(report, commits, "laptop").Uploads {
		raw, err := json.Marshal(upload.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/work", "/elsewhere", "/gone", ".worktrees"} {
			if strings.Contains(string(raw), path) {
				t.Fatalf("%s uploads a path: %s", upload.SessionID, raw)
			}
		}
		// A list the server always expects is never sent as null.
		if !strings.Contains(string(raw), `"segments":[`) {
			t.Fatalf("%s: %s", upload.SessionID, raw)
		}
	}
}
