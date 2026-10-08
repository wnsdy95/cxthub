package domain

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
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
func TestPublicationStatusBinding(t *testing.T) {
	roots, legacy := publicationVectors(t)
	v := roots[0]
	doc := publicationDoc(t, v)
	good := DocFinalizationStatus{ID: v.ID, DocHash: doc.Hash, DocIdentity: doc.Identity, State: "completed", UpdatedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
	if err := good.ValidateFor(v.Repo, doc); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*DocFinalizationStatus){
		"identity-stripped": func(s *DocFinalizationStatus) { s.DocIdentity = "" },
		"identity-unknown":  func(s *DocFinalizationStatus) { s.DocIdentity = "future" },
		"wrong-hash":        func(s *DocFinalizationStatus) { s.DocHash = HashContent([]byte("wrong")) },
		"opaque-id":         func(s *DocFinalizationStatus) { s.ID = string(HashContent([]byte("opaque"))) },
		"unknown-state":     func(s *DocFinalizationStatus) { s.State = "success" },
	} {
		t.Run(name, func(t *testing.T) {
			s := good
			mutate(&s)
			if s.ValidateFor(v.Repo, doc) == nil {
				t.Fatal("receipt accepted")
			}
		})
	}
	if good.ValidateFor(HashContent([]byte("other")), doc) == nil {
		t.Fatal("repo not bound")
	}
	wire, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var got DocFinalizationStatus
	if err = json.Unmarshal(wire, &got); err != nil || got != good {
		t.Fatal("status round trip", err)
	}
	for _, raw := range [][]byte{
		bytes.Replace(wire, []byte(`"doc_identity":`), []byte(`"Doc_Identity":"","doc_identity":`), 1),
		bytes.Replace(wire, []byte(`"doc_identity":"cxt-manifest-sha256-v1"`), []byte(`"doc_identity":null`), 1),
	} {
		before := got
		if json.Unmarshal(raw, &got) == nil || got != before {
			t.Fatal("identity ambiguity or partial decode")
		}
	}
	ld := publicationDoc(t, legacy[0])
	old := DocFinalizationStatus{ID: string(HashContent([]byte("opaque-old-server-id"))), DocHash: ld.Hash, State: "waiting", UpdatedAt: good.UpdatedAt}
	if err = old.ValidateFor(legacy[0].Repo, ld); err != nil {
		t.Fatal("legacy first receipt compatibility", err)
	}
	oldwire, _ := json.Marshal(old)
	want := `{"id":"` + old.ID + `","doc_hash":"` + string(old.DocHash) + `","state":"waiting","updated_at":"2026-10-07T00:00:00Z"}`
	if string(oldwire) != want {
		t.Fatalf("legacy status bytes: %s", oldwire)
	}
	if good.ValidateFor(legacy[0].Repo, ld) == nil {
		t.Fatal("root status satisfied legacy upload")
	}
}
