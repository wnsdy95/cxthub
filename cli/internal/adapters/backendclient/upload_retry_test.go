package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

func uploadOverloadResponse(request *http.Request, hints ...string) *http.Response {
	response := reviewWireResponse(request, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "resource_overloaded", "message": "busy"}})
	if hints != nil {
		response.Header["Retry-After"] = hints
	}
	return response
}

func uploadRetryFixture(t *testing.T, kind string, exchange func(*http.Request) (*http.Response, error)) (*BackendClient, func(context.Context) error) {
	t.Helper()
	repo := string(domain.HashContent([]byte("upload retry repo")))
	client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
	client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/push/negotiate") {
			var offered negotiateReq
			if err := json.NewDecoder(request.Body).Decode(&offered); err != nil {
				t.Fatal(err)
			}
			return reviewWireResponse(request, http.StatusOK, negotiateResp{SnapshotWants: offered.SnapshotHaves}), nil
		}
		if request.Method != http.MethodPost || request.URL.Path != client.reposPath(repo)+"/push/"+kind {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		return exchange(request)
	})
	return client, func(ctx context.Context) error {
		body := []byte("frozen chunk")
		if kind == "chunks" {
			return client.pushChunkBatches(ctx, repo, []chunkObjWire{{Hash: domain.HashContent(body), Data: body}})
		}
		id := domain.HashContent(body)
		return client.Push(ctx, repo, []domain.Snapshot{{ID: id, DocHash: id, RepoID: repo, Branch: "main"}}, nil, nil, false, false)
	}
}

func TestUploadBackpressureRetriesIdenticalBatches(t *testing.T) {
	for _, kind := range []string{"chunks", "objects"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var bodies [][]byte
				var deadlines []time.Time
				client, upload := uploadRetryFixture(t, kind, func(request *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						t.Fatal(err)
					}
					bodies = append(bodies, body)
					deadline, ok := request.Context().Deadline()
					if !ok {
						t.Error("batch has no absolute deadline")
					}
					deadlines = append(deadlines, deadline)
					if request.URL.Host != "synthetic.invalid" || request.Header.Get("Authorization") != "Bearer frozen-token" {
						t.Fatal("retry changed destination or credentials")
					}
					if len(bodies) == 1 {
						return uploadOverloadResponse(request, "1"), nil
					}
					return reviewWireResponse(request, http.StatusOK, nil), nil
				})
				resolved := 0
				client.resolveTransport = func() (string, string, error) {
					resolved++
					if resolved > 1 && kind == "chunks" || resolved > 2 && kind == "objects" {
						return "https://replacement.invalid", "replacement-token", nil
					}
					return "https://synthetic.invalid", "frozen-token", nil
				}
				if err := upload(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) || !deadlines[0].Equal(deadlines[1]) {
					t.Fatalf("batch changed: bodies=%q deadlines=%v", bodies, deadlines)
				}
			})
		})
	}
}

