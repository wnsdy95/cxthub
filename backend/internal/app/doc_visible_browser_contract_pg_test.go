//go:build postgres

package app

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGDocVisibleBrowserContract(t *testing.T) {
	type foldWire struct {
		UnitFrom int                     `json:"unit_from"`
		Pages    []domain.DocVisiblePage `json:"pages"`
	}
	type mainWire struct {
		Main  domain.DocVisiblePage `json:"main"`
		Folds []foldWire            `json:"folds"`
	}
	type selectionWire struct {
		RepoID domain.ContentHash `json:"repoId"`
		Hash   domain.ContentHash `json:"hash"`
		Base   domain.ContentHash `json:"base"`
		Start  int                `json:"start"`
		Mode   string             `json:"mode"`
	}
	type caseWire struct {
		Selection selectionWire `json:"selection"`
		Pages     []mainWire    `json:"pages"`
	}
	fixture := newDocViewPGFixture(t)
	document := visiblePGDocument(fixture, 139, 2)
	hash := seedDocViewBoundaryPG(t, fixture, document)
	previous := document
	previous.Events = previous.Events[:2]
	base := seedDocViewBoundaryPG(t, fixture, previous)
	request := domain.DocVisiblePageRequest{View: "chat", Offset: -1, Base: base}
	if _, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, request); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		t.Fatal(err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		t.Fatal(err)
	}
	bundle := struct {
		Cases []caseWire `json:"cases"`
	}{}
	for _, mode := range []string{"all", "chat", "prompts"} {
		for _, start := range []int{-1, 0, 140} {
			item := caseWire{Selection: selectionWire{fixture.repo, hash, base, start, mode}, Pages: []mainWire{}}
			pages := readAllVisiblePG(t, fixture, hash, domain.DocVisiblePageRequest{View: mode, Offset: start, Base: base, Limit: 20})
			for _, page := range pages {
				wire := mainWire{Main: page, Folds: []foldWire{}}
				for _, unit := range page.Units {
					if unit.Fold == nil {
						continue
					}
					fold := foldWire{UnitFrom: unit.From, Pages: []domain.DocVisiblePage{}}
					token := unit.Fold.Cursor
					for attempt := 0; attempt < 5; attempt++ {
						part, err := fixture.service.ReadDocVisiblePage(fixture.ctx, fixture.repo, hash, domain.DocVisiblePageRequest{Expand: token})
						if err != nil {
							t.Fatal(err)
						}
						fold.Pages = append(fold.Pages, part)
						if part.NextCursor == nil {
							break
						}
						token = *part.NextCursor
					}
					if fold.Pages[len(fold.Pages)-1].NextCursor != nil {
						t.Fatal("fold did not finish")
					}
					wire.Folds = append(wire.Folds, fold)
				}
				item.Pages = append(item.Pages, wire)
			}
			bundle.Cases = append(bundle.Cases, item)
		}
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("CXT_VISIBLE_WIRE_OUTPUT"); path != "" {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
