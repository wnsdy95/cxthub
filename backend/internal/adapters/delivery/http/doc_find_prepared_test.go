package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docFindPreparedHTTPQuery(test *testing.T, repo, hash domain.ContentHash, legacy, continuation bool) url.Values {
	test.Helper()
	query := docFindHTTPInitialQuery(test, repo, hash, "needle")
	if legacy {
		query = docLegacyFindHTTPInitialQuery("needle")
	}
	if continuation {
		progress := &domain.DocFindPreparedProgress{Page: 31, Generation: domain.HashContent([]byte("prepared descriptor"))}
		query = url.Values{"q": {"needle"}}
		if legacy {
			selector := docLegacyFindHTTPTestSelector(repo, hash, "needle")
			selector.Version, selector.Representation, selector.Field, selector.Byte = 3, domain.DocFindPreparedRepresentation, 0, (512<<10)+17
			selector.Prepared = progress
			query.Set("legacy", "1")
			query.Set("cursor", encodeDocLegacyFindHTTPTestSelector(test, selector))
		} else {
			selector := docFindHTTPTestSelector(repo, hash, "needle")
			selector.Version, selector.Representation, selector.Field, selector.Byte = 3, domain.DocFindPreparedRepresentation, 0, (512<<10)+17
			selector.Prepared = progress
			query.Set("cursor", encodeDocFindHTTPTestSelector(test, selector))
		}
	}
	query.Set("representation", domain.DocFindPreparedRepresentation)
	return query
}

func docFindPreparedHTTPObject(test *testing.T, raw []byte, keys ...string) map[string]json.RawMessage {
	test.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		test.Fatal(err)
	}
	if len(object) != len(keys) {
		test.Fatalf("unexpected wire fields: %s", raw)
	}
	for _, key := range keys {
		if _, present := object[key]; !present {
			test.Fatalf("missing %s in %s", key, raw)
		}
	}
	return object
}

func TestDocFindPreparedHTTPWireContract(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, legacy := range []bool{false, true} {
		for _, continuation := range []bool{false, true} {
			for _, terminal := range []bool{false, true} {
				test.Run(fmt.Sprintf("legacy=%t/continuation=%t/terminal=%t", legacy, continuation, terminal), func(test *testing.T) {
					query := docFindPreparedHTTPQuery(test, repo, hash, legacy, continuation)
					page := domain.DocFindPage{
						Projection: domain.DocFindPreparedProjection, Hash: hash, Generation: docVisibleTestSelector(repo, hash, "units").Generation,
						From: 0, To: 12, Index: 4, Byte: (512 << 10) + 23,
						Prepared: &domain.DocFindPreparedProgress{Page: 31, Generation: domain.HashContent([]byte("prepared descriptor"))},
						Matches:  []domain.DocFindMatch{},
					}
					if legacy {
						page.Projection, page.Generation = domain.DocLegacyFindPreparedProjection, ""
						if continuation {
							page.From = 3
						}
					}
					for _, preparedPage := range []int{0, 31} {
						match := domain.DocFindMatch{PreparedPage: &preparedPage, Index: 4, Offset: (512 << 10) + 17, Length: 6,
							Preview: "needle", PreviewOffset: (512 << 10) + 17, SourceRange: &domain.DocFindSpan{From: 2 << 20, To: (2 << 20) + 6}}
						if preparedPage == 0 {
							match.Offset, match.PreviewOffset = 0, 0
						}
						if !legacy {
							selection := docVisibleTestSelector(repo, hash, "units")
							selection.View, selection.Start, selection.Next, selection.Unit, selection.Limit = "all", 4, 4, 4, 1
							match.RevealCursor = encodeDocVisibleTestSelector(test, selection)
						}
						page.Matches = append(page.Matches, match)
					}
					cursor := docFindPreparedHTTPMutatedQuery(test, docFindPreparedHTTPQuery(test, repo, hash, legacy, true), func(selector map[string]any) {
						selector["from"] = page.From
						positionKey := "byte"
						if legacy {
							positionKey = "byte_offset"
						}
						selector[positionKey] = page.Byte
					}).Get("cursor")
					page.NextCursor = &cursor
					if terminal {
						page.Index, page.Byte, page.Prepared, page.NextCursor = page.To, 0, &domain.DocFindPreparedProgress{}, nil
					}
					server, backend, port := docFindHTTPTestServer()
					port.find = func(context.Context) (domain.DocFindPage, error) { return page, nil }
					response := httptest.NewRecorder()
					docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(response, docFindHTTPTestRequest(repo, hash, query.Encode()))
					if response.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash ||
						port.request.Representation != domain.DocFindPreparedRepresentation || port.request.Legacy != legacy ||
						port.request.Cursor != query.Get("cursor") || port.request.Selection != query.Get("selection") || port.request.Query != "needle" ||
						response.Header().Get("Content-Type") != "application/json; charset=utf-8" || !strings.Contains(response.Header().Get("Vary"), "Accept") {
						test.Fatal("prepared request lost transport bindings", response.Code, port.request, response.Body.String())
					}
					var decoded domain.DocFindPage
					if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded, page) {
						test.Fatal("prepared wire lost positions, generation or source ranges without Accept", decoded, err)
					}
					fields := docFindPreparedHTTPObject(test, response.Body.Bytes(), "prepared", "projection", "hash", "generation", "from", "to", "event_index", "field", "byte_offset", "matches", "next_cursor")
					progress := docFindPreparedHTTPObject(test, fields["prepared"], "page", "generation")
					if terminal && (string(progress["page"]) != "0" || string(progress["generation"]) != `""` || string(fields["byte_offset"]) != "0" || string(fields["next_cursor"]) != "null") {
						test.Fatal("terminal prepared state not reset", response.Body.String())
					}
					var matches []json.RawMessage
					if err := json.Unmarshal(fields["matches"], &matches); err != nil || len(matches) != 2 {
						test.Fatal("missing prepared matches", err)
					}
					for index, raw := range matches {
						match := docFindPreparedHTTPObject(test, raw, "prepared_page", "event_index", "field", "byte_offset", "byte_length", "raw", "preview", "preview_byte_offset", "reveal_cursor", "source_range")
						if string(match["field"]) != "0" || string(match["raw"]) != "false" || index == 0 && string(match["prepared_page"]) != "0" {
							test.Fatal("required zero/false fields omitted or changed", string(raw))
						}
						docFindPreparedHTTPObject(test, match["source_range"], "from", "to")
					}
					assertDocFindHTTPNoLegacyReads(test, backend)
				})
			}
		}
	}
}

