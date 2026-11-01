package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	found := make(map[ContentHash]struct{})
	depth, roots := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if depth != 0 || roots != 1 {
				return nil, ErrDocDisplayReferencesUnsupported
			}
			break
		}
		if err != nil {
			return nil, ErrDocDisplayReferencesUnsupported
		}
		if depth == 0 {
			roots++
			if roots > 1 {
				return nil, ErrDocDisplayReferencesUnsupported
			}
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > 512 {
					return nil, ErrDocDisplayReferencesUnsupported
				}
			case '}', ']':
				depth--
			}
		} else if value, ok := token.(string); ok {
			hash := ContentHash(value)
			if ValidateContentHash(hash) == nil {
				found[hash] = struct{}{}
			}
		}
	}
	result = make([]ContentHash, 0, len(found))
	for hash := range found {
		result = append(result, hash)
	}
	slices.Sort(result)
	return result, nil
}
