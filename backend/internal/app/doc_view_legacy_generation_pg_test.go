//go:build postgres

package app

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGLegacyViewUsesSelectedPreparedMetadata(test *testing.T) {
	for _, rootBase := range []bool{false, true} {
		var costs []legacyFindGenerationCosts
		for _, count := range []int{2048, 20480} {
			test.Run(fmt.Sprintf("root_base=%t/events=%d", rootBase, count), func(test *testing.T) {
				fixture := newDocViewPGFixture(test)
				document, base := seedLegacyFindGenerationPairPG(test, fixture, count, rootBase)
				observed := newLegacyFindGenerationPGStore(fixture)
				fixture.service.blobs = observed
				ctx := outbound.WithDocReadOnly(fixture.ctx)
				before := legacyFindPGImage(test, fixture)
				page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, document.Hash, base, -1, 20, "chat")
				if err != nil || page.Hash != document.Hash || page.DocIdentity != domain.DocumentIdentityLegacy ||
					page.Total != count || page.Inherited != count-2 || page.Offset != count-2 || page.Next != -1 || len(page.Events) != 2 ||
					!reflect.DeepEqual(page.Envelope, document.CIR.Envelope) || !reflect.DeepEqual(page.Events[0].Event, document.CIR.Events[count-2]) ||
					!reflect.DeepEqual(page.Events[1].Event, document.CIR.Events[count-1]) {
					test.Fatal("legacy view lost original identity, envelope, inherited selection or events", page, err)
				}
				cost := observed.cost
				if cost.opens != 2 || cost.pages != 1 || observed.pages[base] != 0 || cost.boundaries != 1 || cost.manifests != 0 || cost.manifestBytes != 0 || cost.chunkLookups < 1 || cost.chunkLookups > 2 ||
					cost.chunks < 1 || cost.chunks > 2 || cost.chunkBytes > 2*domain.ChunkTarget || cost.indexes != 0 || cost.whole != 0 || cost.writes != 0 {
					test.Fatal("ready page enumerated legacy history or wrote state", cost, observed.pages)
				}
				costs = append(costs, cost)
				test.Logf("events=%d root_base=%t view cost=%+v; current v4 source reads no full manifest", count, rootBase, cost)
				observed.cost = legacyFindGenerationCosts{}
				fragment, err := fixture.service.ReadDocViewFragment(ctx, fixture.repo, document.Hash, count-1, 0, 64)
				if err != nil || fragment.Index != count-1 || fragment.Offset != 0 || !strings.Contains(fragment.JSON, "Needle") ||
					observed.cost.opens != 1 || observed.cost.pages != 1 || observed.cost.manifests != 0 || observed.cost.manifestBytes != 0 || observed.cost.chunkLookups < 1 || observed.cost.chunkLookups > 2 || observed.cost.chunks < 1 ||
					observed.cost.chunks > 2 || observed.cost.indexes != 0 || observed.cost.whole != 0 || observed.cost.writes != 0 {
					test.Fatal("ready legacy fragment did not keep selected metadata and bytes", fragment, observed.cost, err)
				}
				if after := legacyFindPGImage(test, fixture); after != before {
					test.Fatal("ready view or fragment changed source, index or publication state")
				}
			})
		}
		if len(costs) == 2 && (costs[1].metadataBytes >= 2*costs[0].metadataBytes || costs[1].manifestBytes != 0 || costs[0].manifestBytes != 0) {
			test.Fatal("selected metadata grew with history or loaded a full manifest", costs)
		}
	}
}

func TestPGLegacyViewDeferredMetadataDoesNotReadBody(test *testing.T) {
	fixture := newDocViewPGFixture(test)
	events := make([]domain.CIREvent, 2048)
	for index := range events {
		events[index] = domain.CIREvent{Kind: domain.EventToolCall, Seq: index, ToolName: "Bash", Input: map[string]any{"command": fmt.Sprintf("synthetic command %d", index)}}
	}
	document := seedLegacyFindPGDocument(test, fixture, events)
	prepareLegacyFindGenerationPG(test, fixture, document.Hash)
	observed := newLegacyFindGenerationPGStore(fixture)
	fixture.service.blobs = observed
	page, err := fixture.service.ReadDocViewEvents(outbound.WithDocReadOnly(fixture.ctx), fixture.repo, document.Hash, "", 1024, 20, "all")
	if err != nil || len(page.Events) != 20 || page.Offset != 1024 || page.Next != 1044 || page.Total != len(events) {
		test.Fatal("deferred legacy selection failed", page, err)
	}
	for position, event := range page.Events {
		if !event.Deferred || event.Index != 1024+position || event.Event.Kind != domain.EventToolCall || event.Event.ToolName != "Bash" || event.Event.Input != nil {
			test.Fatal("deferred legacy page changed metadata or exposed raw tool input", event)
		}
	}
	if observed.cost.opens != 1 || observed.cost.pages != 1 || observed.cost.manifests != 0 || observed.cost.chunks != 0 || observed.cost.indexes != 0 || observed.cost.whole != 0 {
		test.Fatal("metadata-only legacy page read body or full index", observed.cost)
	}
}
