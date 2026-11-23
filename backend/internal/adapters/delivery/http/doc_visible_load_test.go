//go:build postgres

package http

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rangeLoadVisibleExpected(doc rangeLoadDocument, view string, start, limit int) ([]domain.DocVisibleUnit, int) {
	units := []domain.DocVisibleUnit{}
	first, ordinal, turn := 0, 0, -1
	for position := 0; position < len(doc.Events) && len(units) < limit; {
		event := doc.Events[position]
		prompt := event.Kind == domain.EventMessage && event.Role == domain.RoleUser
		if prompt {
			turn = position
		}
		unit := domain.DocVisibleUnit{From: position, To: position + 1, TurnIndex: turn, EventIndex: -1}
		switch view {
		case "all":
			unit.EventIndex = position
		case "chat":
			for {
				if doc.Events[unit.To-1].Kind == domain.EventMessage {
					unit.EventIndex = unit.To - 1
					break
				}
				if unit.To == len(doc.Events) || doc.Events[unit.To].Role == domain.RoleUser {
					break
				}
				unit.To++
			}
		case "prompts":
			if prompt {
				unit.EventIndex = position
			}
			for unit.To < len(doc.Events) && doc.Events[unit.To].Role != domain.RoleUser {
				unit.To++
			}
		default:
			panic("invalid fixture view")
		}
		position = unit.To
		if unit.To > start {
			if len(units) == 0 {
				first = ordinal
			}
			unit.From = max(unit.From, start)
			if unit.EventIndex < unit.From {
				unit.EventIndex = -1
			}
			unit.FoldRows = unit.To - unit.From
			if unit.EventIndex >= 0 {
				unit.FoldRows--
			}
			units = append(units, unit)
		}
		ordinal++
	}
	return units, first
}

func TestDocVisibleLoadFixtureOracle(t *testing.T) {
	doc := rangeLoadBuild(t, "visible-oracle", 2<<20, func(domain.ContentHash, []byte) error { return nil })
	for _, test := range []struct {
		view                                        string
		start, from, to, event, turn, rows, ordinal int
	}{
		{"all", 1, 1, 2, 1, 0, 0, 1},
		{"chat", 1, 1, 140, -1, 0, 139, 1},
		{"chat", 142, 142, 146, 145, 140, 3, 3},
		{"prompts", 0, 0, 140, 0, 0, 139, 0},
		{"prompts", 1, 1, 140, -1, 0, 139, 0},
		{"prompts", 142, 142, 150, -1, 140, 8, 1},
	} {
		units, ordinal := rangeLoadVisibleExpected(doc, test.view, test.start, 20)
		if len(units) != 20 || ordinal != test.ordinal {
			t.Fatalf("%+v count/ordinal=%d/%d", test, len(units), ordinal)
		}
		unit := units[0]
		if unit.From != test.from || unit.To != test.to || unit.EventIndex != test.event || unit.TurnIndex != test.turn || unit.FoldRows != test.rows {
			t.Fatalf("%+v unit=%+v", test, unit)
		}
	}
}
