package domain

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestPublicationLegacyJobGolden(t *testing.T) {
	_, vectors := publicationVectors(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			doc := publicationDoc(t, v)
			old, err := NewDocFinalizationJob(v.Repo, doc.Hash, DocChunkManifest{Format: doc.Format, Envelope: doc.Envelope, Chunks: doc.Chunks}, now)
			if err != nil || old.ID != v.ID {
				t.Fatalf("legacy ID changed: %s %v", old.ID, err)
			}
			pre, _ := json.Marshal(struct {
				Repo     ContentHash
				Hash     ContentHash
				Manifest DocChunkManifest
			}{old.RepoID, old.DocHash, old.Manifest})
			if string(pre) != v.Preimage {
				t.Fatalf("legacy preimage changed: %s", pre)
			}
			got, err := NewDocFinalizationJobForRepresentation(v.Repo, doc, now)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(old)
			b, _ := json.Marshal(got)
			if !bytes.Equal(a, b) || bytes.Contains(a, []byte(`doc_identity`)) || bytes.Contains(a, []byte(`root_manifest`)) {
				t.Fatal("legacy job serialization drift")
			}
			rep, err := got.Representation()
			if err != nil || rep.Format == "" {
				t.Fatal("legacy representation", err)
			}
			rep.Envelope[0] = '!'
			rep.Chunks[0] = "invalid"
			if got.Validate() != nil {
				t.Fatal("job accessor aliases")
			}
		})
	}
}
func TestPublicationRootJobVariantOwnershipAndFences(t *testing.T) {
	roots, _ := publicationVectors(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, v := range roots {
		t.Run(v.Name, func(t *testing.T) {
			doc := publicationDoc(t, v)
			job, err := NewDocFinalizationJobForRepresentation(v.Repo, doc, now)
			if err != nil || job.ID != v.ID || job.DocumentRef() != doc.DocumentRef() {
				t.Fatalf("construct %v", err)
			}
			doc.RootManifest[0] = '!'
			if err = job.Validate(); err != nil {
				t.Fatal("constructor aliases input", err)
			}
			rep, err := job.Representation()
			if err != nil {
				t.Fatal(err)
			}
			rep.RootManifest[0] = '!'
			if job.Validate() != nil {
				t.Fatal("accessor aliases")
			}
			raw, _ := json.Marshal(job)
			var round DocFinalizationJob
			if err = json.Unmarshal(raw, &round); err != nil || round.Validate() != nil {
				t.Fatal("durable round trip", err)
			}
			claim := job.Claim(now, time.Minute)
			if !claim.Fences(claim, now) {
				t.Fatal("valid fence")
			}
			for name, mutate := range map[string]func(*DocFinalizationJob){
				"stripped":         func(j *DocFinalizationJob) { j.DocIdentity = "" },
				"unknown":          func(j *DocFinalizationJob) { j.DocIdentity = "future" },
				"mixed":            func(j *DocFinalizationJob) { j.Manifest.Chunks = []ContentHash{} },
				"hash":             func(j *DocFinalizationJob) { j.DocHash = HashContent([]byte("wrong")) },
				"repo":             func(j *DocFinalizationJob) { j.RepoID = HashContent([]byte("wrong")) },
				"invalid-manifest": func(j *DocFinalizationJob) { j.RootManifest = json.RawMessage(`{}`) },
				"missing-manifest": func(j *DocFinalizationJob) { j.RootManifest = nil },
				"id":               func(j *DocFinalizationJob) { j.ID = string(HashContent([]byte("wrong"))) },
			} {
				t.Run(name, func(t *testing.T) {
					bad := claim
					mutate(&bad)
					if bad.Validate() == nil || bad.Fences(claim, now) || claim.Fences(bad, now) {
						t.Fatal("invalid root accepted or fenced")
					}
				})
			}
			malformed := bytes.Replace(raw, []byte(`"doc_identity":`), []byte(`"doc_identity":"","Doc_Identity":`), 1)
			if json.Unmarshal(malformed, &round) == nil {
				t.Fatal("duplicate root identity accepted")
			}
			if claim.Fences(claim, now.Add(time.Minute)) {
				t.Fatal("expired lease")
			}
		})
	}
}
func TestPublicationJobLimitsAndExclusivity(t *testing.T) {
	roots, legacy := publicationVectors(t)
	v := roots[0]
	now := time.Now()
	doc := publicationDoc(t, v)
	for _, mutate := range []func(*DocumentRepresentation){
		func(d *DocumentRepresentation) { d.RootManifest = bytes.Repeat([]byte(" "), MaxDocJobManifestBytes+1) },
		func(d *DocumentRepresentation) { d.Chunks = []ContentHash{} }, func(d *DocumentRepresentation) { d.Envelope = json.RawMessage(`null`) }, func(d *DocumentRepresentation) { d.Format = ChunkFormatV2 },
	} {
		bad := doc
		mutate(&bad)
		if _, err := NewDocFinalizationJobForRepresentation(v.Repo, bad, now); err == nil {
			t.Fatal("invalid root metadata")
		}
	}
	ld := publicationDoc(t, legacy[0])
	ld.RootManifest = json.RawMessage(`null`)
	if _, err := NewDocFinalizationJobForRepresentation(v.Repo, ld, now); err == nil {
		t.Fatal("legacy root leak")
	}
	ld.RootManifest = nil
	ld.Chunks = nil
	if _, err := NewDocFinalizationJobForRepresentation(v.Repo, ld, now); err == nil {
		t.Fatal("legacy empty chunks now allowed")
	}
	ld.Chunks = make([]ContentHash, MaxDocJobChunks+1)
	if _, err := NewDocFinalizationJobForRepresentation(v.Repo, ld, now); err == nil {
		t.Fatal("legacy chunk bound")
	}
}
