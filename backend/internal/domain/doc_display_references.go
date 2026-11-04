package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"
)

var ErrDocDisplayReferencesUnsupported = errors.New("display object reference encoding is unsupported")

const DocDisplayMaintenanceBatch = 64

type DocDisplayMaintenanceStep struct {
	Source         ContentHash
	Phase          string
	Indexed        int
	Marked         int
	Deleted        int
	DeletedBytes   int64
	CatalogCensus  string
	CatalogJobs    int
	CatalogPhase   string
	CatalogDeleted int
}

func DocDisplayHashCandidates(ctx context.Context, raw []byte) (result []ContentHash, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || !utf8.Valid(raw) {
		return nil, ErrDocDisplayReferencesUnsupported
	}
	found := make(map[ContentHash]struct{})
	depth, checked := 0, 0
	for position := 0; position < len(raw); position++ {
		if position >= checked {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			checked = position + 4096
		}
		switch raw[position] {
		case '{', '[':
			depth++
			if depth > 512 {
				return nil, ErrDocDisplayReferencesUnsupported
			}
		case '}', ']':
			depth--
		case '"':
			start, escaped := position, false
			position++
			for position < len(raw) && raw[position] != '"' {
				if position >= checked {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					checked = position + 4096
				}
				if raw[position] == '\\' {
					escaped = true
					position++
				}
				position++
			}
			if position >= len(raw) {
				return nil, ErrDocDisplayReferencesUnsupported
			}
			value := raw[start+1 : position]
			hashBytes := len(contentHashPrefix) + 64
			if len(value) < hashBytes || len(value) > hashBytes*6 {
				continue
			}
			if escaped {
				var decoded string
				if json.Unmarshal(raw[start:position+1], &decoded) != nil {
					return nil, ErrDocDisplayReferencesUnsupported
				}
				value = []byte(decoded)
			}
			if len(value) != hashBytes || !bytes.HasPrefix(value, []byte(contentHashPrefix)) {
				continue
			}
			hash := ContentHash(value)
			if ValidateContentHash(hash) == nil {
				found[hash] = struct{}{}
			}
		}
	}
	if !json.Valid(raw) {
		return nil, ErrDocDisplayReferencesUnsupported
	}
	result = make([]ContentHash, 0, len(found))
	for hash := range found {
		result = append(result, hash)
	}
	slices.Sort(result)
	return result, nil
}
