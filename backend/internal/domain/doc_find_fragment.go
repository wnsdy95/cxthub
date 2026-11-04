package domain

const DocFindFragmentProjection = "find-fragment-v1"
const DocLegacyFindFragmentProjection = "find-legacy-fragment-v1"

type DocFindFragmentRequest struct {
	Selection string
	Legacy    bool
	Index     int
	Total     int
	Offset    int
	Budget    int
}

type DocFindFragmentPage struct {
	Projection string      `json:"projection"`
	Hash       ContentHash `json:"hash"`
	Generation ContentHash `json:"generation"`
	Index      int         `json:"event_index"`
	Offset     int         `json:"byte_offset"`
	Total      int         `json:"total_bytes"`
	JSON       string      `json:"json_fragment"`
	Next       int         `json:"next"`
	Previous   int         `json:"previous"`
}

func DocFindFragmentSelection(repo, hash ContentHash, request DocFindFragmentRequest) (DocVisibleSelector, error) {
	if ValidateContentHash(repo) != nil || ValidateContentHash(hash) != nil || request.Offset < 0 || request.Budget < 4 || request.Budget > 64<<10 {
		return DocVisibleSelector{}, ErrValidation
	}
	if request.Legacy {
		if request.Selection != "" || request.Index < 0 || request.Total < 1 || request.Total > MaxConversationDocumentBytes || request.Index >= request.Total {
			return DocVisibleSelector{}, ErrValidation
		}
		return DocVisibleSelector{}, nil
	}
	if request.Index != 0 || request.Total != 0 {
		return DocVisibleSelector{}, ErrValidation
	}
	selection, err := DecodeDocVisibleSelector(request.Selection)
	if err != nil || selection.Repo != repo || selection.Hash != hash || selection.Purpose != "units" || selection.View != "all" || selection.Limit != 1 || selection.Next >= selection.Total {
		return DocVisibleSelector{}, ErrValidation
	}
	return selection, nil
}
