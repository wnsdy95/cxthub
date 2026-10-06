package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func docUploadFixture(t *testing.T, version string) outbound.DocumentChunks {
	t.Helper()
	raw, err := domain.CanonicalBytes(domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: version},
		Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user",
			Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic upload fixture"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := chunkcas.PlanDoc(raw)
	if !ok {
		t.Fatal("fixture did not produce a manifest")
	}
	return outbound.DocumentChunks{Hash: domain.HashContent(raw), Format: plan.Manifest.Format,
		Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks,
		ReadChunk: func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			body, ok := plan.Bodies[hash]
			if !ok {
				return nil, domain.ErrNotFound
			}
			return body, nil
		}}
}

// Small arbitrary byte-stream partitions exercise the count limit separately
// from the raw-byte limit. Each descriptor still reconstructs canonical CIR.
func docUploadPartitionFixture(t *testing.T, count, bodySize int) (outbound.DocumentChunks, [][]byte) {
	t.Helper()
	doc := docUploadFixture(t, domain.CIRVersionV1)
	var bodies [][]byte
	var raw bytes.Buffer
	raw.WriteString(`{"envelope":`)
	raw.Write(doc.Envelope)
	raw.WriteString(`,"events":[`)
	doc.Chunks = nil
	byHash := map[domain.ContentHash][]byte{}
	for i := 0; i < count; i++ {
		body := []byte(fmt.Sprintf(`{"blocks":[{"text":"%s","type":"text"}],"kind":"message","role":"user","seq":%d}`, strings.Repeat("x", bodySize), i))
		if i > 0 {
			body = append([]byte(","), body...)
		}
		hash := domain.HashContent(body)
		doc.Chunks = append(doc.Chunks, hash)
		byHash[hash] = body
		bodies = append(bodies, body)
		raw.Write(body)
	}
	raw.WriteString(`]}`)
	doc.Hash = domain.HashContent(raw.Bytes())
	var cir domain.CIRDocument
	if err := json.Unmarshal(raw.Bytes(), &cir); err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateSessionDocHash(domain.SessionDoc{Hash: doc.Hash, CIR: cir}); err != nil {
		t.Fatalf("fixture must preserve canonical identity: %v", err)
	}
	doc.ReadChunk = func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		body, ok := byHash[hash]
		if !ok {
			return nil, domain.ErrNotFound
		}
		return body, nil
	}
	return doc, bodies
}

func docUploadCapabilities(doc outbound.DocumentChunks) negotiateResp {
	return negotiateResp{DocWants: []domain.ContentHash{doc.Hash}, ChunkWants: doc.Chunks,
		ChunksSupported: true, BoundedChunksSupported: true,
		ChunkFormatsSupported: []string{chunkcas.FormatV1, chunkcas.FormatV2}, CIRVersionsSupported: domain.SupportedCIRVersions()}
}

// Execute handlers synchronously for deterministic loader/HTTP ordering and
// cancellation at precise boundaries, using the client's real JSON transport.
func docUploadClient(t *testing.T, repo string, handle http.HandlerFunc) *BackendClient {
	t.Helper()
	c := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
	c.httpc.Transport = reviewTransferRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost && !(r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/push/doc-jobs/")) {
			t.Fatalf("unexpected method %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.URL.Path, "/repos/"+repo+"/push/") {
			t.Fatalf("unexpected resource %s", r.URL.Path)
		}
		w := httptest.NewRecorder()
		handle(w, r)
		return w.Result(), nil
	})
	return c
}

