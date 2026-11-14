package domain

import "encoding/json"

// VerifiedDocReference is a schema-validated content identity. Its private
// fields prevent wire input or adapters from minting an unchecked proof. It
// does not prove repository ownership or current physical existence.
type VerifiedDocReference struct {
	hash     ContentHash
	identity DocumentIdentity
}

func (v VerifiedDocReference) Valid() bool       { return v.hash != "" && v.identity.Validate() == nil }
func (v VerifiedDocReference) Hash() ContentHash { return v.hash }
func (v VerifiedDocReference) DocumentRef() DocumentRef {
	return DocumentRef{Hash: v.hash, Identity: v.identity}
}
func (v VerifiedDocReference) Matches(s Snapshot) bool {
	return v.Valid() && v.DocumentRef().MatchesSnapshot(s)
}
func (d VerifiedSessionDoc) Reference() VerifiedDocReference {
	if !d.Valid() {
		return VerifiedDocReference{}
	}
	return VerifiedDocReference{hash: d.Hash(), identity: d.identity}
}

// FinalizedDocReference is the reference that a completed legacy finalization
// job attests. A job becomes completed only in the transaction that verified the
// whole document and published it, so the record stands for that verification
// without the body being read again. Its identity binds the repository, the
// document hash and the exact manifest. The caller must have found the job
// under that identity for the manifest stored now, and must check that the
// repository still owns the document and its chunks. The record does not show
// that the stored bytes are still intact; readers verify the bytes they return.
func FinalizedDocReference(job DocFinalizationJob) (VerifiedDocReference, error) {
	if err := job.Validate(); err != nil {
		return VerifiedDocReference{}, err
	}
	if job.State != "completed" || job.DocIdentity != DocumentIdentityLegacy || job.RootManifest != nil {
		return VerifiedDocReference{}, ErrValidation
	}
	return VerifiedDocReference{hash: job.DocHash, identity: job.DocIdentity}, nil
}

// VerifyStoredDocBytes also accepts legacy non-canonical representations when
// decoding and canonical validation reproduce the claimed content address.
func VerifyStoredDocBytes(hash ContentHash, raw []byte) (VerifiedDocReference, error) {
	var cir CIRDocument
	if err := json.Unmarshal(raw, &cir); err != nil {
		return VerifiedDocReference{}, ErrIntegrity
	}
	_, err := validatedSessionDocBytes(SessionDoc{Hash: hash, CIR: cir}, canonicalDecodedEventJSON)
	if err != nil {
		return VerifiedDocReference{}, err
	}
	return VerifiedDocReference{hash: hash}, nil
}
