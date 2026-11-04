package domain

import (
	"encoding/json"
	"unicode/utf8"
)

type docVisibleUnitJSON struct {
	EventIndex     int `json:"event_index"`
	FoldRows       int `json:"fold_rows"`
	From           int `json:"from"`
	RenderedBefore int `json:"rendered_before"`
	To             int `json:"to"`
	TurnIndex      int `json:"turn_index"`
}

type docVisibleSpanJSON struct {
	First    int         `json:"first"`
	From     int         `json:"from"`
	Hash     ContentHash `json:"hash"`
	Limit    int         `json:"limit"`
	Rank     int         `json:"rank"`
	Rendered int         `json:"rendered"`
	To       int         `json:"to"`
}

func canonicalDocVisibleObjectJSON(value any) ([]byte, error) {
	switch object := value.(type) {
	case docVisiblePage:
		var units []docVisibleUnitJSON
		if object.Units != nil {
			units = make([]docVisibleUnitJSON, len(object.Units))
		}
		for position, unit := range object.Units {
			units[position] = docVisibleUnitJSON{
				EventIndex: unit.EventIndex, FoldRows: unit.FoldRows, From: unit.From,
				RenderedBefore: unit.RenderedBefore, To: unit.To, TurnIndex: unit.TurnIndex,
			}
		}
		return json.Marshal(struct {
			Start int                  `json:"start"`
			Units []docVisibleUnitJSON `json:"units"`
		}{Start: object.Start, Units: units})
	case docVisibleDirectory:
		var children []docVisibleSpanJSON
		if object.Children != nil {
			children = make([]docVisibleSpanJSON, len(object.Children))
		}
		for position, child := range object.Children {
			if !utf8.ValidString(string(child.Hash)) {
				return canonicalJSON(value)
			}
			children[position] = docVisibleSpanJSON{
				First: child.First, From: child.From, Hash: child.Hash, Limit: child.Limit,
				Rank: child.Rank, Rendered: child.Rendered, To: child.To,
			}
		}
		return json.Marshal(struct {
			Children []docVisibleSpanJSON `json:"children"`
			Level    int                  `json:"level"`
		}{Children: children, Level: object.Level})
	default:
		return canonicalJSON(value)
	}
}
