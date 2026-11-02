package http

import (
	"net/url"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docLegacyFindPageRequest(query url.Values, repo, hash domain.ContentHash) (domain.DocFindPageRequest, error) {
	var request domain.DocFindPageRequest
	for key, values := range query {
		switch key {
		case "q", "legacy", "from", "to", "total", "inherited", "base", "cursor", "representation":
		default:
			return request, domain.ErrValidation
		}
		if len(values) != 1 || values[0] == "" {
			return request, domain.ErrValidation
		}
	}
	if query.Get("legacy") != "1" {
		return request, domain.ErrValidation
	}
	request.Query, request.Legacy = query.Get("q"), true
	request.Representation = query.Get("representation")
	keyCount := len(query)
	if request.Representation != "" {
		if request.Representation != domain.DocFindDisplayRepresentation && request.Representation != domain.DocFindPreparedRepresentation {
			return request, domain.ErrValidation
		}
		keyCount--
	}
	if query.Has("cursor") {
		if keyCount != 3 {
			return request, domain.ErrValidation
		}
		request.Cursor = query.Get("cursor")
	} else {
		if keyCount != 6 && !(keyCount == 7 && query.Has("base")) {
			return request, domain.ErrValidation
		}
		request.Base = domain.ContentHash(query.Get("base"))
		for key, target := range map[string]*int{"from": &request.From, "to": &request.To, "total": &request.Total, "inherited": &request.Inherited} {
			raw := query.Get(key)
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || value > domain.MaxConversationDocumentBytes || strconv.Itoa(value) != raw {
				return request, domain.ErrValidation
			}
			*target = value
		}
	}
	if _, err := domain.DocLegacyFindSelection(repo, hash, request); err != nil {
		return request, err
	}
	return request, nil
}
