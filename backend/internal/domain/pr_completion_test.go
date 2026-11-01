package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPRCompletionID(test *testing.T) {
	receipt, completion := prCompletionFixture()
	completionID := PRCompletionID(receipt.ID)
	if completionID != completion.ID {
		test.Fatalf("completion ID = %q; want %q", completionID, completion.ID)
	}
	for _, receiptID := range []string{"", "1123456789abcdef0123456789abcdef", receipt.ID + ":completed"} {
		otherID := PRCompletionID(receiptID)
		if otherID == completionID {
			test.Fatalf("different receipt %q produced the same completion ID %q", receiptID, otherID)
		}
		if repeatedID := PRCompletionID(receiptID); repeatedID != otherID {
			test.Fatalf("receipt %q produced different completion IDs: %q and %q", receiptID, otherID, repeatedID)
		}
	}
	if repeatedID := PRCompletionID(receipt.ID); repeatedID != completionID {
		test.Fatalf("receipt %q produced different completion IDs: %q and %q", receipt.ID, completionID, repeatedID)
	}
}

func TestHasPRCompletionValid(test *testing.T) {
	receipt, completion := prCompletionFixture()
	if receipt.PR == completion.PR || *receipt.PR != *completion.PR {
		test.Fatal("fixture must use distinct PR pointers with equal values")
	}
	completed, err := HasPRCompletion([]HistoryEvent{receipt, completion}, receipt)
	if !completed || err != nil {
		test.Fatalf("completion = %t, %v; want true, nil", completed, err)
	}
}

func TestHasPRCompletionMissing(test *testing.T) {
	receipt, completion := prCompletionFixture()
	for _, scenario := range []struct {
		name string
		rows []HistoryEvent
	}{
		{name: "nil history"},
		{name: "empty history", rows: []HistoryEvent{}},
		{name: "pending receipt only", rows: []HistoryEvent{receipt}},
		{name: "unrelated malformed row", rows: []HistoryEvent{{ID: "unrelated"}}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			completed, err := HasPRCompletion(scenario.rows, receipt)
			if completed || err != nil {
				test.Fatalf("completion = %t, %v; want false, nil", completed, err)
			}
		})
	}
	for _, identity := range []string{
		"",
		receipt.ID,
		receipt.ID + ":completed",
		strings.ToUpper(completion.ID),
		completion.ID[:16],
		"0b13ffaf2a57fd5b2e0e8317101290f9b880339b7d0bcb8e2a46bbe7d9568289",
		"b880339b7d0bcb8e2a46bbe7d9568289",
	} {
		test.Run("nonmatching ID/"+identity, func(test *testing.T) {
			unrelated := completion
			unrelated.ID = identity
			completed, err := HasPRCompletion([]HistoryEvent{unrelated}, receipt)
			if completed || err != nil {
				test.Fatalf("completion = %t, %v; want false, nil", completed, err)
			}
		})
	}
}

func TestHasPRCompletionConflictingFields(test *testing.T) {
	for _, field := range []string{
		"kind",
		"completion marker",
		"nil event PR",
		"PR number",
		"PR base branch",
		"PR head branch",
		"PR head SHA",
		"PR merge SHA",
		"source",
		"source branch identity",
		"destination branch identity",
	} {
		test.Run(field, func(test *testing.T) {
			receipt, completion := prCompletionFixture()
			switch field {
			case "kind":
				completion.Kind = "position"
			case "completion marker":
				completion.PRCompleted = false
			case "nil event PR":
				completion.PR = nil
			case "PR number":
				completion.PR.Number++
			case "PR base branch":
				completion.PR.BaseBranch = "other-main"
			case "PR head branch":
				completion.PR.HeadBranch = "other-feature"
			case "PR head SHA":
				completion.PR.HeadSHA = strings.Repeat("c", 40)
			case "PR merge SHA":
				completion.PR.MergeSHA = strings.Repeat("d", 40)
			case "source":
				completion.Source = HashContent([]byte("other-source"))
			case "source branch identity":
				completion.SourceBranchID = "other-feature-id"
			case "destination branch identity":
				completion.BranchID = "other-main-id"
			}
			completed, err := HasPRCompletion([]HistoryEvent{completion}, receipt)
			if completed || err != ErrRefConflict {
				test.Fatalf("completion = %t, %v; want false, ErrRefConflict", completed, err)
			}
		})
	}
}

