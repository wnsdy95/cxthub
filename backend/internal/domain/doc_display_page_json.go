package domain

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

func marshalDocDisplayPage(page DocDisplayPage) ([]byte, error) {
	if len(page.Runs) > DocDisplayPageRuns || len(page.Text) > DocDisplayPageTextBytes || len(page.Source) > len(contentHashPrefix)+64 ||
		!utf8.ValidString(page.Text) || !utf8.ValidString(string(page.Source)) {
		return canonicalJSON(page)
	}
	source, err := json.Marshal(page.Source)
	if err != nil {
		return nil, err
	}
	text, err := json.Marshal(page.Text)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 0, 128+len(source)+len(text)+len(page.Runs)*80)
	raw = append(raw, `{"from":`...)
	raw = strconv.AppendInt(raw, int64(page.From), 10)
	raw = append(raw, `,"index":`...)
	raw = strconv.AppendInt(raw, int64(page.Index), 10)
	raw = append(raw, `,"runs":`...)
	if page.Runs == nil {
		raw = append(raw, "null"...)
	} else {
		raw = append(raw, '[')
		for index, run := range page.Runs {
			if index > 0 {
				raw = append(raw, ',')
			}
			raw = append(raw, `{"linear":`...)
			raw = strconv.AppendBool(raw, run.Linear)
			raw = append(raw, `,"source_from":`...)
			raw = strconv.AppendInt(raw, int64(run.SourceFrom), 10)
			raw = append(raw, `,"source_to":`...)
			raw = strconv.AppendInt(raw, int64(run.SourceTo), 10)
			raw = append(raw, `,"to":`...)
			raw = strconv.AppendInt(raw, int64(run.To), 10)
			raw = append(raw, '}')
		}
		raw = append(raw, ']')
	}
	raw = append(raw, `,"source":`...)
	raw = append(raw, source...)
	raw = append(raw, `,"text":`...)
	raw = append(raw, text...)
	raw = append(raw, `,"version":`...)
	raw = strconv.AppendInt(raw, int64(page.Version), 10)
	return append(raw, '}'), nil
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
	var decoded DocDisplayPage
	var ok bool
	raw, decoded.From, ok = readDocDisplayRunInteger(raw, `{"from":`)
	if !ok {
		return ErrIntegrity
	}
	raw, decoded.Index, ok = readDocDisplayRunInteger(raw, `,"index":`)
	if !ok {
		return ErrIntegrity
	}
	raw, ok = bytes.CutPrefix(raw, []byte(`,"runs":`))
	if !ok {
		return ErrIntegrity
	}
	end := bytes.IndexByte(raw, ']')
	if end < 0 {
		return ErrIntegrity
	}
	var runs docDisplayRunsJSON
	if err := runs.UnmarshalJSON(raw[:end+1]); err != nil {
		return err
	}
	decoded.Runs = []DocDisplayRun(runs)
	var source string
	raw, source, ok = readDocDisplayPageString(raw[end+1:], `,"source":`)
	if !ok {
		return ErrIntegrity
	}
	decoded.Source = ContentHash(source)
	raw, decoded.Text, ok = readDocDisplayPageString(raw, `,"text":`)
	if !ok {
		return ErrIntegrity
	}
	raw, decoded.Version, ok = readDocDisplayRunInteger(raw, `,"version":`)
	if !ok || !bytes.Equal(raw, []byte("}")) {
		return ErrIntegrity
	}
	*page = decoded
	return nil
}

func readDocDisplayPageString(raw []byte, prefix string) ([]byte, string, bool) {
	raw, ok := bytes.CutPrefix(raw, []byte(prefix))
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return nil, "", false
	}
	for end := 1; end < len(raw); end++ {
		if raw[end] == '\\' {
			end++
		} else if raw[end] == '"' {
			var decoded string
			if err := json.Unmarshal(raw[:end+1], &decoded); err != nil {
				return nil, "", false
			}
			return raw[end+1:], decoded, true
		}
	}
	return nil, "", false
}