func TestPushDocChunksValidatesDescriptorBeforeNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*outbound.DocumentChunks, *string)
		want error
	}{
		{"repo", func(_ *outbound.DocumentChunks, r *string) { *r = "invalid" }, domain.ErrHashMismatch},
		{"document hash", func(d *outbound.DocumentChunks, _ *string) { d.Hash = "invalid" }, domain.ErrHashMismatch},
		{"format", func(d *outbound.DocumentChunks, _ *string) { d.Format = "future" }, domain.ErrHashMismatch},
		{"empty chunks", func(d *outbound.DocumentChunks, _ *string) { d.Chunks = nil }, domain.ErrHashMismatch},
		{"chunk hash", func(d *outbound.DocumentChunks, _ *string) { d.Chunks[0] = "invalid" }, domain.ErrHashMismatch},
		{"nil loader", func(d *outbound.DocumentChunks, _ *string) { d.ReadChunk = nil }, domain.ErrHashMismatch},
		{"missing envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = nil }, domain.ErrInvalidCIR},
		{"empty envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`{}`) }, domain.ErrInvalidCIR},
		{"incomplete envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`{"cir_version":"1"}`) }, domain.ErrInvalidCIR},
		{"noncanonical envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = append([]byte(" "), d.Envelope...) }, domain.ErrInvalidCIR},
		{"null envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`null`) }, domain.ErrInvalidCIR},
		{"array envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`[]`) }, domain.ErrInvalidCIR},
		{"string envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`"oops"`) }, domain.ErrInvalidCIR},
		{"malformed envelope", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`{`) }, domain.ErrInvalidCIR},
		{"malformed version", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`{"cir_version":2}`) }, domain.ErrInvalidCIR},
		{"unknown version", func(d *outbound.DocumentChunks, _ *string) { d.Envelope = json.RawMessage(`{"cir_version":"999"}`) }, domain.ErrUnsupportedCIRVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := docUploadFixture(t, domain.CIRVersionV1)
			repo := domain.HashContent([]byte("repo"))
			doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
				t.Fatal("invalid descriptor read a chunk")
				return nil, nil
			}
			tc.edit(&doc, &repo)
			c := docUploadClient(t, repo, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid descriptor contacted server") })
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			if !ok || !errors.Is(err, tc.want) {
				t.Fatalf("handled=%v error=%v, want %v without fallback", ok, err, tc.want)
			}
		})
	}
}

func TestPushDocChunksCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		edit    func(*negotiateResp)
		wantOK  bool
		wantErr error
	}{
		{"modern v1", "1", func(*negotiateResp) {}, true, nil},
		{"modern v2", "2", func(*negotiateResp) {}, true, nil},
		{"empty version means v1", "", func(n *negotiateResp) { n.CIRVersionsSupported = nil }, true, nil},
		{"missing CIR capability means v1", "1", func(n *negotiateResp) { n.CIRVersionsSupported = nil }, true, nil},
		{"v2 requires CIR capability", "2", func(n *negotiateResp) { n.CIRVersionsSupported = nil }, true, domain.ErrUnsupportedCIRVersion},
		{"v2 rejected by v1 peer", "2", func(n *negotiateResp) { n.CIRVersionsSupported = []string{"1"} }, true, domain.ErrUnsupportedCIRVersion},
		{"v1 rejected by explicit v2 peer", "1", func(n *negotiateResp) { n.CIRVersionsSupported = []string{"2"} }, true, domain.ErrUnsupportedCIRVersion},
		{"no chunks", "1", func(n *negotiateResp) { n.ChunksSupported = false }, false, nil},
		{"no bounded chunks", "1", func(n *negotiateResp) { n.BoundedChunksSupported = false }, false, nil},
		{"no formats", "1", func(n *negotiateResp) { n.ChunkFormatsSupported = nil }, false, nil},
		{"v1 format only", "1", func(n *negotiateResp) { n.ChunkFormatsSupported = []string{chunkcas.FormatV1} }, false, nil},
		{"old peer", "1", func(n *negotiateResp) { *n = negotiateResp{DocWants: n.DocWants} }, false, nil},
		{"old peer rejects CIR v2 before fallback", "2", func(n *negotiateResp) { *n = negotiateResp{DocWants: n.DocWants} }, true, domain.ErrUnsupportedCIRVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := docUploadFixture(t, tc.version)
			doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
				t.Fatal("empty wants read a chunk")
				return nil, nil
			}
			repo := domain.HashContent([]byte("repo"))
			neg := docUploadCapabilities(doc)
			neg.ChunkWants = nil
			tc.edit(&neg)
			negotiations, commits := 0, 0
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					negotiations++
					_ = json.NewEncoder(w).Encode(neg)
				case strings.HasSuffix(r.URL.Path, "/objects"):
					commits++
				default:
					t.Fatalf("unexpected write %s", r.URL.Path)
				}
			})
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			wantCommits := 0
			if tc.wantOK && tc.wantErr == nil {
				wantCommits = 1
			}
			if ok != tc.wantOK || !errors.Is(err, tc.wantErr) || negotiations != 1 || commits != wantCommits {
				t.Fatalf("handled=%v error=%v negotiations=%d commits=%d", ok, err, negotiations, commits)
			}
		})
	}
}

