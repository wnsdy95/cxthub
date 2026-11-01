package domain

import (
	"bytes"
	"encoding/json"
	"strconv"
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

type docDisplayRunsJSON []DocDisplayRun

func (decoded *docDisplayRunsJSON) UnmarshalJSON(raw []byte) error {
	raw, ok := bytes.CutPrefix(raw, []byte("["))
	if !ok {
		return ErrIntegrity
	}
	runs := make([]DocDisplayRun, 0, min(DocDisplayPageRuns, len(raw)/len(`{"linear":true,"source_from":0,"source_to":0,"to":0}`)))
	for {
		if len(runs) == DocDisplayPageRuns {
			return ErrIntegrity
		}
		raw, ok = bytes.CutPrefix(raw, []byte(`{"linear":`))
		if !ok {
			return ErrIntegrity
		}
		var run DocDisplayRun
		if remaining, matched := bytes.CutPrefix(raw, []byte("true")); matched {
			raw, run.Linear = remaining, true
		} else if remaining, matched := bytes.CutPrefix(raw, []byte("false")); matched {
			raw = remaining
		} else {
			return ErrIntegrity
		}
		raw, run.SourceFrom, ok = readDocDisplayRunInteger(raw, `,"source_from":`)
		if !ok {
			return ErrIntegrity
		}
		raw, run.SourceTo, ok = readDocDisplayRunInteger(raw, `,"source_to":`)
		if !ok {
			return ErrIntegrity
		}
		raw, run.To, ok = readDocDisplayRunInteger(raw, `,"to":`)
		if !ok {
			return ErrIntegrity
		}
		raw, ok = bytes.CutPrefix(raw, []byte("}"))
		if !ok {
			return ErrIntegrity
		}
		runs = append(runs, run)
		if bytes.Equal(raw, []byte("]")) {
			*decoded = runs
			return nil
		}
		raw, ok = bytes.CutPrefix(raw, []byte(","))
		if !ok {
			return ErrIntegrity
		}
	}
}

func readDocDisplayRunInteger(raw []byte, prefix string) ([]byte, int, bool) {
	raw, ok := bytes.CutPrefix(raw, []byte(prefix))
	if !ok {
		return nil, 0, false
	}
	end := 0
	for end < len(raw) && raw[end] >= '0' && raw[end] <= '9' {
		end++
	}
	value, err := strconv.Atoi(string(raw[:end]))
	return raw[end:], value, err == nil
}

func unmarshalDocDisplayPage(raw []byte, page *DocDisplayPage) error {
	var decoded struct {
		From    int                `json:"from"`
		Index   int                `json:"index"`
		Runs    docDisplayRunsJSON `json:"runs"`
		Source  ContentHash        `json:"source"`
		Text    string             `json:"text"`
		Version int                `json:"version"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*page = DocDisplayPage{From: decoded.From, Index: decoded.Index, Runs: []DocDisplayRun(decoded.Runs), Source: decoded.Source, Text: decoded.Text, Version: decoded.Version}
	return nil
}
