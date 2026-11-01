package domain

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

const legacyCanonicalPreparedInlineBytes = 512

func legacyCanonicalPreparedInlineArrayValid(values []any) bool {
	if values == nil || len(values) > 8 {
		return false
	}
	textBytes := 0
	for _, value := range values {
		switch typed := value.(type) {
		case nil, bool:
		case string:
			if len(typed) > 64-textBytes || !utf8.ValidString(typed) {
				return false
			}
			textBytes += len(typed)
		default:
			return false
		}
	}
	return true
}

func legacyCanonicalPreparedPackArray(values []any) []byte {
	if !legacyCanonicalPreparedInlineArrayValid(values) {
		return nil
	}
	body, err := json.Marshal(values)
	if err != nil || len(body) > legacyCanonicalPreparedInlineBytes {
		return nil
	}
	return body
}

func legacyCanonicalPreparedUnpackArray(body []byte) ([]any, error) {
	if len(body) == 0 || len(body) > legacyCanonicalPreparedInlineBytes {
		return nil, ErrIntegrity
	}
	var values []any
	if err := json.Unmarshal(body, &values); err != nil || !legacyCanonicalPreparedInlineArrayValid(values) {
		return nil, ErrIntegrity
	}
	if !bytes.Equal(legacyCanonicalPreparedPackArray(values), body) {
		return nil, ErrIntegrity
	}
	return values, nil
}
