package domain

import (
	"crypto/sha256"
	"fmt"
)

func PRCompletionID(receiptID string) string {
	key := sha256.Sum256([]byte(receiptID + ":completed"))
	return fmt.Sprintf("%x", key[:16])
}

func HasPRCompletion(rows []HistoryEvent, receipt HistoryEvent) (bool, error) {
	completionID := PRCompletionID(receipt.ID)
	for _, event := range rows {
		if event.ID != completionID {
			continue
		}
		if event.Kind != "pr-merge" || !event.PRCompleted || event.PR == nil || receipt.PR == nil || *event.PR != *receipt.PR || event.Source != receipt.Source || event.SourceBranchID != receipt.SourceBranchID || event.BranchID != receipt.BranchID {
			return false, ErrRefConflict
		}
		return true, nil
	}
	return false, nil
}
