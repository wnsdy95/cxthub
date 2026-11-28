package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

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
