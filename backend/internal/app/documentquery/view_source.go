package documentquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// docViewSource is a conversation document opened for paged reading: its
// envelope, how many events it holds, and each event's place and bytes.
type docViewSource struct {
	envelope domain.CIREnvelope
	total    int
	entry    func(int) (domain.DocRangeEntry, error)
	location func(int) (domain.DocEventIndex, error)
	read     func(int, int) ([]byte, error)
}

// docViewSource opens a document through its stored read index.
func (queries *Queries) docViewSource(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, error) {
	ctx, release, err := queries.acquireViewBulk(ctx)
	if err != nil {
		return docViewSource{}, err
	}
	defer release()
	index, err := queries.legacyDocReadIndex(ctx, repo, hash)
	if err != nil {
		return docViewSource{}, err
	}
	return legacyDocViewSource(queries.indexedReadSource(ctx, repo, hash, index)), nil
}

func legacyDocViewSource(source docReadSource) docViewSource {
	cachedEvent := -1
	var cachedBody []byte
	var cachedEntry domain.DocRangeEntry
	entry := func(position int) (domain.DocRangeEntry, error) {
		if position < 0 || position >= len(source.index.Events) {
			return domain.DocRangeEntry{}, domain.ErrValidation
		}
		if position == cachedEvent {
			return cachedEntry, nil
		}
		item := source.index.Events[position]
		raw, err := source.read(item.Offset, item.Length)
		if err != nil {
			return domain.DocRangeEntry{}, err
		}
		metadata, err := domain.DecodeDocRangeMetadata(raw, item)
		if err != nil {
			return domain.DocRangeEntry{}, err
		}
		cachedEvent, cachedBody, cachedEntry = position, raw, metadata
		return metadata, nil
	}
	location := func(position int) (domain.DocEventIndex, error) {
		if position < 0 || position >= len(source.index.Events) {
			return domain.DocEventIndex{}, domain.ErrValidation
		}
		return source.index.Events[position], nil
	}
	read := func(offset, length int) ([]byte, error) {
		if cachedEvent >= 0 && offset >= cachedEntry.Offset && length > 0 && length <= cachedEntry.Length && offset-cachedEntry.Offset <= cachedEntry.Length-length {
			return append([]byte(nil), cachedBody[offset-cachedEntry.Offset:offset-cachedEntry.Offset+length]...), nil
		}
		return source.read(offset, length)
	}
	return docViewSource{envelope: source.index.Envelope, total: len(source.index.Events), entry: entry, location: location, read: read}
}
