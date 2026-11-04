package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

func (plan LegacyByteBackfill) rangeEntry(ctx context.Context, position, offset int, body []byte) (DocRangeEntry, error) {
	if err := ctx.Err(); err != nil {
		return DocRangeEntry{}, err
	}
	if !plan.Valid() || plan.source == nil {
		return DocRangeEntry{}, ErrIntegrity
	}
	return canonicalDocRangeEntry(ctx, position, offset, body, 0, false)
}

func canonicalDocRangeEntry(ctx context.Context, position, offset int, body []byte, sequence int, sequenceKnown bool) (DocRangeEntry, error) {
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return DocRangeEntry{}, ErrIntegrity
	}
	var metadata docRangeMetadata
	for remaining := body[1:]; len(remaining) > 1; {
		remaining = bytes.TrimLeft(remaining, ", \t\r\n")
		key, rest, err := docRangeJSONValueContext(ctx, remaining)
		if err != nil {
			return DocRangeEntry{}, err
		}
		if len(rest) == 0 || rest[0] != ':' {
			return DocRangeEntry{}, ErrIntegrity
		}
		value, rest, err := docRangeJSONValueContext(ctx, rest[1:])
		if err != nil {
			return DocRangeEntry{}, err
		}
		if len(value) == 0 || len(rest) >= len(remaining) {
			return DocRangeEntry{}, ErrIntegrity
		}
		switch string(key) {
		case `"seq"`:
			metadata.Seq = value
		case `"kind"`:
			err = json.Unmarshal(value, &metadata.Kind)
		case `"role"`:
			metadata.Role = docRangeJSONText(value, 128, false)
		case `"tool_name"`:
			metadata.ToolName = docRangeJSONText(value, 128, false)
		case `"compact_summary"`:
			err = json.Unmarshal(value, &metadata.CompactSummary)
		case `"blocks"`:
			metadata.Blocks = value
		case `"redacted_summary"`:
			metadata.RedactedSummary = value
		}
		if err != nil {
			return DocRangeEntry{}, err
		}
		remaining = rest
	}
	return docRangeMetadataEntry(ctx, metadata, position, offset, body, sequence, sequenceKnown)
}
