package domain

import "encoding/json"

// VerifiedDocReference is a schema-validated content identity. Its private
// fields prevent wire input or adapters from minting an unchecked proof. It
// does not prove repository ownership or current physical existence.
type VerifiedDocReference struct{ hash ContentHash }

func (v VerifiedDocReference) Valid() bool       { return v.hash != "" }
func (v VerifiedDocReference) Hash() ContentHash { return v.hash }
func (v VerifiedDocReference) Matches(s Snapshot) bool {
	return v.Valid() && s.ID == s.DocHash && s.DocHash == v.hash
}
func (d VerifiedSessionDoc) Reference() VerifiedDocReference {
	if !d.Valid() {
		return VerifiedDocReference{}
	}
	return VerifiedDocReference{hash: d.Hash()}
}

// VerifyStoredDocBytes also accepts legacy non-canonical representations when
// decoding and canonical validation reproduce the claimed content address.
func VerifyStoredDocBytes(hash ContentHash, raw []byte) (VerifiedDocReference, error) {
	var cir CIRDocument
	if err := json.Unmarshal(raw, &cir); err != nil {
		return VerifiedDocReference{}, ErrIntegrity
	}
	_, err := ValidatedSessionDocBytes(SessionDoc{Hash: hash, CIR: cir})
	if err != nil {
		return VerifiedDocReference{}, err
	}
	return VerifiedDocReference{hash: hash}, nil
}
