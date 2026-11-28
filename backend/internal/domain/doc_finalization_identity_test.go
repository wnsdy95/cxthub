package domain

import (
	"encoding/json"
	"os"
	"testing"
)

type publicationVector struct {
	Name     string      `json:"name"`
	Repo     ContentHash `json:"repo_id"`
	Document string      `json:"document_json"`
	ID       string      `json:"job_id"`
	Preimage string      `json:"preimage"`
}

func publicationVectors(t *testing.T) ([]publicationVector, []publicationVector) {
	t.Helper()
	raw, err := os.ReadFile("testdata/doc_finalization_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var all struct{ Root, Legacy []publicationVector }
	if err = json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	return all.Root, all.Legacy
}
func publicationDoc(t *testing.T, v publicationVector) DocumentRepresentation {
	t.Helper()
	var doc DocumentRepresentation
	if err := json.Unmarshal([]byte(v.Document), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
func TestPublicationRootJobIdentityVectors(t *testing.T) {
	roots, legacy := publicationVectors(t)
	for _, v := range roots {
		t.Run(v.Name, func(t *testing.T) {
			doc := publicationDoc(t, v)
			id, err := RootDocFinalizationID(v.Repo, doc)
			if err != nil || id != v.ID {
				t.Fatalf("ID=%s want=%s err=%v", id, v.ID, err)
			}
			if _, err := RootDocFinalizationID("", doc); err == nil {
				t.Fatal("invalid repo")
			}
			bad := doc
			bad.Hash = HashContent([]byte("bad"))
			if _, err := RootDocFinalizationID(v.Repo, bad); err == nil {
				t.Fatal("wrong root")
			}
			bad = doc
			bad.Identity = ""
			if _, err := RootDocFinalizationID(v.Repo, bad); err == nil {
				t.Fatal("root relabeled")
			}
		})
	}
	for _, v := range legacy {
		if _, err := RootDocFinalizationID(v.Repo, publicationDoc(t, v)); err == nil {
			t.Fatal("legacy accepted as root")
		}
	}
}