func docFindPreparedHTTPMutatedQuery(test *testing.T, query url.Values, mutate func(map[string]any)) url.Values {
	test.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(query.Get("cursor"))
	if err != nil {
		test.Fatal(err)
	}
	var selector map[string]any
	if err := json.Unmarshal(raw, &selector); err != nil {
		test.Fatal(err)
	}
	mutate(selector)
	raw, err = json.Marshal(selector)
	if err != nil {
		test.Fatal(err)
	}
	copy, err := url.ParseQuery(query.Encode())
	if err != nil {
		test.Fatal(err)
	}
	copy.Set("cursor", base64.RawURLEncoding.EncodeToString(raw))
	return copy
}

func TestDocFindPreparedHTTPProgressBounds(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, legacy := range []bool{false, true} {
		for _, fixture := range []struct {
			page, position int
			generation     domain.ContentHash
			terminal       bool
		}{
			{0, 0, "", false}, {0, 512 << 10, "", false},
			{0, (512 << 10) + 1, domain.HashContent([]byte("descriptor")), false},
			{domain.MaxDocRangePages - 1, domain.MaxConversationDocumentBytes, domain.HashContent([]byte("descriptor")), false},
			{0, 0, "", true},
		} {
			query := docFindPreparedHTTPMutatedQuery(test, docFindPreparedHTTPQuery(test, repo, hash, legacy, true), func(selector map[string]any) {
				selector["prepared"] = map[string]any{"page": fixture.page, "generation": fixture.generation}
				positionKey, indexKey := "byte", "index"
				if legacy {
					positionKey, indexKey = "byte_offset", "event_index"
				}
				selector[positionKey] = fixture.position
				if fixture.terminal {
					selector[indexKey] = selector["to"]
				}
			})
			if request, err := docFindPageRequest(query, repo, hash); err != nil || request.Cursor != query.Get("cursor") || request.Legacy != legacy {
				test.Fatal("valid prepared boundary rejected", legacy, fixture, request, err)
			}
		}
	}
}

