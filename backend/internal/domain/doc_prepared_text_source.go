package domain

import "context"

func (reader *docPreparedTextReader) locateSource(ctx context.Context, from, length int, literal bool) (result DocFindSpan, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocFindSpan{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocFindSpan{}, err
	}
	if from < 0 || length <= 0 || length > reader.textBytes || from > reader.textBytes-length {
		return DocFindSpan{}, ErrValidation
	}
	first, err := reader.selectPage(ctx, from)
	if err != nil {
		return DocFindSpan{}, err
	}
	end := from + length
	if end <= first.From+len(first.Text) {
		return first.SourceRange(from-first.From, length)
	}
	last, err := reader.selectPage(ctx, end-1)
	if err != nil {
		return DocFindSpan{}, err
	}
	gap := last.From - first.From - len(first.Text)
	middle := last.Index - first.Index - 1
	before, after := first.Runs[len(first.Runs)-1], last.Runs[0]
	if middle < 0 || int64(gap) < int64(middle)*DocDisplayPageRuns || int64(gap) > int64(middle)*DocDisplayPageTextBytes || after.SourceFrom < before.SourceFrom || after.SourceTo < before.SourceTo || middle == 0 && !reader.adjacent(first, last) {
		return DocFindSpan{}, ErrIntegrity
	}
	if literal {
		rawGap := after.SourceFrom - before.SourceTo
		if !validDocJSONStringByteRatio(rawGap, gap) || middle > 0 && (gap == 0 || rawGap == 0) {
			return DocFindSpan{}, ErrIntegrity
		}
	}
	startRaw, err := docDecodedTextSourceBoundary(first, from-first.From, false)
	if err != nil {
		return DocFindSpan{}, err
	}
	endRaw, err := docDecodedTextSourceBoundary(last, end-last.From, true)
	if err != nil || startRaw >= endRaw {
		return DocFindSpan{}, ErrIntegrity
	}
	return DocFindSpan{From: startRaw, To: endRaw}, nil
}