func TestUploadBackpressureStreamsWithoutRereading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		doc, _ := docUploadPartitionFixture(t, 2*maxChunkWireObjects+3, 8)
		repo := string(domain.HashContent([]byte("stream retry")))
		read := doc.ReadChunk
		reads, accepted := 0, 0
		doc.ReadChunk = func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
			reads++
			if reads-accepted > maxChunkWireObjects+1 {
				t.Fatal("read beyond one batch and lookahead")
			}
			return read(ctx, hash)
		}
		var chunkBodies, objectBodies [][]byte
		client := docUploadClient(t, repo, func(writer http.ResponseWriter, request *http.Request) {
			switch {
			case strings.HasSuffix(request.URL.Path, "/negotiate"):
				_ = json.NewEncoder(writer).Encode(docUploadCapabilities(doc))
			case strings.HasSuffix(request.URL.Path, "/chunks"):
				body, _ := io.ReadAll(request.Body)
				chunkBodies = append(chunkBodies, body)
				if len(chunkBodies) == 2 {
					writer.Header().Set("Retry-After", "1")
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(writer, `{"error":{"code":"resource_overloaded"}}`)
					return
				}
				var batch chunksReq
				if err := json.Unmarshal(body, &batch); err != nil {
					t.Fatal(err)
				}
				for _, chunk := range batch.Chunks {
					if chunk.Hash != doc.Representation.Chunks[accepted] || domain.HashContent(chunk.Data) != chunk.Hash {
						t.Fatal("replayed accepted chunk or changed content")
					}
					accepted++
				}
			case strings.HasSuffix(request.URL.Path, "/objects"):
				if accepted != len(doc.Representation.Chunks) {
					t.Fatal("manifest preceded chunks")
				}
				body, _ := io.ReadAll(request.Body)
				objectBodies = append(objectBodies, body)
				if len(objectBodies) == 1 {
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(writer, `{"error":{"code":"resource_overloaded"}}`)
				}
			default:
				t.Fatal("unexpected publication", request.URL.Path)
			}
		})
		handled, err := client.PushDocChunks(context.Background(), repo, doc)
		if err != nil || !handled || reads != accepted || len(chunkBodies) != 4 || len(objectBodies) != 2 {
			t.Fatalf("handled=%v err=%v reads=%d accepted=%d chunk calls=%d object calls=%d", handled, err, reads, accepted, len(chunkBodies), len(objectBodies))
		}
		if !bytes.Equal(chunkBodies[1], chunkBodies[2]) || !bytes.Equal(objectBodies[0], objectBodies[1]) {
			t.Fatal("retry changed frozen bytes")
		}
	})
}

func TestUploadBackpressureFreezesPayload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		data := []byte("original bytes")
		chunks := []chunkObjWire{{Hash: domain.HashContent(data), Data: data}}
		var bodies [][]byte
		client, _ := uploadRetryFixture(t, "chunks", func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			bodies = append(bodies, body)
			if len(bodies) == 1 {
				data[0] = 'X'
				return uploadOverloadResponse(request), nil
			}
			return reviewWireResponse(request, 200, nil), nil
		})
		if err := client.pushChunkBatches(context.Background(), string(domain.HashContent([]byte("upload retry repo"))), chunks); err != nil {
			t.Fatal(err)
		}
		if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
			t.Fatalf("payload was reread between attempts: %q", bodies)
		}
	})
}

func TestUploadBackpressureExhaustion(t *testing.T) {
	for _, kind := range []string{"chunks", "objects"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts []time.Time
				_, upload := uploadRetryFixture(t, kind, func(request *http.Request) (*http.Response, error) {
					attempts = append(attempts, time.Now())
					return uploadOverloadResponse(request, "1"), nil
				})
				var rejected *HTTPError
				if err := upload(context.Background()); !errors.As(err, &rejected) || rejected.Status != 503 || rejected.Code != "resource_overloaded" {
					t.Fatalf("lost final rejection: %v", err)
				}
				if len(attempts) != 5 {
					t.Fatalf("attempts=%d, want 5", len(attempts))
				}
				for index := 1; index < len(attempts); index++ {
					delay := attempts[index].Sub(attempts[index-1])
					backoff := time.Second << (index - 1)
					if delay <= backoff || delay > backoff+250*time.Millisecond {
						t.Fatalf("retry %d delay=%s", index, delay)
					}
				}
			})
		})
	}
}

func TestUploadBackpressureAbsoluteBudget(t *testing.T) {
	for _, test := range []struct {
		name                                 string
		clientTimeout, callerTimeout, budget time.Duration
	}{
		{"default", 30 * time.Second, time.Minute, 30 * time.Second},
		{"caller", 30 * time.Second, 5 * time.Second, 5 * time.Second},
		{"shorter client", 5 * time.Second, time.Minute, 5 * time.Second},
		{"longer client", time.Minute, time.Minute, 30 * time.Second},
		{"unlimited client", 0, time.Minute, 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				calls := 0
				client, upload := uploadRetryFixture(t, "chunks", func(request *http.Request) (*http.Response, error) {
					calls++
					deadline, ok := request.Context().Deadline()
					if !ok || !deadline.Equal(started.Add(test.budget)) {
						t.Fatalf("deadline=%v, want %v", deadline, started.Add(test.budget))
					}
					if calls == 1 {
						return uploadOverloadResponse(request), nil
					}
					<-request.Context().Done()
					return nil, request.Context().Err()
				})
				client.httpc.Timeout = test.clientTimeout
				ctx, cancel := context.WithTimeout(context.Background(), test.callerTimeout)
				defer cancel()
				if err := upload(ctx); !errors.Is(err, context.DeadlineExceeded) || calls != 2 || time.Since(started) != test.budget {
					t.Fatalf("err=%v calls=%d elapsed=%s", err, calls, time.Since(started))
				}
			})
		})
	}
}

