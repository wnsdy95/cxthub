package domain

func DecodeDocRangeMetadata(raw []byte, location DocEventIndex) (DocRangeEntry, error) {
	if location.Index < 0 || location.Offset < 0 || len(raw) != location.Length || location.Length == 0 {
		return DocRangeEntry{}, ErrIntegrity
	}
	entry, err := docRangeEntry(location.Index, location.Offset, raw, location.Seq)
	if err != nil || entry.Hash != location.Hash {
		return DocRangeEntry{}, ErrIntegrity
	}
	switch entry.Kind {
	case EventMessage, EventTurn, EventToolCall, EventToolResult, EventReasoning, EventCompaction:
	default:
		return DocRangeEntry{}, ErrIntegrity
	}
	entry.DocEventIndex = location
	return entry, nil
}