func TestPushDocChunksManifestLimitsFallBackBeforeRequests(t *testing.T) {
	for _, kind := range []string{"ordered-count", "encoded-bytes"} {
		t.Run(kind, func(t *testing.T) {
			doc := docUploadFixture(t, domain.CIRVersionV1)
			if kind == "ordered-count" {
				id := doc.Chunks[0]
				for len(doc.Chunks) <= chunkcas.MaxPortableManifestChunks {
					doc.Chunks = append(doc.Chunks, id)
				}
			} else {
				raw, err := domain.CanonicalBytes(domain.CIRDocument{Envelope: domain.Envelope{
					CIRVersion: domain.CIRVersionV1, SessionOriginID: strings.Repeat("x", chunkcas.MaxPortableManifestBytes),
				}})
				if err != nil {
					t.Fatal(err)
				}
				var value struct {
					Envelope json.RawMessage `json:"envelope"`
				}
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				doc.Envelope = value.Envelope
			}
			doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
				t.Error("nonportable manifest read an upload body")
				return nil, errors.New("unexpected body read")
			}
			repo := domain.HashContent([]byte(t.Name()))
			client := docUploadClient(t, repo, func(http.ResponseWriter, *http.Request) {
				t.Error("nonportable manifest contacted server")
			})
			if used, err := client.PushDocChunks(context.Background(), repo, doc); used || err != nil {
				t.Fatalf("clean pre-write fallback: used=%v error=%v", used, err)
			}
		})
	}
}

func TestPushDocChunksRejectsMaliciousNegotiationBeforeReadsOrFallback(t *testing.T) {
	other := domain.HashContent([]byte("unoffered"))
	for _, tc := range []struct {
		name string
		edit func(*negotiateResp)
	}{
		{"snapshot", func(n *negotiateResp) { n.SnapshotWants = []domain.ContentHash{other} }},
		{"unoffered document", func(n *negotiateResp) { n.DocWants = []domain.ContentHash{other} }},
		{"duplicate document", func(n *negotiateResp) { n.DocWants = append(n.DocWants, n.DocWants[0]) }},
		{"invalid document", func(n *negotiateResp) { n.DocWants = []domain.ContentHash{"invalid"} }},
		{"unoffered chunk", func(n *negotiateResp) { n.ChunkWants = []domain.ContentHash{other} }},
		{"duplicate chunk", func(n *negotiateResp) { n.ChunkWants = append(n.ChunkWants, n.ChunkWants[0]) }},
		{"invalid chunk", func(n *negotiateResp) { n.ChunkWants = []domain.ContentHash{"invalid"} }},
		{"unoffered chunk for existing document", func(n *negotiateResp) { n.DocWants = nil; n.ChunkWants = []domain.ContentHash{other} }},
	} {
		for _, supported := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/supported=%v", tc.name, supported), func(t *testing.T) {
				doc := docUploadFixture(t, "1")
				doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
					t.Fatal("malicious wants drove a local read")
					return nil, nil
				}
				repo := domain.HashContent([]byte("repo"))
				neg := docUploadCapabilities(doc)
				neg.BoundedChunksSupported = supported
				tc.edit(&neg)
				calls := 0
				c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
					calls++
					if !strings.HasSuffix(r.URL.Path, "/negotiate") {
						t.Fatalf("malicious wants caused a write: %s", r.URL.Path)
					}
					_ = json.NewEncoder(w).Encode(neg)
				})
				ok, err := c.PushDocChunks(context.Background(), repo, doc)
				if !ok || !errors.Is(err, domain.ErrHashMismatch) || calls != 1 {
					t.Fatalf("handled=%v error=%v calls=%d", ok, err, calls)
				}
			})
		}
	}
}

