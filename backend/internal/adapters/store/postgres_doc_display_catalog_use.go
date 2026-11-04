//go:build postgres

package store

import "github.com/wnsdy95/cxthub/backend/internal/domain"

func docDisplayCatalogUseKey(event domain.ContentHash, profile string) string {
	return "cxthub-doc-catalog-use:" + string(event) + ":" + profile
}
