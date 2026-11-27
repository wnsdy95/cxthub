package store

import (
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rlHash(c byte) domain.ContentHash {
	return domain.ContentHash("sha256:" + strings.Repeat(string(c), 64))
}
