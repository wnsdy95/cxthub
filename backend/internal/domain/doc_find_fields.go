package domain

import (
	"bytes"
	"encoding/json"
)

const MaxDocFindFieldsBytes = 2048

type DocFindSpan struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type DocFindFields struct {
	Version    int          `json:"version"`
	EventHash  ContentHash  `json:"event_hash"`
	EventBytes int          `json:"event_bytes"`
	Summary    *DocFindSpan `json:"summary"`
	Blocks     *DocFindSpan `json:"blocks,omitempty"`
}

func (fields DocFindFields) Validate(eventHash ContentHash, eventBytes int) error {
	if fields.Version != 1 || ValidateContentHash(eventHash) != nil || fields.EventHash != eventHash ||
		eventBytes <= 0 || fields.EventBytes != eventBytes || fields.Summary != nil && fields.Blocks != nil {
		return ErrIntegrity
	}
	for _, span := range []*DocFindSpan{fields.Summary, fields.Blocks} {
		if span != nil && (span.From < 0 || span.To > eventBytes || span.From >= span.To || span.To-span.From < 2) {
			return ErrIntegrity
		}
	}
	return nil
}

func (fields DocFindFields) Encode() ([]byte, error) {
	if err := fields.Validate(fields.EventHash, fields.EventBytes); err != nil {
		return nil, err
	}
	raw, err := canonicalJSON(fields)
	if err != nil || len(raw) > MaxDocFindFieldsBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocFindFields(expectedProjectionHash, eventHash ContentHash, eventBytes int, raw []byte) (DocFindFields, error) {
	var fields DocFindFields
	if len(raw) == 0 || len(raw) > MaxDocFindFieldsBytes || HashContent(raw) != expectedProjectionHash ||
		json.Unmarshal(raw, &fields) != nil || fields.Validate(eventHash, eventBytes) != nil {
		return DocFindFields{}, ErrIntegrity
	}
	canonical, err := fields.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocFindFields{}, ErrIntegrity
	}
	return fields, nil
}