func TestPushDocChunksMissingOnlyAndEmptyWants(t *testing.T) {
	for _, mode := range []string{"missing only", "all chunks present", "document present", "document present on old peer"} {
		t.Run(mode, func(t *testing.T) {
			doc, _ := docUploadPartitionFixture(t, 4, 10)
			doc.Chunks = append(doc.Chunks, doc.Chunks[1]) // Repeated IDs must survive publication.
			repo := domain.HashContent([]byte("repo"))
			neg := docUploadCapabilities(doc)
			neg.ChunkWants = []domain.ContentHash{doc.Chunks[3], doc.Chunks[1]}
			wantReads := []domain.ContentHash{doc.Chunks[1], doc.Chunks[3]}
			wantCommit := 1
			if mode != "missing only" {
				wantReads = nil
				if mode == "all chunks present" {
					neg.ChunkWants = nil
				} else {
					neg.DocWants = nil
					wantCommit = 0
					if mode == "document present on old peer" {
						neg = negotiateResp{}
					}
				}
			}
			var reads, sent []domain.ContentHash
			read := doc.ReadChunk
			doc.ReadChunk = func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
				reads = append(reads, hash)
				return read(ctx, hash)
			}
			commits := 0
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					var req negotiateReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					if len(req.SnapshotHaves) != 0 || !reflect.DeepEqual(req.DocHaves, []domain.ContentHash{doc.Hash}) || !reflect.DeepEqual(req.ChunkHaves, doc.Chunks[:4]) {
						t.Fatalf("incorrect/duplicate offers %+v", req)
					}
					_ = json.NewEncoder(w).Encode(neg)
				case strings.HasSuffix(r.URL.Path, "/chunks"):
					var req chunksReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					for _, chunk := range req.Chunks {
						if domain.HashContent(chunk.Data) != chunk.Hash {
							t.Fatal("uploaded corrupt body")
						}
						sent = append(sent, chunk.Hash)
					}
				case strings.HasSuffix(r.URL.Path, "/objects"):
					commits++
					var req objectsReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					want := objectsReq{ChunkedDocs: []chunkedDocWire{{Hash: doc.Hash, Format: doc.Format, Envelope: doc.Envelope, Chunks: doc.Chunks}}}
					if !reflect.DeepEqual(req, want) || !reflect.DeepEqual(sent, wantReads) {
						t.Fatalf("manifest changed or published early: %+v", req)
					}
				default:
					t.Fatalf("unexpected write %s", r.URL.Path)
				}
			})
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			if !ok || err != nil || !reflect.DeepEqual(reads, wantReads) || !reflect.DeepEqual(sent, wantReads) || commits != wantCommit {
				t.Fatalf("handled=%v error=%v reads=%v sent=%v commits=%d", ok, err, reads, sent, commits)
			}
		})
	}
}

