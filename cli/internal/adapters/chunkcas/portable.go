package chunkcas

import "encoding/json"

// These limits mirror backend/internal/domain/doc_job.go and the document-job
// contract in schemas/openapi.yaml. Counts include repeated ordered IDs.
const (
	MaxPortableManifestChunks = 2048
	MaxPortableManifestBytes  = 256 << 10
	MaxPortableDocBytes       = 512 << 20
)

// PortableManifest checks manifest admission bounds, not canonical identity or
// current chunk bodies. Callers must validate those before treating false as a
// clean fallback. The assembled byte limit needs separately verified body sizes.
func PortableManifest(manifest Manifest) bool {
	if !SupportedFormat(manifest.Format) || len(manifest.Chunks) == 0 || len(manifest.Chunks) > MaxPortableManifestChunks || len(manifest.Envelope) > MaxPortableManifestBytes {
		return false
	}
	// The server normalizes the legacy empty format before encoding/admission.
	manifest.Format = normalizeFormat(manifest.Format)
	encoded, err := json.Marshal(manifest)
	return err == nil && len(encoded) <= MaxPortableManifestBytes
}
