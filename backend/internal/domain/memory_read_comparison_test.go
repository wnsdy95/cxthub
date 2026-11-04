package domain

import (
	"context"
	"errors"
	"testing"
)

func memoryComparisonFixture(test *testing.T) (MemoryReadComparison, MemoryReadHeader) {
	test.Helper()
	ctx := context.Background()
	items := memoryReadTestItems(100)
	bundle := memoryReadTestBuild(test, memoryReadTestHeader(test, items), items)
	target, err := MemoryReadTargetID(ctx, MemoryReadTarget{Version: MemoryReadWorkVersion, Rules: bundle.Header.Rules,
		RepoID: bundle.Header.Source.Graph.RepoID, Selection: bundle.Header.View.Selection, Content: bundle.Header.View.Content})
	if err != nil {
		test.Fatal(err)
	}
	actual, err := MemoryReadComparisonStateHash(ctx, bundle.Header.Source.Graph.RepoID, bundle.Header.Origin, bundle.Header.View)
	if err != nil {
		test.Fatal(err)
	}
	return MemoryReadComparison{Version: MemoryReadComparisonVersion, ReadID: bundle.ID, CaptureID: bundle.Header.CaptureID,
		TargetID: target, Source: bundle.Header.Source, Expected: actual, Actual: actual}, bundle.Header
}

func TestMemoryReadComparisonBindsGenerationAndTarget(test *testing.T) {
	comparison, header := memoryComparisonFixture(test)
	ctx := context.Background()
	if err := ValidateMemoryReadComparison(ctx, comparison, header); err != nil || !comparison.Matched() {
		test.Fatal("valid comparison", err)
	}
	for name, mutate := range map[string]func(*MemoryReadComparison){
		"version":           func(value *MemoryReadComparison) { value.Version++ },
		"read":              func(value *MemoryReadComparison) { value.ReadID = HashContent([]byte("other read")) },
		"capture":           func(value *MemoryReadComparison) { value.CaptureID = HashContent([]byte("other capture")) },
		"target":            func(value *MemoryReadComparison) { value.TargetID = HashContent([]byte("other target")) },
		"graph":             func(value *MemoryReadComparison) { value.Source.Graph.Sequence++ },
		"memory":            func(value *MemoryReadComparison) { value.Source.Sequence++ },
		"actual":            func(value *MemoryReadComparison) { value.Actual = HashContent([]byte("other actual")) },
		"missing reference": func(value *MemoryReadComparison) { value.Expected = "" },
	} {
		test.Run(name, func(test *testing.T) {
			changed := comparison
			mutate(&changed)
			if err := ValidateMemoryReadComparison(ctx, changed, header); !errors.Is(err, ErrIntegrity) {
				test.Fatal("accepted another identity", err)
			}
		})
	}
	comparison.Expected = HashContent([]byte("different complete reference"))
	if err := ValidateMemoryReadComparison(ctx, comparison, header); err != nil || comparison.Matched() {
		test.Fatal("mismatch must be storable without being accepted", err)
	}
	if raw, err := MemoryReadComparisonBytes(ctx, comparison); err != nil || len(raw) > MaxMemoryReadComparisonBytes {
		test.Fatal("unbounded receipt", err)
	}
	if (MemoryReadComparison{}).Matched() {
		test.Fatal("missing proofs matched")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := ValidateMemoryReadComparison(canceled, comparison, header); !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation ignored", err)
	}
}

func TestMemoryReadComparisonCoversOffPageItemsAndMetadata(test *testing.T) {
	comparison, header := memoryComparisonFixture(test)
	ctx := context.Background()
	page := header.View
	page.Items, page.NextCursor = memoryReadTestItems(1), "arbitrary page cursor"
	actual, err := MemoryReadComparisonStateHash(ctx, header.Source.Graph.RepoID, header.Origin, page)
	if err != nil || actual != comparison.Actual {
		test.Fatal("comparison depends on page boundaries", err)
	}
	for name, mutate := range map[string]func(*EffectiveMemoryPage){
		"state":    func(view *EffectiveMemoryPage) { view.StateHash = HashContent([]byte("other state")) },
		"lineage":  func(view *EffectiveMemoryPage) { view.LineageHash = HashContent([]byte("other lineage")) },
		"revision": func(view *EffectiveMemoryPage) { view.Revision.Evidence++ },
		"last item": func(view *EffectiveMemoryPage) {
			items := memoryReadTestItems(view.Total)
			items[len(items)-1].Text = "changed outside first page"
			var err error
			view.DeliveryStateHash, err = EffectiveMemoryDeliveryStateHash(ctx, header.Source.Graph.RepoID, header.Origin, *view, items)
			if err != nil {
				test.Fatal(err)
			}
		},
	} {
		test.Run(name, func(test *testing.T) {
			changed := page
			mutate(&changed)
			actual, err := MemoryReadComparisonStateHash(ctx, header.Source.Graph.RepoID, header.Origin, changed)
			if err != nil || actual == comparison.Actual {
				test.Fatal("comparison missed semantic change", err)
			}
		})
	}
	page.DeliveryStateHash = ""
	if _, err := MemoryReadComparisonStateHash(ctx, header.Source.Graph.RepoID, header.Origin, page); !errors.Is(err, ErrIntegrity) {
		test.Fatal("partial projection was accepted", err)
	}
}
