package domain

// VerifiedSessionDoc carries immutable canonical content after schema and hash
// validation. Its private string cannot alias caller-owned CIR or byte slices.
// This is a request-scoped value, never a persisted trust flag or wire input.
type VerifiedSessionDoc struct {
	hash      ContentHash
	canonical string
}

func VerifySessionDoc(doc SessionDoc) (VerifiedSessionDoc, error) {
	raw, err := ValidatedSessionDocBytes(doc)
	if err != nil {
		return VerifiedSessionDoc{}, err
	}
	return VerifiedSessionDoc{hash: doc.Hash, canonical: string(raw)}, nil
}

func (d VerifiedSessionDoc) Valid() bool       { return d.hash != "" && d.canonical != "" }
func (d VerifiedSessionDoc) Hash() ContentHash { return d.hash }

// Bytes returns an owned copy. A storage adapter cannot alter the proof or a
// later consumer by changing the returned buffer.
func (d VerifiedSessionDoc) Bytes() []byte { return []byte(d.canonical) }