func TestUploadBackpressureCancellationAndInsufficientBudget(t *testing.T) {
	for _, mode := range []string{"already canceled", "backoff canceled", "insufficient budget"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "already canceled" {
					cancel()
				} else if mode == "backoff canceled" {
					time.AfterFunc(250*time.Millisecond, cancel)
				} else {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 500*time.Millisecond)
					defer stop()
				}
				calls := 0
				_, upload := uploadRetryFixture(t, "chunks", func(request *http.Request) (*http.Response, error) {
					calls++
					return uploadOverloadResponse(request), nil
				})
				started := time.Now()
				err := upload(ctx)
				if mode == "insufficient budget" {
					var rejected *HTTPError
					if !errors.As(err, &rejected) || calls != 1 || time.Since(started) != 0 {
						t.Fatalf("budget overrun: err=%v calls=%d elapsed=%s", err, calls, time.Since(started))
					}
				} else if !errors.Is(err, context.Canceled) || calls > 1 || mode == "already canceled" && calls != 0 {
					t.Fatalf("cancellation: err=%v calls=%d", err, calls)
				}
			})
		})
	}
}

func TestUploadBackpressureRetryAfter(t *testing.T) {
	for _, mode := range []string{"absent", "zero", "seconds", "maximum", "date", "past date"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var hints []string
				minimum := time.Second
				switch mode {
				case "zero":
					hints = []string{"0"}
				case "seconds":
					hints, minimum = []string{"3"}, 3*time.Second
				case "maximum":
					hints, minimum = []string{"10"}, 10*time.Second
				case "date":
					hints, minimum = []string{time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)}, 5*time.Second
				case "past date":
					hints = []string{time.Now().Add(-time.Second).UTC().Format(http.TimeFormat)}
				}
				calls := 0
				_, upload := uploadRetryFixture(t, "chunks", func(request *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return uploadOverloadResponse(request, hints...), nil
					}
					return reviewWireResponse(request, http.StatusOK, nil), nil
				})
				started := time.Now()
				if err := upload(context.Background()); err != nil || calls != 2 || time.Since(started) <= minimum || time.Since(started) > minimum+250*time.Millisecond {
					t.Fatalf("err=%v calls=%d delay=%s", err, calls, time.Since(started))
				}
			})
		})
	}
}

