package app

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func h(c byte) domain.ContentHash {
	sum := sha256.Sum256([]byte{c})
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}
