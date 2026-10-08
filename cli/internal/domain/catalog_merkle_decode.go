package domain

func catalogMerkleInvalid(message string) error {
	return catalogInvalid("merkle " + message)
}

func catalogMerkleRefKey(key string) ([2]string, error) { return catalogRefKey(key) }

func catalogMerkleValidateEntry(repo ContentHash, entry CatalogEntry) error {
	return ValidateCatalogEntry(string(repo), entry)
}

// DecodeCatalogMerklePage rejects duplicate, null, unknown, case-aliased, or
// missing wire fields, including typed entity values. Validate must additionally
// bind the result to the caller's actual repository and original request.
func DecodeCatalogMerklePage(raw []byte) (CatalogMerklePage, error) {
	var page CatalogMerklePage
	if err := decodeCatalogJSON(raw, &page); err != nil {
		return CatalogMerklePage{}, err
	}
	request := CatalogMerkleRequest{Version: CatalogMerkleVersion, RootHash: page.RootHash, Checkpoint: &page.Checkpoint, Prefix: page.Prefix, Offset: page.Offset, Limit: MaxCatalogLimit}
	if err := page.Validate(page.RepoID, request); err != nil {
		return CatalogMerklePage{}, err
	}
	return page, nil
}