func TestUploadBackpressureRejectsOtherFailures(t *testing.T) {
	for _, mode := range []string{"401", "403", "404", "409", "413", "422", "429", "500", "502", "504", "other code", "missing code", "malformed envelope", "broken body", "network", "timeout", "empty hint", "negative hint", "signed hint", "fractional hint", "unknown hint", "overflow hint", "oversized hint", "large hint", "large date", "duplicate hint", "joined hint"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				cause := errors.New("ambiguous network failure")
				_, upload := uploadRetryFixture(t, "chunks", func(request *http.Request) (*http.Response, error) {
					calls++
					response := uploadOverloadResponse(request, "1")
					switch mode {
					case "other code", "missing code":
						code := ""
						if mode == "other code" {
							code = "service_unavailable"
						}
						response = reviewWireResponse(request, 503, map[string]any{"error": map[string]string{"code": code}})
					case "malformed envelope":
						response.Body = io.NopCloser(strings.NewReader(`{"error":`))
					case "broken body":
						response.Body = io.NopCloser(io.MultiReader(response.Body, iotest.ErrReader(io.ErrUnexpectedEOF)))
					case "network":
						return nil, cause
					case "timeout":
						return nil, context.DeadlineExceeded
					case "empty hint":
						response.Header.Set("Retry-After", "")
					case "negative hint":
						response.Header.Set("Retry-After", "-1")
					case "signed hint":
						response.Header.Set("Retry-After", "+1")
					case "fractional hint":
						response.Header.Set("Retry-After", "1.5")
					case "unknown hint":
						response.Header.Set("Retry-After", "later")
					case "overflow hint":
						response.Header.Set("Retry-After", "18446744073709551616")
					case "oversized hint":
						response.Header.Set("Retry-After", strings.Repeat("0", 1024))
					case "large hint":
						response.Header.Set("Retry-After", "11")
					case "large date":
						response.Header.Set("Retry-After", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
					case "duplicate hint":
						response.Header.Add("Retry-After", "1")
					case "joined hint":
						response.Header.Set("Retry-After", "1, 1")
					default:
						if err := json.Unmarshal([]byte(mode), &response.StatusCode); err != nil {
							t.Fatal(err)
						}
					}
					return response, nil
				})
				started := time.Now()
				err := upload(context.Background())
				if err == nil || calls != 1 || time.Since(started) != 0 || mode == "network" && !errors.Is(err, cause) || mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("retried or masked failure: err=%v calls=%d elapsed=%s", err, calls, time.Since(started))
				}
			})
		})
	}
}

func TestUploadBackpressureDoesNotRetryMutations(t *testing.T) {
	for _, path := range []string{"/refs/batch", "/refs/branch/main", "/history", "/history/memory-selection", "/prs/promote", "/prs/promotions", "/push/doc-jobs", "/memory-attachments/snapshot", "/snapshots/snapshot/graft"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := string(domain.HashContent([]byte("mutation exclusion")))
				ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: domain.HashContent([]byte("target"))}
				event := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, BranchID: "identity", Branch: "main", Kind: "position", CreatedAt: time.Unix(1, 0).UTC(), BindingParent: strings.Repeat("2", 32)}
				pr := domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
				client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
				calls := 0
				client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/refs") {
						return reviewWireResponse(request, 200, []domain.Ref{}), nil
					}
					if request.URL.Path != client.reposPath(repo)+path {
						t.Fatal("unexpected mutation", request.URL.Path)
					}
					calls++
					return uploadOverloadResponse(request, "1"), nil
				})
				started := time.Now()
				ctx := context.Background()
				var err error
				switch path {
				case "/refs/batch":
					err = client.Push(ctx, repo, nil, nil, []domain.Ref{ref}, false, false)
				case "/refs/branch/main":
					err = client.UpdateRefRemote(ctx, repo, ref, false)
				case "/history", "/history/memory-selection":
					if strings.HasSuffix(path, "/memory-selection") {
						event.MemorySelectionParent = strings.Repeat("3", 32)
						event.Source, event.Target = ref.Target, ref.Target
						event.MemoryPinned, event.MemoryHash = true, domain.HashContent([]byte("memory"))
						event.GitAfter, event.WorktreeID = strings.Repeat("d", 40), strings.Repeat("e", 32)
					}
					err = client.PushHistoryEvent(ctx, event)
				case "/prs/promote":
					_, err = client.PromotePullRequest(ctx, repo, pr)
				case "/prs/promotions":
					err = client.SubmitPRPromotion(ctx, repo, pr)
				default:
					err = client.do(ctx, http.MethodPost, client.reposPath(repo)+path, map[string]string{"target": "unchanged"}, nil)
				}
				if err == nil || calls != 1 || time.Since(started) != 0 || errors.Is(err, domain.ErrSyncConflict) {
					t.Fatalf("mutation retried: err=%v calls=%d", err, calls)
				}
			})
		})
	}
}

type uploadBlockedACK struct{ context context.Context }

func (body uploadBlockedACK) Read([]byte) (int, error) {
	<-body.context.Done()
	return 0, body.context.Err()
}