func TestDocFindPreparedHTTPRejectsMixedProfilesAndMalformedState(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, legacy := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy=%t", legacy), func(test *testing.T) {
			initial := docFindPreparedHTTPQuery(test, repo, hash, legacy, false)
			continuation := docFindPreparedHTTPQuery(test, repo, hash, legacy, true)
			queries := docFindHTTPInvalidRepresentationQueries(test, initial, continuation)
			positionKey, indexKey := "byte", "index"
			if legacy {
				positionKey, indexKey = "byte_offset", "event_index"
			}
			mutations := []func(map[string]any){
				func(selector map[string]any) { delete(selector, "prepared") },
				func(selector map[string]any) { selector["prepared"] = nil },
				func(selector map[string]any) { selector["prepared"] = map[string]any{} },
				func(selector map[string]any) { selector["field"] = 1 },
				func(selector map[string]any) { selector[positionKey] = -1 },
				func(selector map[string]any) { selector[positionKey] = domain.MaxConversationDocumentBytes + 1 },
				func(selector map[string]any) { selector["version"] = 2 },
				func(selector map[string]any) { selector["representation"] = "display-v2" },
				func(selector map[string]any) { selector["unknown"] = 0 },
				func(selector map[string]any) { selector["query_hash"] = domain.HashContent([]byte("other query")) },
				func(selector map[string]any) { selector[indexKey], selector[positionKey] = selector["to"], 0 },
				func(selector map[string]any) {
					selector[indexKey], selector[positionKey] = selector["to"], 0
					selector["prepared"].(map[string]any)["page"] = 0
				},
			}
			for _, progress := range []map[string]any{
				{"page": 0}, {"generation": ""}, {"page": -1, "generation": ""},
				{"page": domain.MaxDocRangePages, "generation": domain.HashContent([]byte("descriptor"))},
				{"page": 1, "generation": ""}, {"page": 1, "generation": "invalid"},
				{"page": 0, "generation": ""}, {"page": 1.5, "generation": ""},
				{"page": 0, "generation": "", "unknown": 0},
			} {
				mutations = append(mutations, func(selector map[string]any) { selector["prepared"] = progress })
			}
			for _, mutate := range mutations {
				queries = append(queries, docFindPreparedHTTPMutatedQuery(test, continuation, mutate).Encode())
			}
			for _, version := range []int{1, 2} {
				old := docFindPreparedHTTPMutatedQuery(test, continuation, func(selector map[string]any) {
					selector["version"], selector[positionKey] = version, 0
					if version == 1 {
						delete(selector, "representation")
					} else {
						selector["representation"] = domain.DocFindDisplayRepresentation
					}
				})
				old.Del("representation")
				if version == 2 {
					old.Set("representation", domain.DocFindDisplayRepresentation)
				}
				queries = append(queries, old.Encode())
				old = docFindPreparedHTTPMutatedQuery(test, old, func(selector map[string]any) { delete(selector, "prepared") })
				old.Set("representation", domain.DocFindPreparedRepresentation)
				queries = append(queries, old.Encode())
			}
			for _, representation := range []string{"", domain.DocFindDisplayRepresentation} {
				copy, _ := url.ParseQuery(continuation.Encode())
				copy.Del("representation")
				if representation != "" {
					copy.Set("representation", representation)
				}
				queries = append(queries, copy.Encode())
			}
			crossMode := docFindPreparedHTTPQuery(test, repo, hash, !legacy, true)
			crossMode.Set("cursor", continuation.Get("cursor"))
			queries = append(queries, crossMode.Encode(), initial.Encode()+"&prepared=0", continuation.Encode()+"&prepared_page=0")
			raw, err := base64.RawURLEncoding.DecodeString(continuation.Get("cursor"))
			if err != nil {
				test.Fatal(err)
			}
			for _, malformed := range []string{"!", continuation.Get("cursor") + "=", base64.RawURLEncoding.EncodeToString(append(raw, ' ')),
				strings.Repeat("A", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes)+1),
				base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"prepared":`, `"prepared":null,"prepared":`, 1))),
				base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"page":31`, `"page":31,"page":31`, 1))),
				base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `"version":3`, `"version":3,"version":3`, 1)))} {
				copy, _ := url.ParseQuery(continuation.Encode())
				copy.Set("cursor", malformed)
				queries = append(queries, copy.Encode())
			}
			assertDocFindHTTPRepresentationQueriesRejected(test, repo, hash, queries)
		})
	}
}

func TestDocFindPreparedHTTPPendingAndUnavailable(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, legacy := range []bool{false, true} {
		for _, continuation := range []bool{false, true} {
			for _, fixture := range []struct {
				err         error
				code, retry string
			}{
				{domain.ErrDocDisplayPending, "doc_display_pending", "1"},
				{domain.ErrDocFindDisplayUnavailable, "doc_find_display_unavailable", ""},
			} {
				server, backend, port := docFindHTTPTestServer()
				port.find = func(context.Context) (domain.DocFindPage, error) {
					return domain.DocFindPage{}, fmt.Errorf("prepared find: %w", fixture.err)
				}
				response := httptest.NewRecorder()
				query := docFindPreparedHTTPQuery(test, repo, hash, legacy, continuation)
				docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(response, docFindHTTPTestRequest(repo, hash, query.Encode()))
				if response.Code != http.StatusServiceUnavailable || port.calls != 1 || response.Header().Get("Retry-After") != fixture.retry ||
					!strings.Contains(response.Body.String(), `"code":"`+fixture.code+`"`) || strings.Contains(response.Body.String(), `"matches"`) {
					test.Fatal("prepared error changed retry policy or leaked partial results", legacy, continuation, fixture, response.Code, response.Header(), response.Body.String())
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			}
		}
	}
}
