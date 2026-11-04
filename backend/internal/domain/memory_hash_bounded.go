package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
)

func MemoryDigestHashBounded(digest MemoryDigest, maxBytes int) (ContentHash, error) {
	if maxBytes <= 0 {
		return "", ErrMemoryProjectionLimit
	}
	hasher := sha256.New()
	writer := accumulatorJSON{limit: maxBytes, wireOnly: true, hash: hasher}
	writer.value(reflect.ValueOf(digest))
	if writer.failed {
		return "", ErrMemoryProjectionLimit
	}
	return ContentHash("sha256:" + hex.EncodeToString(hasher.Sum(nil))), nil
}