func TestUploadBackpressurePreservesACKSemantics(t *testing.T) {
	for _, kind := range []string{"chunks", "objects"} {
		for _, mode := range []string{"broken", "stalled", "caller canceled"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					calls := 0
					var body *ackResponseBody
					_, upload := uploadRetryFixture(t, kind, func(request *http.Request) (*http.Response, error) {
						calls++
						var reader io.Reader = iotest.ErrReader(io.ErrUnexpectedEOF)
						if mode != "broken" {
							reader = uploadBlockedACK{context: request.Context()}
						}
						body = &ackResponseBody{ReadCloser: io.NopCloser(reader)}
						if mode == "caller canceled" {
							body.onFirstRead = cancel
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
					})
					if err := upload(ctx); err != nil || calls != 1 || body.closes != 1 {
						t.Fatalf("acknowledged upload retried: err=%v calls=%d body=%+v", err, calls, body)
					}
				})
			})
		}
	}
}

func TestUploadBackpressureObjectsBeforeRefs(t *testing.T) {
	for _, exhaust := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovers", true: "exhausts"}[exhaust], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := string(domain.HashContent([]byte("object batch retry")))
				first, second := domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))
				snapshots := []domain.Snapshot{{ID: first, DocHash: first, RepoID: repo, Branch: "main"}, {ID: second, DocHash: second, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{first}}}
				ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: second}
				counts := map[domain.ContentHash]int{}
				accepted := map[domain.ContentHash]bool{}
				bodies := map[domain.ContentHash][]byte{}
				refs := 0
				client := NewBackendClient(func() string { return "https://synthetic.invalid" }, func() string { return "" }, domain.TeamIdentity{})
				client.httpc.Transport = reviewTransferRoundTripper(func(request *http.Request) (*http.Response, error) {
					switch {
					case strings.HasSuffix(request.URL.Path, "/push/negotiate"):
						return reviewWireResponse(request, 200, negotiateResp{SnapshotWants: []domain.ContentHash{first, second}}), nil
					case strings.HasSuffix(request.URL.Path, "/push/objects"):
						body, _ := io.ReadAll(request.Body)
						var batch objectsReq
						if err := json.Unmarshal(body, &batch); err != nil || len(batch.Snapshots) != 1 {
							t.Fatalf("invalid batch: %s %v", body, err)
						}
						id := batch.Snapshots[0].ID
						counts[id]++
						if previous, ok := bodies[id]; ok && !bytes.Equal(previous, body) {
							t.Fatal("object retry changed bytes")
						}
						bodies[id] = body
						if id == second && !accepted[first] {
							t.Fatal("child preceded parent")
						}
						if id == second && (exhaust || counts[id] == 1) {
							return uploadOverloadResponse(request), nil
						}
						accepted[id] = true
					case strings.HasSuffix(request.URL.Path, "/refs"):
						return reviewWireResponse(request, 200, []domain.Ref{}), nil
					case strings.HasSuffix(request.URL.Path, "/refs/batch"):
						refs++
						if !accepted[first] || !accepted[second] {
							t.Fatal("ref preceded objects")
						}
					default:
						t.Fatal("unexpected request", request.URL.Path)
					}
					return reviewWireResponse(request, 200, nil), nil
				})
				collector := outbound.NewSyncDiagnostics(64)
				ctx := outbound.WithSyncDiagnostics(context.Background(), collector)
				err := client.Push(ctx, repo, snapshots, nil, []domain.Ref{ref}, false, false)
				wantSecond, wantRefs := 2, 1
				if exhaust {
					wantSecond, wantRefs = 5, 0
				}
				if (err != nil) != exhaust || !reflect.DeepEqual(counts, map[domain.ContentHash]int{first: 1, second: wantSecond}) || refs != wantRefs {
					t.Fatalf("err=%v counts=%v refs=%d", err, counts, refs)
				}
				if !collector.HasFailures() {
					t.Fatal("overload attempts disappeared from diagnostics")
				}
			})
		})
	}
}