func TestHasPRCompletionIgnoresPlacementFields(test *testing.T) {
	for _, field := range []string{
		"target",
		"empty target",
		"shared target",
		"empty shared target",
		"completion time",
		"completion branch name",
		"receipt target",
		"receipt shared target",
		"receipt time",
	} {
		test.Run(field, func(test *testing.T) {
			receipt, completion := prCompletionFixture()
			switch field {
			case "target":
				completion.Target = HashContent([]byte("retained-current-tip"))
			case "empty target":
				completion.Target = ""
			case "shared target":
				completion.SharedTarget = HashContent([]byte("other-before"))
			case "empty shared target":
				completion.SharedTarget = ""
			case "completion time":
				completion.CreatedAt = time.Time{}
			case "completion branch name":
				completion.Branch = "renamed-main"
			case "receipt target":
				receipt.Target = HashContent([]byte("other-receipt-target"))
			case "receipt shared target":
				receipt.SharedTarget = HashContent([]byte("other-receipt-before"))
			case "receipt time":
				receipt.CreatedAt = completion.CreatedAt.Add(time.Hour)
			}
			completed, err := HasPRCompletion([]HistoryEvent{completion}, receipt)
			if !completed || err != nil {
				test.Fatalf("completion = %t, %v; want true, nil", completed, err)
			}
		})
	}
}

func TestHasPRCompletionSurvivesRepositionAndGraftChange(test *testing.T) {
	receipt, completion := prCompletionFixture()
	previous := HashContent([]byte("previous"))
	view := RepositoryView{
		History:   []HistoryEvent{receipt, completion},
		Refs:      []Ref{{Kind: RefBranch, Name: receipt.Branch, BranchID: receipt.BranchID, Target: completion.Target}},
		Snapshots: []Snapshot{{ID: receipt.Source, GraftParents: []ContentHash{previous}}, {ID: previous}},
	}
	completed, err := HasPRCompletion(view.History, receipt)
	if !completed || err != nil {
		test.Fatalf("initial completion = %t, %v; want true, nil", completed, err)
	}
	view.Snapshots[0].GraftParents = nil
	view.Snapshots[1].GraftParents = []ContentHash{receipt.Source}
	view.Refs[0].Target = previous
	view.History = append(view.History, HistoryEvent{
		ID:        "later-reposition",
		Kind:      "position",
		Branch:    receipt.Branch,
		BranchID:  receipt.BranchID,
		Source:    receipt.Source,
		Target:    previous,
		CreatedAt: completion.CreatedAt.Add(time.Hour),
	})
	completed, err = HasPRCompletion(view.History, receipt)
	if !completed || err != nil {
		test.Fatalf("repositioned completion = %t, %v; want true, nil", completed, err)
	}
	view.Snapshots = nil
	view.Refs = nil
	completed, err = HasPRCompletion(view.History, receipt)
	if !completed || err != nil {
		test.Fatalf("completion without current placement = %t, %v; want true, nil", completed, err)
	}
}

func TestHasPRCompletionUsesFirstMatchingID(test *testing.T) {
	receipt, completion := prCompletionFixture()
	conflict := completion
	conflict.Source = HashContent([]byte("conflicting-source"))
	nilPR := completion
	nilPR.PR = nil
	unrelated := conflict
	unrelated.ID = "unrelated"
	for _, scenario := range []struct {
		name      string
		rows      []HistoryEvent
		completed bool
		wantError error
	}{
		{"identical duplicates", []HistoryEvent{completion, completion}, true, nil},
		{"valid before conflict", []HistoryEvent{completion, conflict}, true, nil},
		{"conflict before valid", []HistoryEvent{conflict, completion}, false, ErrRefConflict},
		{"valid before nil PR", []HistoryEvent{completion, nilPR}, true, nil},
		{"nil PR before valid", []HistoryEvent{nilPR, completion}, false, ErrRefConflict},
		{"skip unrelated conflict", []HistoryEvent{unrelated, receipt, completion, conflict}, true, nil},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			completed, err := HasPRCompletion(scenario.rows, receipt)
			if completed != scenario.completed || err != scenario.wantError {
				test.Fatalf("completion = %t, %v; want %t, %v", completed, err, scenario.completed, scenario.wantError)
			}
		})
	}
}

