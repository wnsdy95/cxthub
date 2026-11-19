package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func sessionContentDoc(texts ...string) domain.CIRDocument {
	doc := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV1, SourceProvider: domain.ProviderClaude, SessionOriginID: "session-1", Fidelity: domain.FidelityFull}}
	for index, text := range texts {
		doc.Events = append(doc.Events, domain.Event{Kind: domain.EventMessage, Seq: index, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	return doc
}

// The uploader goes through the document path the server already verifies:
// negotiate, missing chunks only, then the manifest.
func TestSessionContentUploaderSendsADocumentThroughTheChunkPath(t *testing.T) {
	repo := domain.HashContent([]byte("repo"))
	doc := sessionContentDoc("first record", "second record")
	canonical, err := domain.CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.HashContent(canonical)
	serverHas := map[domain.ContentHash][]byte{}
	var manifests []chunkedDocWire
	var requests []string
	client := docUploadClient(t, string(repo), func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		switch {
		case strings.HasSuffix(r.URL.Path, "/negotiate"):
			var req negotiateReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			answer := negotiateResp{ChunksSupported: true, BoundedChunksSupported: true,
				ChunkFormatsSupported: []string{chunkcas.FormatV1, chunkcas.FormatV2}, CIRVersionsSupported: domain.SupportedCIRVersions()}
			if len(manifests) == 0 {
				answer.DocWants = req.DocHaves
			}
			for _, hash := range req.ChunkHaves {
				if _, ok := serverHas[hash]; !ok {
					answer.ChunkWants = append(answer.ChunkWants, hash)
				}
			}
			_ = json.NewEncoder(w).Encode(answer)
		case strings.HasSuffix(r.URL.Path, "/chunks"):
			var req chunksReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			for _, chunk := range req.Chunks {
				if domain.HashContent(chunk.Data) != chunk.Hash {
					t.Fatal("a chunk does not match its hash")
				}
				serverHas[chunk.Hash] = chunk.Data
			}
		case strings.HasSuffix(r.URL.Path, "/objects"):
			var req objectsReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if len(req.Snapshots) != 0 || len(req.ChunkedDocs) != 1 {
				t.Fatalf("published more than one document: %+v", req)
			}
			manifests = append(manifests, req.ChunkedDocs[0])
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	})
	uploader := NewSessionContentUploader(client, func(context.Context) (domain.DocumentIdentity, error) { return domain.DocumentIdentityLegacy, nil })
	ref, err := uploader.PutSessionContent(context.Background(), string(repo), doc)
	if err != nil {
		t.Fatal(err)
	}
	if ref != (domain.DocumentRef{Hash: want}) || len(manifests) != 1 || manifests[0].Hash != want {
		t.Fatalf("ref=%+v manifests=%+v", ref, manifests)
	}
	// What the server received reassembles to the same canonical document.
	var stream bytes.Buffer
	for _, hash := range manifests[0].Chunks {
		stream.Write(serverHas[hash])
	}
	if rebuilt := `{"envelope":` + string(manifests[0].Envelope) + `,"events":[` + stream.String() + `]}`; domain.HashContent([]byte(rebuilt)) != want {
		t.Fatalf("the uploaded chunks do not rebuild the document: %s", rebuilt)
	}
	if strings.Join(requests, " ") != "negotiate chunks objects" {
		t.Fatalf("requests = %v", requests)
	}

	// The same document again: the server has it, nothing is sent.
	requests = nil
	if again, err := uploader.PutSessionContent(context.Background(), string(repo), doc); err != nil || again != ref || strings.Join(requests, " ") != "negotiate" {
		t.Fatalf("again=%+v err=%v requests=%v", again, err, requests)
	}
}

func TestSessionContentUploaderRefusesWhatItCannotUpload(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	requests := 0
	client := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
		requests++
		// A peer without chunk support.
		_ = json.NewEncoder(w).Encode(negotiateResp{DocWants: nil})
	})
	legacy := func(context.Context) (domain.DocumentIdentity, error) { return domain.DocumentIdentityLegacy, nil }
	// A peer that wants the document but takes no chunks: this document is not
	// accepted, and nothing was written.
	old := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
		var req negotiateReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(negotiateResp{DocWants: req.DocHaves})
	})
	if _, err := NewSessionContentUploader(old, legacy).PutSessionContent(context.Background(), repo, sessionContentDoc("one")); !errors.Is(err, domain.ErrSessionContentNotAccepted) {
		t.Fatalf("a peer without chunk support: %v", err)
	}
	if _, err := NewSessionContentUploader(client, legacy).PutSessionContent(context.Background(), repo, sessionContentDoc()); !errors.Is(err, domain.ErrInvalidCIR) || requests != 0 {
		t.Fatalf("a document without events: %v (requests %d)", err, requests)
	}
	failure := errors.New("synthetic failure")
	failing := func(context.Context) (domain.DocumentIdentity, error) { return domain.DocumentIdentityLegacy, failure }
	if _, err := NewSessionContentUploader(client, failing).PutSessionContent(context.Background(), repo, sessionContentDoc("one")); !errors.Is(err, failure) || requests != 0 {
		t.Fatalf("an unknown identity: %v (requests %d)", err, requests)
	}
	unknown := func(context.Context) (domain.DocumentIdentity, error) {
		return domain.DocumentIdentity("another-identity"), nil
	}
	if _, err := NewSessionContentUploader(client, unknown).PutSessionContent(context.Background(), repo, sessionContentDoc("one")); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) || requests != 0 {
		t.Fatalf("an unsupported identity: %v (requests %d)", err, requests)
	}
	if _, err := (*SessionContentUploader)(nil).PutSessionContent(context.Background(), repo, sessionContentDoc("one")); err == nil {
		t.Fatal("an unwired uploader must fail")
	}
	// A root document is built from the same events and carries its manifest.
	root := func(context.Context) (domain.DocumentIdentity, error) { return domain.DocumentIdentityRootV1, nil }
	var offered negotiateReq
	rootClient := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
	rootClient.httpc.Transport = reviewTransferRoundTripper(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+repo:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": repo, "required_doc_identity": domain.DocumentIdentityRootV1,
				"doc_identities_supported": []domain.DocumentIdentity{domain.DocumentIdentityRootV1}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/push/negotiate"):
			_ = json.NewDecoder(r.Body).Decode(&offered)
			// The document is already there: nothing more is asked for.
			_ = json.NewEncoder(w).Encode(negotiateResp{ChunksSupported: true, BoundedChunksSupported: true, AsyncDocsSupported: true, RootPublicationEnabled: true,
				ChunkFormatsSupported: []string{chunkcas.FormatV2}, CIRVersionsSupported: domain.SupportedCIRVersions(), DocIdentitiesSupported: []domain.DocumentIdentity{domain.DocumentIdentityRootV1}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return w.Result(), nil
	})
	ref, err := NewSessionContentUploader(rootClient, root).PutSessionContent(context.Background(), repo, sessionContentDoc("one"))
	if err != nil || ref.Identity != domain.DocumentIdentityRootV1 || ref.Validate() != nil || len(offered.DocHaves) != 1 || offered.DocHaves[0] != ref.Hash {
		t.Fatalf("root upload: ref=%+v err=%v offered=%+v", ref, err, offered)
	}
}