func TestPushDocChunksStreamsBoundedBatchesBeforeReadingWholeDocument(t *testing.T) {
	for _, tc := range []struct {
		name           string
		count, size    int
		firstBatchSize int
	}{
		{"object count", 2*maxChunkWireObjects + 3, 8, maxChunkWireObjects},
		{"raw bytes", 11, chunkcas.ChunkTarget, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, _ := docUploadPartitionFixture(t, tc.count, tc.size)
			repo := domain.HashContent([]byte("repo"))
			read := doc.ReadChunk
			reads, uploaded, batches, commits := 0, 0, 0, 0
			doc.ReadChunk = func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
				reads++
				if reads-uploaded > tc.firstBatchSize+1 {
					t.Fatal("read beyond bounded batch and one lookahead")
				}
				return read(ctx, hash)
			}
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					neg := docUploadCapabilities(doc)
					// Upload order belongs to the descriptor, not server response order.
					neg.ChunkWants = append([]domain.ContentHash(nil), neg.ChunkWants...)
					for i, j := 0, len(neg.ChunkWants)-1; i < j; i, j = i+1, j-1 {
						neg.ChunkWants[i], neg.ChunkWants[j] = neg.ChunkWants[j], neg.ChunkWants[i]
					}
					_ = json.NewEncoder(w).Encode(neg)
				case strings.HasSuffix(r.URL.Path, "/chunks"):
					batches++
					var req chunksReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					total := 0
					for _, chunk := range req.Chunks {
						if chunk.Hash != doc.Chunks[uploaded] || domain.HashContent(chunk.Data) != chunk.Hash {
							t.Fatal("chunk order/identity changed")
						}
						uploaded++
						total += len(chunk.Data)
					}
					if total > maxChunkWireRawBytes || len(req.Chunks) > maxChunkWireObjects || len(req.Chunks) == 0 {
						t.Fatalf("unbounded batch: raw=%d objects=%d", total, len(req.Chunks))
					}
					if batches == 1 && (len(req.Chunks) != tc.firstBatchSize || reads >= tc.count) {
						t.Fatalf("first upload: chunks=%d reads=%d", len(req.Chunks), reads)
					}
				case strings.HasSuffix(r.URL.Path, "/objects"):
					commits++
					if uploaded != tc.count {
						t.Fatal("manifest published before all requested chunks")
					}
				default:
					t.Fatalf("unexpected request %s", r.URL.Path)
				}
			})
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			if !ok || err != nil || uploaded != tc.count || reads != tc.count || batches < 3 || commits != 1 {
				t.Fatalf("handled=%v error=%v uploaded=%d reads=%d batches=%d commits=%d", ok, err, uploaded, reads, batches, commits)
			}
		})
	}
}

func TestPushDocChunksLoaderFailuresNeverFallbackOrPublish(t *testing.T) {
	loaderErr := errors.New("synthetic read failure")
	for _, afterUpload := range []bool{false, true} {
		for _, failure := range []string{"loader error", "tampered", "empty", "oversized"} {
			t.Run(fmt.Sprintf("%s/partial=%v", failure, afterUpload), func(t *testing.T) {
				doc, _ := docUploadPartitionFixture(t, maxChunkWireObjects+5, 8)
				repo := domain.HashContent([]byte("repo"))
				failAt := 0
				wantBatches := 0
				if afterUpload {
					failAt = maxChunkWireObjects + 1
					wantBatches = 1
				}
				var badBody []byte
				if failure == "oversized" {
					badBody = bytes.Repeat([]byte{'x'}, maxChunkWireRawBytes+1)
				}
				if failure == "empty" || failure == "oversized" {
					doc.Chunks[failAt] = domain.HashContent(badBody)
				}
				read := doc.ReadChunk
				reads, batches := 0, 0
				doc.ReadChunk = func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
					reads++
					if hash == doc.Chunks[failAt] {
						switch failure {
						case "loader error":
							return nil, loaderErr
						case "tampered":
							return []byte("changed after verification"), nil
						default:
							return badBody, nil
						}
					}
					return read(ctx, hash)
				}
				c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/negotiate"):
						_ = json.NewEncoder(w).Encode(docUploadCapabilities(doc))
					case strings.HasSuffix(r.URL.Path, "/chunks"):
						batches++
					default:
						t.Fatalf("loader failure published or retried: %s", r.URL.Path)
					}
				})
				ok, err := c.PushDocChunks(context.Background(), repo, doc)
				if !ok || err == nil || reads != failAt+1 || batches != wantBatches {
					t.Fatalf("handled=%v error=%v reads=%d batches=%d", ok, err, reads, batches)
				}
				if failure == "loader error" && !errors.Is(err, loaderErr) || failure == "tampered" && !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("lost error identity: %v", err)
				}
			})
		}
	}
}

