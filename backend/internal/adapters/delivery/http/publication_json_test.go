package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPublicationContainerCannotEraseIdentity(t *testing.T) {
	for _, field := range []string{"snapshots", "docs", "chunked_docs", "objects"} {
		identity := "identity"
		if field == "snapshots" {
			identity = "doc_identity"
		}
		root := fmt.Sprintf(`[{%q:"cxt-manifest-sha256-v1"}]`, identity)
		if field == "chunked_docs" {
			// A valid strict root union must reach the duplicate-container guard;
			// malformed unions would fail earlier and would not test erasure.
			manifest, _, err := domain.ConversationManifestForCIR(domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}})
			if err != nil {
				t.Fatal(err)
			}
			hash, err := domain.ConversationManifestHash(manifest)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := domain.CanonicalConversationManifest(manifest)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(inbound.ChunkedDoc{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw})
			if err != nil {
				t.Fatal(err)
			}
			root = "[" + string(wire) + "]"
		}
		empty := `[]`
		if field == "objects" {
			root = `{"snapshots":[{"doc_identity":"cxt-manifest-sha256-v1"}]}`
			empty = `{}`
		}
		for _, alias := range []string{field, strings.ToUpper(field), fmt.Sprintf(`\u%04x%s`, field[0], field[1:])} {
			for _, limited := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%v", field, alias, limited), func(t *testing.T) {
					wire := fmt.Sprintf(`{"%s":%s,"%s":%s}`, field, root, alias, empty)
					var dst any = &objectsBody{}
					if field == "objects" {
						dst = &memoryPublicationBody{}
					}
					checkPublicationDecode(t, wire, dst, limited, 409, "unsupported_document_identity")
				})
			}
		}
	}
}

func TestPublicationDecoderPreservesErrorContract(t *testing.T) {
	for _, limited := range []bool{false, true} {
		for _, manifest := range []string{`null`, `{}`} {
			checkPublicationDecode(t, `{"root_manifest":`+manifest+`}`, &inbound.ChunkedDoc{}, limited, 400, "bad_request")
		}
		checkPublicationDecode(t, `{"snapshots":}`, &objectsBody{}, limited, 400, "bad_request")
	}
	checkPublicationDecode(t, `{"docs":`+strings.Repeat(" ", 4096)+`[]}`, &objectsBody{}, true, 413, "payload_too_large")
}

func checkPublicationDecode(t *testing.T, wire string, dst any, limited bool, status int, code string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(wire))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s := &Server{}
	var ok bool
	if limited {
		ok = s.decodeLimited(w, r, dst, 4096)
	} else {
		ok = s.decode(w, r, dst)
	}
	var response struct{ Error struct{ Code string } }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if ok || w.Code != status || response.Error.Code != code {
		t.Fatalf("decode=%v response=%d %s; want %d %s", ok, w.Code, response.Error.Code, status, code)
	}
}

func TestPublicationDecodeReusePreservesLegacyShape(t *testing.T) {
	wire := []byte(`{"snapshots":[{"message":"legacy"}],"docs":[],"chunked_docs":[],"chunk_objects":[]}`)
	var body objectsBody
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatal(err)
	}
	want := body
	for _, raw := range [][]byte{wire, []byte(`{}`), []byte(`null`)} {
		if err := json.Unmarshal(raw, &body); err != nil || !reflect.DeepEqual(body, want) {
			t.Fatalf("reused body: %v %v", body, err)
		}
	}
}
