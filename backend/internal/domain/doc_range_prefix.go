package domain

import "context"

const DocRangePrefixAlgorithm = 1
const DocRangePrefixBatchPages = 64

type DocRangePrefixSource struct {
	Total    int
	PageHash func(context.Context, int) (ContentHash, error)
	ReadPage func(context.Context, int) ([]DocRangeEntry, error)
}

type DocRangePrefixProgress struct {
	Events   int
	Complete bool
}

func CompareDocRangePrefix(ctx context.Context, left, right DocRangePrefixSource, start, pageBudget int) (DocRangePrefixProgress, error) {
	if err := ctx.Err(); err != nil {
		return DocRangePrefixProgress{}, err
	}
	total := min(left.Total, right.Total)
	if left.Total < 0 || right.Total < 0 || left.Total > MaxConversationDocumentBytes || right.Total > MaxConversationDocumentBytes || start < 0 || start > total ||
		(start != total && start%DocRangePageEvents != 0) || pageBudget < 1 || pageBudget > DocRangePrefixBatchPages ||
		left.PageHash == nil || right.PageHash == nil || left.ReadPage == nil || right.ReadPage == nil {
		return DocRangePrefixProgress{}, ErrValidation
	}
	progress := DocRangePrefixProgress{Events: start}
	for pages := 0; progress.Events < total && pages < pageBudget; pages++ {
		if err := ctx.Err(); err != nil {
			return DocRangePrefixProgress{}, err
		}
		page := progress.Events / DocRangePageEvents
		leftHash, err := left.PageHash(ctx, page)
		if err != nil {
			return DocRangePrefixProgress{}, err
		}
		rightHash, err := right.PageHash(ctx, page)
		if err != nil {
			return DocRangePrefixProgress{}, err
		}
		if ValidateContentHash(leftHash) != nil || ValidateContentHash(rightHash) != nil {
			return DocRangePrefixProgress{}, ErrIntegrity
		}
		end := min(total, (page+1)*DocRangePageEvents)
		if leftHash == rightHash {
			progress.Events = end
			continue
		}
		leftEntries, err := left.ReadPage(ctx, page)
		if err != nil {
			return DocRangePrefixProgress{}, err
		}
		rightEntries, err := right.ReadPage(ctx, page)
		if err != nil {
			return DocRangePrefixProgress{}, err
		}
		if len(leftEntries) != min(DocRangePageEvents, left.Total-page*DocRangePageEvents) ||
			len(rightEntries) != min(DocRangePageEvents, right.Total-page*DocRangePageEvents) {
			return DocRangePrefixProgress{}, ErrIntegrity
		}
		for progress.Events < end {
			if err := ctx.Err(); err != nil {
				return DocRangePrefixProgress{}, err
			}
			ordinal := progress.Events % DocRangePageEvents
			leftEvent, rightEvent := leftEntries[ordinal].Hash, rightEntries[ordinal].Hash
			if ValidateContentHash(leftEvent) != nil || ValidateContentHash(rightEvent) != nil {
				return DocRangePrefixProgress{}, ErrIntegrity
			}
			if leftEvent != rightEvent {
				progress.Complete = true
				return progress, nil
			}
			progress.Events++
		}
	}
	progress.Complete = progress.Events == total
	if err := ctx.Err(); err != nil {
		return DocRangePrefixProgress{}, err
	}
	return progress, nil
}