func TestPushDocChunksHTTPFailuresNeverFallback(t *testing.T) {
	for _, endpoint := range []string{"negotiate", "chunks", "objects", "doc-jobs"} {
		for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("%s/%d", endpoint, status), func(t *testing.T) {
				doc := docUploadFixture(t, "1")
				repo := domain.HashContent([]byte("repo"))
				failed, reads := false, 0
				read := doc.ReadChunk
				doc.ReadChunk = func(ctx context.Context, hash domain.ContentHash) ([]byte, error) { reads++; return read(ctx, hash) }
				c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
					if failed {
						t.Fatalf("request after failure: %s", r.URL.Path)
					}
					if strings.HasSuffix(r.URL.Path, "/"+endpoint) {
						failed = true
						http.Error(w, "synthetic rejection", status)
						return
					}
					switch {
					case strings.HasSuffix(r.URL.Path, "/negotiate"):
						neg := docUploadCapabilities(doc)
						neg.AsyncDocsSupported = endpoint == "doc-jobs"
						_ = json.NewEncoder(w).Encode(neg)
					case strings.HasSuffix(r.URL.Path, "/chunks"):
					default:
						t.Fatalf("unexpected request %s", r.URL.Path)
					}
				})
				ok, err := c.PushDocChunks(context.Background(), repo, doc)
				var httpErr *HTTPError
				if !ok || !failed || !errors.As(err, &httpErr) || httpErr.Status != status || endpoint == "negotiate" && reads != 0 {
					t.Fatalf("handled=%v error=%v failed=%v reads=%d", ok, err, failed, reads)
				}
			})
		}
	}
}

func TestPushDocChunksCancellationNeverFallback(t *testing.T) {
	for _, stage := range []string{"before negotiation", "after negotiation", "unsupported peer", "loader", "after partial upload", "accepted job"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			doc, _ := docUploadPartitionFixture(t, maxChunkWireObjects+5, 8)
			repo := domain.HashContent([]byte("repo"))
			read := doc.ReadChunk
			reads, writes := 0, 0
			doc.ReadChunk = func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
				if ctx.Err() != nil {
					t.Fatal("read after cancellation")
				}
				reads++
				if stage == "loader" {
					cancel() // Even a loader ignoring cancellation must not cause a write.
				}
				return read(ctx, hash)
			}
			if stage == "before negotiation" {
				cancel()
			}
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				if ctx.Err() != nil {
					t.Fatalf("HTTP after cancellation: %s", r.URL.Path)
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					neg := docUploadCapabilities(doc)
					if stage == "after negotiation" || stage == "unsupported peer" {
						cancel()
						neg.BoundedChunksSupported = stage != "unsupported peer"
					}
					if stage == "accepted job" {
						neg.AsyncDocsSupported = true
						neg.ChunkWants = nil
					}
					_ = json.NewEncoder(w).Encode(neg)
				case strings.HasSuffix(r.URL.Path, "/chunks"):
					writes++
					if stage != "after partial upload" {
						t.Fatalf("unexpected chunk write at %s", stage)
					}
					cancel()
				case strings.HasSuffix(r.URL.Path, "/doc-jobs"):
					writes++
					_ = json.NewEncoder(w).Encode(docJobStatus{ID: domain.HashContent([]byte("job")), DocHash: doc.Hash, State: "running"})
					cancel()
				default:
					t.Fatalf("unexpected write %s", r.URL.Path)
				}
			})
			ok, err := c.PushDocChunks(ctx, repo, doc)
			wantReads, wantWrites := 0, 0
			switch stage {
			case "loader":
				wantReads = 1
			case "after partial upload":
				wantReads, wantWrites = maxChunkWireObjects+1, 1
			case "accepted job":
				wantWrites = 1
			}
			if !ok || !errors.Is(err, context.Canceled) || reads != wantReads || writes != wantWrites {
				t.Fatalf("handled=%v error=%v reads=%d writes=%d", ok, err, reads, writes)
			}
		})
	}
}

