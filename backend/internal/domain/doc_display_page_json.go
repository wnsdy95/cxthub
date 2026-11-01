package domain

import (
	"encoding/json"
	"unicode/utf8"
)

type docDisplayRunJSON struct {
	Linear     bool `json:"linear"`
	SourceFrom int  `json:"source_from"`
	SourceTo   int  `json:"source_to"`
	To         int  `json:"to"`
}

func marshalDocDisplayPage(page DocDisplayPage) ([]byte, error) {
	if !utf8.ValidString(page.Text) || !utf8.ValidString(string(page.Source)) {
		return canonicalJSON(page)
	}
	var runs []docDisplayRunJSON
	if page.Runs != nil {
		runs = make([]docDisplayRunJSON, len(page.Runs))
	}
	for index, run := range page.Runs {
		runs[index] = docDisplayRunJSON{Linear: run.Linear, SourceFrom: run.SourceFrom, SourceTo: run.SourceTo, To: run.To}
	}
	return json.Marshal(struct {
		From    int                 `json:"from"`
		Index   int                 `json:"index"`
		Runs    []docDisplayRunJSON `json:"runs"`
		Source  ContentHash         `json:"source"`
		Text    string              `json:"text"`
		Version int                 `json:"version"`
	}{From: page.From, Index: page.Index, Runs: runs, Source: page.Source, Text: page.Text, Version: page.Version})
}