func TestHasPRCompletionNilReceiptPRFailsClosedWithoutPanic(test *testing.T) {
	receipt, completion := prCompletionFixture()
	receipt.PR = nil
	nilPR := completion
	nilPR.PR = nil
	for _, scenario := range []struct {
		name      string
		rows      []HistoryEvent
		wantError error
	}{
		{"matching completion fails closed", []HistoryEvent{completion}, ErrRefConflict},
		{"both PR pointers nil fail closed", []HistoryEvent{nilPR}, ErrRefConflict},
		{"missing completion stays missing", nil, nil},
		{"nonmatching row stays missing", []HistoryEvent{receipt}, nil},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			completed, err := HasPRCompletion(scenario.rows, receipt)
			if completed || err != scenario.wantError {
				test.Fatalf("completion = %t, %v; want false, %v", completed, err, scenario.wantError)
			}
		})
	}
}

func TestHasPRCompletionDoesNotMutateInputs(test *testing.T) {
	for _, scenario := range []string{"valid", "conflict", "missing", "nil receipt PR"} {
		test.Run(scenario, func(test *testing.T) {
			receipt, completion := prCompletionFixture()
			conflict := completion
			conflict.SourceBranchID = "conflicting-feature-id"
			rows := []HistoryEvent{receipt, completion, conflict}
			switch scenario {
			case "conflict":
				rows = []HistoryEvent{receipt, conflict, completion}
			case "missing":
				rows = []HistoryEvent{receipt}
			case "nil receipt PR":
				receipt.PR = nil
			}
			beforeRows := append([]HistoryEvent(nil), rows...)
			for index := range beforeRows {
				if beforeRows[index].PR != nil {
					beforePR := *beforeRows[index].PR
					beforeRows[index].PR = &beforePR
				}
			}
			beforeReceipt := receipt
			if receipt.PR != nil {
				beforePR := *receipt.PR
				beforeReceipt.PR = &beforePR
			}
			HasPRCompletion(rows, receipt)
			if !reflect.DeepEqual(rows, beforeRows) || !reflect.DeepEqual(receipt, beforeReceipt) {
				test.Fatal("completion check modified source history or receipt")
			}
		})
	}
}

func prCompletionFixture() (HistoryEvent, HistoryEvent) {
	receipt := HistoryEvent{
		ID:             "0123456789abcdef0123456789abcdef",
		RepoID:         string(HashContent([]byte("repository"))),
		Kind:           "pr-merge",
		Branch:         "main",
		BranchID:       "main-id",
		SourceBranchID: "feature-id",
		Source:         HashContent([]byte("source")),
		Target:         HashContent([]byte("source")),
		PR: &PullRequestMerge{
			Number:     17,
			BaseBranch: "main",
			HeadBranch: "feature",
			HeadSHA:    strings.Repeat("a", 40),
			MergeSHA:   strings.Repeat("b", 40),
		},
		CreatedAt: time.Date(2026, time.October, 27, 1, 0, 0, 0, time.UTC),
	}
	completion := receipt
	completion.ID = "0b13ffaf2a57fd5b2e0e8317101290f9"
	completion.PRCompleted = true
	completion.SharedTarget = HashContent([]byte("before"))
	completion.CreatedAt = receipt.CreatedAt.Add(time.Minute)
	completionPR := *receipt.PR
	completion.PR = &completionPR
	return receipt, completion
}