func TestPushDocChunksAsyncReceiptValidation(t *testing.T) {
	for _, mode := range []string{"completed", "poll completed", "rejected", "wrong document", "bad job ID", "unknown state", "poll changed ID", "poll rejected", "malformed acknowledgement"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			doc := docUploadFixture(t, "2")
			repo := domain.HashContent([]byte("repo"))
			id := domain.HashContent([]byte("job"))
			polls, chunks, jobs := 0, 0, 0
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					neg := docUploadCapabilities(doc)
					neg.AsyncDocsSupported = true
					_ = json.NewEncoder(w).Encode(neg)
				case strings.HasSuffix(r.URL.Path, "/chunks"):
					chunks++
				case strings.HasSuffix(r.URL.Path, "/doc-jobs"), strings.HasSuffix(r.URL.Path, "/doc-jobs/"+id):
					if chunks != 1 {
						t.Fatal("job before chunks")
					}
					job := docJobStatus{ID: id, DocHash: doc.Hash, State: "completed"}
					if r.Method == http.MethodGet {
						polls++
						if jobs != 1 {
							t.Fatal("poll before acceptance")
						}
						if mode == "poll changed ID" {
							job.ID = domain.HashContent([]byte("other job"))
						} else if mode == "poll rejected" {
							job.State = "rejected"
						}
					} else {
						jobs++
						var manifest chunkedDocWire
						if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
							t.Fatal(err)
						}
						want := chunkedDocWire{Hash: doc.Hash, Format: doc.Format, Envelope: doc.Envelope, Chunks: doc.Chunks}
						if !reflect.DeepEqual(manifest, want) {
							t.Fatalf("manifest changed: %+v", manifest)
						}
						w.WriteHeader(http.StatusAccepted)
						switch mode {
						case "poll completed", "poll changed ID", "poll rejected":
							job.State = "waiting"
						case "rejected":
							job.State, job.Reason = "rejected", "invalid_document"
						case "wrong document":
							job.DocHash = domain.HashContent([]byte("other document"))
						case "bad job ID":
							job.ID = "bad"
						case "unknown state":
							job.State = "unknown"
						case "malformed acknowledgement":
							_, _ = io.WriteString(w, `{"id":`)
							return
						}
					}
					_ = json.NewEncoder(w).Encode(job)
				default:
					t.Fatalf("async path published objects/refs: %s", r.URL.Path)
				}
			})
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			wantErr := mode != "completed" && mode != "poll completed"
			wantPolls := 0
			if strings.HasPrefix(mode, "poll ") {
				wantPolls = 1
			}
			if !ok || (err != nil) != wantErr || polls != wantPolls || jobs != 1 {
				t.Fatalf("handled=%v error=%v polls=%d jobs=%d", ok, err, polls, jobs)
			}
		})
	}
}

func TestPushDocChunksSyncAcknowledgementUsesExistingHeaderSemantics(t *testing.T) {
	doc := docUploadFixture(t, "1")
	repo := domain.HashContent([]byte("repo"))
	c := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
	body := &ackResponseBody{ReadCloser: io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))}
	writes := 0
	c.httpc.Transport = reviewTransferRoundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/negotiate") {
			neg := docUploadCapabilities(doc)
			neg.ChunkWants = nil
			return reviewWireResponse(r, http.StatusOK, neg), nil
		}
		if !strings.HasSuffix(r.URL.Path, "/objects") {
			t.Fatalf("unexpected write %s", r.URL.Path)
		}
		writes++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
		t.Fatal("empty wants read a chunk")
		return nil, nil
	}
	ok, err := c.PushDocChunks(context.Background(), repo, doc)
	if !ok || err != nil || writes != 1 || body.closes != 1 || !errors.Is(body.readErr, io.ErrUnexpectedEOF) {
		t.Fatalf("handled=%v error=%v writes=%d body=%+v", ok, err, writes, body)
	}
}

