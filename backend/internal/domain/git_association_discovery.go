package domain

import "math"

type GitAssociationDiscovery struct {
	RepoID     ContentHash        `json:"repo_id"`
	GitOrigin  string             `json:"git_origin"`
	Version    int64              `json:"version,string"`
	Checkpoint *CatalogCheckpoint `json:"checkpoint,omitempty"`
	Cursor     string             `json:"cursor,omitempty"`
	Epoch      string             `json:"epoch,omitempty"`
	Through    int64              `json:"through,string"`
	Mode       string             `json:"mode,omitempty"`
}

func (discovery GitAssociationDiscovery) Validate() error {
	if ValidateContentHash(discovery.RepoID) != nil || !validPRContributionIdentity(discovery.GitOrigin) || discovery.Version < 0 || len(discovery.Cursor) > 16<<10 {
		return ErrValidation
	}
	if discovery.Version == 0 && (discovery.Checkpoint != nil || discovery.Cursor != "") {
		return ErrValidation
	}
	if discovery.Checkpoint != nil {
		request := CatalogRequest{Version: CatalogVersion, After: discovery.Checkpoint}
		if request.Validate() != nil || discovery.Checkpoint.RepoID != discovery.RepoID {
			return ErrValidation
		}
	}
	if discovery.Cursor == "" {
		if discovery.Epoch != "" || discovery.Through != 0 || discovery.Mode != "" {
			return ErrValidation
		}
	} else if !catalogEpochValid(discovery.Epoch) || discovery.Through < 0 || (discovery.Mode != "baseline" && discovery.Mode != "delta") {
		return ErrValidation
	}
	if discovery.Mode == "delta" && (discovery.Checkpoint == nil || discovery.Checkpoint.Epoch != discovery.Epoch || discovery.Through < discovery.Checkpoint.Sequence) {
		return ErrValidation
	}
	if discovery.Mode == "baseline" && discovery.Checkpoint != nil {
		return ErrValidation
	}
	return nil
}

func (discovery GitAssociationDiscovery) Request() CatalogRequest {
	request := CatalogRequest{Version: CatalogVersion, Cursor: discovery.Cursor, Limit: MaxCatalogLimit}
	if discovery.Cursor == "" && discovery.Checkpoint != nil {
		checkpoint := *discovery.Checkpoint
		request.After = &checkpoint
	}
	return request
}

func (discovery GitAssociationDiscovery) Reset() (GitAssociationDiscovery, error) {
	if discovery.Validate() != nil || discovery.Version == math.MaxInt64 {
		return GitAssociationDiscovery{}, ErrValidation
	}
	return GitAssociationDiscovery{RepoID: discovery.RepoID, GitOrigin: discovery.GitOrigin, Version: discovery.Version + 1}, nil
}

func (discovery GitAssociationDiscovery) Advance(page CatalogPage) (GitAssociationDiscovery, error) {
	var empty GitAssociationDiscovery
	if discovery.Validate() != nil || discovery.Version == math.MaxInt64 || page.Version != CatalogVersion || page.RepoID != discovery.RepoID || !catalogEpochValid(page.Epoch) || page.Through < 0 || len(page.Entries) > MaxCatalogLimit || (page.Mode != "baseline" && page.Mode != "delta") {
		return empty, ErrIntegrity
	}
	if discovery.Cursor != "" {
		if page.Epoch != discovery.Epoch || page.Through != discovery.Through || page.Mode != discovery.Mode || page.NextCursor == discovery.Cursor {
			return empty, ErrIntegrity
		}
	} else if discovery.Checkpoint == nil {
		if page.Mode != "baseline" {
			return empty, ErrIntegrity
		}
	} else if page.Mode != "delta" || page.Epoch != discovery.Checkpoint.Epoch || page.Through < discovery.Checkpoint.Sequence {
		return empty, ErrIntegrity
	}
	for _, entry := range page.Entries {
		if entry.Sequence < 1 || entry.Sequence > page.Through || entry.Key == "" || (page.Mode == "delta" && entry.Sequence <= discovery.Checkpoint.Sequence) {
			return empty, ErrIntegrity
		}
		switch entry.Kind {
		case "snapshot", "ref", "history", "protocol":
		default:
			return empty, ErrIntegrity
		}
	}
	next := discovery
	if discovery.Checkpoint != nil {
		checkpoint := *discovery.Checkpoint
		next.Checkpoint = &checkpoint
	}
	next.Version++
	if page.NextCursor != "" {
		if page.Checkpoint != nil || len(page.Entries) == 0 {
			return empty, ErrIntegrity
		}
		next.Cursor, next.Epoch, next.Through, next.Mode = page.NextCursor, page.Epoch, page.Through, page.Mode
	} else {
		if page.Checkpoint == nil || page.Checkpoint.Version != CatalogVersion || page.Checkpoint.RepoID != page.RepoID || page.Checkpoint.Epoch != page.Epoch || page.Checkpoint.Sequence != page.Through {
			return empty, ErrIntegrity
		}
		checkpoint := *page.Checkpoint
		next.Checkpoint = &checkpoint
		next.Cursor, next.Epoch, next.Through, next.Mode = "", "", 0, ""
	}
	if next.Validate() != nil {
		return empty, ErrIntegrity
	}
	return next, nil
}
