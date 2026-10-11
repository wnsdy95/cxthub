package store

import "github.com/wnsdy95/cxthub/backend/internal/domain"

func prJobAssociationNumbers(repo domain.ContentHash, numbers []int) (map[int]bool, error) {
	if domain.ValidateContentHash(repo) != nil || len(numbers) == 0 || len(numbers) > 100 {
		return nil, domain.ErrValidation
	}
	selected := make(map[int]bool, len(numbers))
	for _, number := range numbers {
		if number <= 0 {
			return nil, domain.ErrValidation
		}
		selected[number] = true
	}
	return selected, nil
}