func TestPushDocChunksTransportFailuresAfterStagingNeverFallback(t *testing.T) {
	transportErr := errors.New("synthetic lost acknowledgement")
	for _, endpoint := range []string{"negotiate", "chunks", "objects", "doc-jobs"} {
		t.Run(endpoint, func(t *testing.T) {
			doc, _ := docUploadPartitionFixture(t, maxChunkWireObjects+5, 8)
			repo := domain.HashContent([]byte("repo"))
			batches, failed := 0, false
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					neg := docUploadCapabilities(doc)
					neg.AsyncDocsSupported = endpoint == "doc-jobs"
					_ = json.NewEncoder(w).Encode(neg)
				case strings.HasSuffix(r.URL.Path, "/chunks"):
					batches++
				default:
					t.Fatalf("unexpected request %s", r.URL.Path)
				}
			})
			transport := c.httpc.Transport
			c.httpc.Transport = reviewTransferRoundTripper(func(r *http.Request) (*http.Response, error) {
				if failed {
					t.Fatalf("retry after ambiguous write: %s", r.URL.Path)
				}
				if strings.HasSuffix(r.URL.Path, "/"+endpoint) && (endpoint != "chunks" || batches == 1) {
					failed = true
					return nil, transportErr
				}
				return transport.RoundTrip(r)
			})
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			wantBatches := 2
			if endpoint == "negotiate" {
				wantBatches = 0
			} else if endpoint == "chunks" {
				wantBatches = 1
			}
			if !ok || !failed || !errors.Is(err, transportErr) || batches != wantBatches {
				t.Fatalf("handled=%v failed=%v error=%v staged batches=%d", ok, failed, err, batches)
			}
		})
	}
}

func TestPushDocChunksMalformedNegotiationNeverFallsBack(t *testing.T) {
	for _, reply := range []string{`{"chunks_supported":`, `{"doc_wants":true}`, `{"bounded_chunks_supported":"yes"}`} {
		t.Run(reply, func(t *testing.T) {
			doc := docUploadFixture(t, "1")
			repo := domain.HashContent([]byte("repo"))
			calls := 0
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if !strings.HasSuffix(r.URL.Path, "/negotiate") {
					t.Fatalf("malformed negotiation caused write %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, reply)
			})
			doc.ReadChunk = func(context.Context, domain.ContentHash) ([]byte, error) {
				t.Fatal("malformed negotiation caused read")
				return nil, nil
			}
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			if !ok || err == nil || calls != 1 {
				t.Fatalf("handled=%v error=%v calls=%d", ok, err, calls)
			}
		})
	}
}

func TestPushDocChunksLegacyManifestEncoding(t *testing.T) {
	for _, format := range []string{"", chunkcas.FormatV1} {
		t.Run(fmt.Sprintf("format=%q", format), func(t *testing.T) {
			// A single event has the same bytes in v1 and v2.
			doc := docUploadFixture(t, "1")
			doc.Format = format
			repo := domain.HashContent([]byte("repo"))
			commits := 0
			c := docUploadClient(t, repo, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					neg := docUploadCapabilities(doc)
					neg.ChunkFormatsSupported = []string{chunkcas.FormatV1}
					neg.ChunkWants = nil
					_ = json.NewEncoder(w).Encode(neg)
				case strings.HasSuffix(r.URL.Path, "/objects"):
					commits++
					var req objectsReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Fatal(err)
					}
					if len(req.ChunkedDocs) != 1 || req.ChunkedDocs[0].Format != "" || req.ChunkedDocs[0].Hash != doc.Hash || !reflect.DeepEqual(req.ChunkedDocs[0].Chunks, doc.Chunks) {
						t.Fatalf("changed legacy manifest: %+v", req)
					}
				default:
					t.Fatalf("unexpected write %s", r.URL.Path)
				}
			})
			ok, err := c.PushDocChunks(context.Background(), repo, doc)
			if !ok || err != nil || commits != 1 {
				t.Fatalf("handled=%v error=%v commits=%d", ok, err, commits)
			}
		})
	}
}
