package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type docFindHTTPPort struct {
	calls   int
	repo    domain.ContentHash
	hash    domain.ContentHash
	request domain.DocFindPageRequest
	find    func(context.Context) (domain.DocFindPage, error)
}

func (port *docFindHTTPPort) FindDocPage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindPageRequest) (domain.DocFindPage, error) {
	port.calls++
	port.repo, port.hash, port.request = repo, hash, request
	if port.find != nil {
		return port.find(ctx)
	}
	return domain.DocFindPage{Projection: domain.DocFindProjection, Hash: hash, From: request.From, To: request.To, Matches: []domain.DocFindMatch{}}, nil
}

func docFindHTTPTestServer() (*Server, *docVisibleHTTPBackend, *docFindHTTPPort) {
	backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}
	port := &docFindHTTPPort{}
	server := &Server{b: backend}
	server.SetDocumentFind(port)
	return server, backend, port
}

func docFindHTTPTestRouter(handler http.HandlerFunc) http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /api/v1/repos/{repoID}/docs/{hash}/find", handler)
	return router
}

func docFindHTTPTestRequest(repo, hash domain.ContentHash, query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/docs/"+string(hash)+"/find?"+query, nil)
}

func docFindHTTPInitialQuery(test *testing.T, repo, hash domain.ContentHash, query string) url.Values {
	test.Helper()
	selection := encodeDocVisibleTestSelector(test, docVisibleTestSelector(repo, hash, "units"))
	return url.Values{"q": {query}, "selection": {selection}, "from": {"0"}, "to": {"12"}}
}

func docFindHTTPTestSelector(repo, hash domain.ContentHash, query string) domain.DocFindSelector {
	return domain.DocFindSelector{
		Version: 1, Selection: docVisibleTestSelector(repo, hash, "units"),
		QueryHash: domain.HashContent([]byte(query)), From: 0, To: 12, Index: 4, Field: 1, Byte: 2,
	}
}

func encodeDocFindHTTPTestSelector(test *testing.T, selector domain.DocFindSelector) string {
	test.Helper()
	encoded, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return encoded
}

func assertDocFindHTTPNoLegacyReads(test *testing.T, backend *docVisibleHTTPBackend) {
	test.Helper()
	if backend.calls != 0 || backend.eventCalls != 0 || backend.viewCalls != 0 {
		test.Fatalf("find reached document readers: visible=%d events=%d view=%d", backend.calls, backend.eventCalls, backend.viewCalls)
	}
}

func TestDocFindHTTPParserContract(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, text := range []string{"x", " ", "a+b&c", "한글🙂", strings.Repeat("한", domain.MaxDocFindQueryRunes)} {
		test.Run(text, func(test *testing.T) {
			initial := docFindHTTPInitialQuery(test, repo, hash, text)
			for _, bounds := range [][2]int{{0, 12}, {0, 0}, {4, 7}, {12, 12}} {
				initial.Set("from", strconv.Itoa(bounds[0]))
				initial.Set("to", strconv.Itoa(bounds[1]))
				server, backend, port := docFindHTTPTestServer()
				writer := httptest.NewRecorder()
				docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, initial.Encode()))
				want := domain.DocFindPageRequest{Query: text, Selection: initial.Get("selection"), From: bounds[0], To: bounds[1]}
				if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request != want {
					test.Fatalf("status=%d calls=%d request=%+v want=%+v body=%s", writer.Code, port.calls, port.request, want, writer.Body)
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			}
			cursor := encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, text))
			server, backend, port := docFindHTTPTestServer()
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, url.Values{"q": {text}, "cursor": {cursor}}.Encode()))
			if want := (domain.DocFindPageRequest{Query: text, Cursor: cursor}); writer.Code != http.StatusOK || port.calls != 1 || port.request != want || port.repo != repo || port.hash != hash {
				test.Fatalf("continuation changed: status=%d request=%+v body=%s", writer.Code, port.request, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		})
	}
	for _, terminal := range []bool{false, true} {
		selection := docVisibleTestSelector(repo, hash, "units")
		selection.Start = 3
		if terminal {
			selection.Start, selection.Total, selection.Next, selection.Unit = 0, 0, 0, 0
		}
		values := url.Values{"q": {"x"}, "selection": {encodeDocVisibleTestSelector(test, selection)}, "from": {strconv.Itoa(selection.Start)}, "to": {strconv.Itoa(selection.Total)}}
		server, backend, port := docFindHTTPTestServer()
		writer := httptest.NewRecorder()
		docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, values.Encode()))
		if writer.Code != http.StatusOK || port.calls != 1 || port.request.From != selection.Start || port.request.To != selection.Total {
			test.Fatalf("selection boundary rejected: status=%d request=%+v body=%s", writer.Code, port.request, writer.Body)
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
	}
}

func TestDocFindHTTPSourceRangesAreExplicitAndDoNotMutateSharedMatches(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	const supported = "application/json, application/vnd.cxthub.find-source-ranges-v1+json"
	for _, legacy := range []bool{false, true} {
		for _, representation := range []string{"", domain.DocFindDisplayRepresentation, domain.DocFindPreparedRepresentation} {
			for _, continuation := range []bool{false, true} {
				for _, headers := range [][]string{nil, {supported}, {"application/json"}, {"*/*"}, {""}, {supported, supported}} {
					server, backend, port := docFindHTTPTestServer()
					matches := []domain.DocFindMatch{{SourceRange: &domain.DocFindSpan{From: 9000, To: 9010}}}
					projection := domain.DocFindProjection
					if legacy {
						projection = domain.DocLegacyFindProjection
					}
					if representation != "" {
						projection = domain.DocFindDisplayProjection
						if legacy {
							projection = domain.DocLegacyFindDisplayProjection
						}
					}
					if representation == domain.DocFindPreparedRepresentation {
						projection = domain.DocFindPreparedProjection
						if legacy {
							projection = domain.DocLegacyFindPreparedProjection
						}
					}
					page := domain.DocFindPage{Projection: projection, Hash: hash, Matches: matches}
					port.find = func(context.Context) (domain.DocFindPage, error) { return page, nil }
					query := docFindHTTPInitialQuery(test, repo, hash, "needle")
					if legacy {
						query = docLegacyFindHTTPInitialQuery("needle")
					}
					if continuation {
						query = url.Values{"q": {"needle"}}
						selector := docFindHTTPTestSelector(repo, hash, "needle")
						selector.Representation = representation
						if representation != "" {
							selector.Version, selector.Field = 2, 0
						}
						if representation == domain.DocFindPreparedRepresentation {
							selector.Version, selector.Prepared = 3, &domain.DocFindPreparedProgress{}
						}
						query.Set("cursor", encodeDocFindHTTPTestSelector(test, selector))
						if legacy {
							selector := docLegacyFindHTTPTestSelector(repo, hash, "needle")
							selector.Representation = representation
							if representation != "" {
								selector.Version, selector.Field = 2, 0
							}
							if representation == domain.DocFindPreparedRepresentation {
								selector.Version, selector.Prepared = 3, &domain.DocFindPreparedProgress{}
							}
							query.Set("legacy", "1")
							query.Set("cursor", encodeDocLegacyFindHTTPTestSelector(test, selector))
						}
					}
					if representation != "" {
						query.Set("representation", representation)
					}
					request := docFindHTTPTestRequest(repo, hash, query.Encode())
					for _, value := range headers {
						request.Header.Add("Accept", value)
					}
					response := httptest.NewRecorder()
					docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(response, request)
					wantRanges := representation != "" || len(headers) == 1 && headers[0] == supported
					if !wantRanges {
						page.Matches = []domain.DocFindMatch{{}}
					}
					want, err := json.Marshal(page)
					if err != nil {
						test.Fatal(err)
					}
					if response.Code != http.StatusOK || port.calls != 1 || strings.TrimSpace(response.Body.String()) != string(want) ||
						strings.Contains(response.Body.String(), `"source_range"`) != wantRanges || matches[0].SourceRange == nil || !strings.Contains(response.Header().Get("Vary"), "Accept") {
						test.Fatal("find changed its wire contract or mutated application data", legacy, representation, continuation, headers, response.Code, response.Body.String(), matches)
					}
					assertDocFindHTTPNoLegacyReads(test, backend)
				}
			}
		}
	}
}

func TestDocFindHTTPRepresentationParserContract(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, representation := range []string{"", domain.DocFindDisplayRepresentation, domain.DocFindPreparedRepresentation} {
		for _, bounds := range [][2]int{{0, 12}, {0, 0}, {4, 7}, {12, 12}} {
			query := docFindHTTPInitialQuery(test, repo, hash, "needle")
			query.Set("from", strconv.Itoa(bounds[0]))
			query.Set("to", strconv.Itoa(bounds[1]))
			if representation != "" {
				query.Set("representation", representation)
			}
			server, backend, port := docFindHTTPTestServer()
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query.Encode()))
			want := domain.DocFindPageRequest{Representation: representation, Query: "needle", Selection: query.Get("selection"), From: bounds[0], To: bounds[1]}
			if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request != want {
				test.Fatal("initial representation lost exact query counts or bindings", query, writer.Code, port.request, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
		for _, terminal := range []bool{false, true} {
			selector := docFindHTTPTestSelector(repo, hash, "needle")
			selector.Representation = representation
			if representation != "" {
				selector.Version, selector.Field = 2, 0
			}
			if representation == domain.DocFindPreparedRepresentation {
				selector.Version, selector.Prepared = 3, &domain.DocFindPreparedProgress{}
			}
			if terminal {
				selector.Index, selector.Field, selector.Byte = selector.To, 0, 0
			}
			query := url.Values{"q": {"needle"}, "cursor": {encodeDocFindHTTPTestSelector(test, selector)}}
			if representation != "" {
				query.Set("representation", representation)
			}
			server, backend, port := docFindHTTPTestServer()
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query.Encode()))
			want := domain.DocFindPageRequest{Representation: representation, Query: "needle", Cursor: query.Get("cursor")}
			if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request != want {
				test.Fatal("continuation representation lost exact query counts or bindings", query, writer.Code, port.request, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
	}
}

func assertDocFindHTTPRepresentationQueriesRejected(test *testing.T, repo, hash domain.ContentHash, queries []string) {
	test.Helper()
	for index, query := range queries {
		test.Run(fmt.Sprintf("query-%d", index), func(test *testing.T) {
			server, backend, port := docFindHTTPTestServer()
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query))
			if writer.Code != http.StatusUnprocessableEntity || port.calls != 0 || !strings.Contains(writer.Body.String(), `"code":"validation"`) {
				test.Fatalf("representation reached port: query=%q status=%d calls=%d body=%s", query, writer.Code, port.calls, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		})
	}
}

func docFindHTTPInvalidRepresentationQueries(test *testing.T, initial, continuation url.Values) []string {
	test.Helper()
	var queries []string
	for _, values := range []url.Values{initial, continuation} {
		for _, representation := range []string{"", "source-v1", "display-v2", "DISPLAY-V1", " display-v1", "display-v1 "} {
			copy, err := url.ParseQuery(values.Encode())
			if err != nil {
				test.Fatal(err)
			}
			copy.Set("representation", representation)
			queries = append(queries, copy.Encode())
		}
		for _, suffix := range []string{"&representation=display-v1", "&representation=", "&representation=display-v1&representation=display-v1", "&representation=&representation=display-v1", "&unknown=value"} {
			queries = append(queries, values.Encode()+suffix)
		}
		for key := range values {
			queries = append(queries, values.Encode()+"&"+key+"="+url.QueryEscape(values.Get(key)))
			if key == "representation" || key == "base" {
				continue
			}
			copy, err := url.ParseQuery(values.Encode())
			if err != nil {
				test.Fatal(err)
			}
			copy.Del(key)
			queries = append(queries, copy.Encode())
		}
	}
	for key, values := range initial {
		if key != "q" && key != "representation" && key != "legacy" {
			queries = append(queries, continuation.Encode()+"&"+key+"="+url.QueryEscape(values[0]))
		}
	}
	queries = append(queries, initial.Encode()+"&cursor="+url.QueryEscape(continuation.Get("cursor")))
	return queries
}

func TestDocFindHTTPRejectsMixedRepresentationsBeforePort(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	initial := docFindHTTPInitialQuery(test, repo, hash, "needle")
	initial.Set("representation", domain.DocFindDisplayRepresentation)
	selector := docFindHTTPTestSelector(repo, hash, "needle")
	selector.Version, selector.Representation, selector.Field = 2, domain.DocFindDisplayRepresentation, 0
	cursor := encodeDocFindHTTPTestSelector(test, selector)
	continuation := url.Values{"q": {"needle"}, "cursor": {cursor}, "representation": {domain.DocFindDisplayRepresentation}}
	queries := docFindHTTPInvalidRepresentationQueries(test, initial, continuation)
	queries = append(queries, url.Values{"q": {"needle"}, "cursor": {cursor}}.Encode())
	sourceCursor := encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, "needle"))
	queries = append(queries, url.Values{"q": {"needle"}, "cursor": {sourceCursor}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	for _, mutate := range []func(*domain.DocFindSelector){
		func(value *domain.DocFindSelector) { value.Selection.Repo = domain.HashContent([]byte("other repo")) },
		func(value *domain.DocFindSelector) { value.Selection.Hash = domain.HashContent([]byte("other doc")) },
		func(value *domain.DocFindSelector) { value.QueryHash = domain.HashContent([]byte("NEEDLE")) },
	} {
		foreign := selector
		mutate(&foreign)
		queries = append(queries, url.Values{"q": {"needle"}, "cursor": {encodeDocFindHTTPTestSelector(test, foreign)}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	}
	for _, token := range []string{strings.Repeat("A", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes)+1),
		encodeDocLegacyFindHTTPTestSelector(test, domain.DocLegacyFindSelector{Version: 2, Representation: domain.DocFindDisplayRepresentation,
			Repo: repo, Hash: hash, QueryHash: domain.HashContent([]byte("needle"))})} {
		queries = append(queries, url.Values{"q": {"needle"}, "cursor": {token}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		test.Fatal(err)
	}
	for _, replacement := range [][2]string{{`{`, `{"unknown":0,`}, {`{`, `{"representation":"display-v1",`},
		{`"representation":"display-v1"`, `"representation":"display-v2"`}, {`"version":2`, `"version":1`}} {
		token := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), replacement[0], replacement[1], 1)))
		queries = append(queries, url.Values{"q": {"needle"}, "cursor": {token}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	}
	assertDocFindHTTPRepresentationQueriesRejected(test, repo, hash, queries)
}

func TestDocFindHTTPRejectsInvalidQueryBeforePort(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	initial := docFindHTTPInitialQuery(test, repo, hash, "needle")
	cursor := encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, "needle"))
	continuation := url.Values{"q": {"needle"}, "cursor": {cursor}}
	queries := []string{"", "q=needle", initial.Encode() + "&unknown=value", continuation.Encode() + "&projection=find-v1"}
	for _, values := range []url.Values{initial, continuation} {
		for key := range values {
			for _, mutate := range []func(url.Values){
				func(copy url.Values) { copy.Del(key) },
				func(copy url.Values) { copy.Set(key, "") },
				func(copy url.Values) { copy.Add(key, copy.Get(key)) },
				func(copy url.Values) { copy.Add(key, "") },
			} {
				copy, err := url.ParseQuery(values.Encode())
				if err != nil {
					test.Fatal(err)
				}
				mutate(copy)
				queries = append(queries, copy.Encode())
			}
		}
		for _, extra := range []string{"&=value", "&view=chat", "&base=" + string(hash), "&limit=20", "&offset=0", "&q=%zz", "&%zz=value", "&ignored=%zz", "&ignored=value;other=value"} {
			queries = append(queries, values.Encode()+extra)
		}
		for _, text := range []string{strings.Repeat("a", domain.MaxDocFindQueryRunes+1), strings.Repeat("한", domain.MaxDocFindQueryRunes+1), string([]byte{0xff}), "valid" + string([]byte{0xc3})} {
			copy, err := url.ParseQuery(values.Encode())
			if err != nil {
				test.Fatal(err)
			}
			copy.Set("q", text)
			queries = append(queries, copy.Encode())
		}
	}
	for _, key := range []string{"selection", "from", "to"} {
		queries = append(queries, continuation.Encode()+"&"+key+"="+url.QueryEscape(initial.Get(key)))
		queries = append(queries, continuation.Encode()+"&"+key+"=")
	}
	queries = append(queries, initial.Encode()+"&cursor="+cursor)
	for _, key := range []string{"from", "to"} {
		for _, raw := range []string{"-1", "-0", "00", "01", "+1", " 1", "1 ", "1.0", "1e1", "0x1", "１", "13", strconv.Itoa(domain.MaxConversationDocumentBytes + 1), "9999999999999999999999999"} {
			copy := docFindHTTPInitialQuery(test, repo, hash, "needle")
			copy.Set(key, raw)
			queries = append(queries, copy.Encode())
		}
	}
	for _, bounds := range [][2]int{{5, 4}, {12, 11}} {
		copy := docFindHTTPInitialQuery(test, repo, hash, "needle")
		copy.Set("from", strconv.Itoa(bounds[0]))
		copy.Set("to", strconv.Itoa(bounds[1]))
		queries = append(queries, copy.Encode())
	}
	selection := docVisibleTestSelector(repo, hash, "units")
	selection.Start = 3
	copy := docFindHTTPInitialQuery(test, repo, hash, "needle")
	copy.Set("selection", encodeDocVisibleTestSelector(test, selection))
	queries = append(queries, copy.Encode())
	for _, mutate := range []func(*domain.DocVisibleSelector){
		func(selector *domain.DocVisibleSelector) { selector.Repo = domain.HashContent([]byte("other repo")) },
		func(selector *domain.DocVisibleSelector) { selector.Hash = domain.HashContent([]byte("other doc")) },
		func(selector *domain.DocVisibleSelector) { selector.Purpose = "fold" },
	} {
		selection = docVisibleTestSelector(repo, hash, "units")
		mutate(&selection)
		copy := docFindHTTPInitialQuery(test, repo, hash, "needle")
		copy.Set("selection", encodeDocVisibleTestSelector(test, selection))
		queries = append(queries, copy.Encode())
	}
	for _, mutate := range []func(*domain.DocFindSelector){
		func(selector *domain.DocFindSelector) {
			selector.Selection.Repo = domain.HashContent([]byte("other repo"))
		},
		func(selector *domain.DocFindSelector) {
			selector.Selection.Hash = domain.HashContent([]byte("other doc"))
		},
		func(selector *domain.DocFindSelector) { selector.QueryHash = domain.HashContent([]byte("NEEDLE")) },
	} {
		selector := docFindHTTPTestSelector(repo, hash, "needle")
		mutate(&selector)
		queries = append(queries, url.Values{"q": {"needle"}, "cursor": {encodeDocFindHTTPTestSelector(test, selector)}}.Encode())
	}
	for _, token := range []struct{ key, value string }{{"selection", initial.Get("selection")}, {"cursor", cursor}} {
		raw, err := base64.RawURLEncoding.DecodeString(token.value)
		if err != nil {
			test.Fatal(err)
		}
		malformed := []string{"not-a-selector", "e30", token.value + "=", " " + token.value, strings.Repeat("a", 10000), initial.Get("selection"), cursor}
		malformed = append(malformed,
			base64.RawURLEncoding.EncodeToString(append(append([]byte{}, raw...), ' ')),
			base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `{`, `{"extra":true,`, 1))),
			base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), `{`, `{"version":1,`, 1))),
		)
		if token.key == "cursor" {
			for _, replace := range [][2]string{{`"version":1`, `"version":2`}, {`"purpose":"units"`, `"purpose":"fold"`}, {`"index":4`, `"index":13`}, {`"field":1`, `"field":-1`}, {`"byte":2`, `"byte":-1`}, {`"from":0`, `"from":-1`}, {`"to":12`, `"to":13`}} {
				malformed = append(malformed, base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), replace[0], replace[1], 1))))
			}
		}
		for _, value := range malformed {
			if value == token.value {
				continue
			}
			values := url.Values{"q": {"needle"}, "cursor": {value}}
			if token.key == "selection" {
				values = docFindHTTPInitialQuery(test, repo, hash, "needle")
				values.Set("selection", value)
			}
			queries = append(queries, values.Encode())
		}
	}
	for index, query := range queries {
		test.Run(fmt.Sprintf("query-%d", index), func(test *testing.T) {
			server, backend, port := docFindHTTPTestServer()
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query))
			if writer.Code != http.StatusUnprocessableEntity || !strings.Contains(writer.Body.String(), `"code":"validation"`) || port.calls != 0 {
				test.Fatalf("query=%q status=%d calls=%d body=%s", query, writer.Code, port.calls, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		})
	}
	for _, invalid := range []domain.ContentHash{"invalid", domain.ContentHash(strings.ToUpper(string(repo)))} {
		for _, identity := range [][2]domain.ContentHash{{invalid, hash}, {repo, invalid}} {
			server, backend, port := docFindHTTPTestServer()
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(identity[0], identity[1], initial.Encode()))
			if writer.Code != http.StatusUnprocessableEntity || port.calls != 0 {
				test.Fatalf("invalid route identity reached port: status=%d calls=%d", writer.Code, port.calls)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
	}
}

func TestDocFindHTTPExplicitPortAndUnavailable(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	backend := &struct {
		*docVisibleHTTPBackend
		*docFindHTTPPort
	}{&docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}, &docFindHTTPPort{}}
	server := &Server{b: backend}
	query := docFindHTTPInitialQuery(test, repo, hash, "needle").Encode()
	port := &docFindHTTPPort{}
	for _, configured := range []inbound.DocumentFindQuery{nil, port, nil} {
		server.SetDocumentFind(configured)
		writer := httptest.NewRecorder()
		docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query))
		if configured == nil {
			if writer.Code != http.StatusServiceUnavailable || !strings.Contains(writer.Body.String(), `"code":"unavailable"`) {
				test.Fatalf("missing port did not fail closed: status=%d body=%s", writer.Code, writer.Body)
			}
		} else if writer.Code != http.StatusOK || port.calls != 1 {
			test.Fatalf("explicit port unused: status=%d calls=%d body=%s", writer.Code, port.calls, writer.Body)
		}
		if backend.docFindHTTPPort.calls != 0 {
			test.Fatal("find dynamically used the legacy backend")
		}
		assertDocFindHTTPNoLegacyReads(test, backend.docVisibleHTTPBackend)
	}
}

func TestDocFindHTTPWireContract(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, terminal := range []bool{false, true} {
		page := domain.DocFindPage{
			Projection: domain.DocFindProjection, Hash: hash, Generation: domain.HashContent([]byte("visible generation")),
			From: 0, To: 12, Index: 4, Field: 1, Byte: 2,
			Matches: []domain.DocFindMatch{{Index: 3, Field: 1, Offset: 4, Length: 6, Raw: false}, {Index: 4, Field: 0, Offset: 7, Length: 6, Raw: true}},
		}
		for index := range page.Matches {
			match := &page.Matches[index]
			match.Preview = strings.Repeat("x", match.Offset) + "needle suffix"
			selection := docVisibleTestSelector(repo, hash, "units")
			selection.View, selection.Start, selection.Next, selection.Unit, selection.Limit = "all", match.Index, match.Index, match.Index, 1
			match.RevealCursor = encodeDocVisibleTestSelector(test, selection)
		}
		if terminal {
			page.Index, page.Field, page.Byte, page.Matches = page.To, 0, 0, []domain.DocFindMatch{}
		} else {
			cursor := encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, "needle"))
			page.NextCursor = &cursor
		}
		server, backend, port := docFindHTTPTestServer()
		port.find = func(context.Context) (domain.DocFindPage, error) { return page, nil }
		writer := httptest.NewRecorder()
		docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, docFindHTTPInitialQuery(test, repo, hash, "needle").Encode()))
		expected, err := json.Marshal(page)
		if err != nil {
			test.Fatal(err)
		}
		if writer.Code != http.StatusOK || strings.TrimSpace(writer.Body.String()) != string(expected) || writer.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			test.Fatalf("wire contract changed: status=%d body=%s want=%s", writer.Code, writer.Body, expected)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(writer.Body.Bytes(), &fields); err != nil {
			test.Fatal(err)
		}
		if len(fields) != 10 || terminal && (string(fields["next_cursor"]) != "null" || string(fields["matches"]) != "[]") {
			test.Fatalf("unexpected find fields: %s", writer.Body)
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
	}
}

func TestDocFindHTTPErrorMapping(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, testCase := range []struct {
		err    error
		status int
		code   string
		retry  string
	}{
		{domain.ErrUnauthorized, http.StatusUnauthorized, "unauthenticated", ""},
		{domain.ErrForbidden, http.StatusForbidden, "forbidden", ""},
		{domain.ErrDocVisibleGenerationStale, http.StatusConflict, "doc_visible_generation_stale", ""},
		{domain.ErrDocVisibleProjectionUnsupported, http.StatusConflict, "doc_visible_projection_unsupported", ""},
		{domain.ErrDocFindFieldsUnavailable, http.StatusServiceUnavailable, "doc_find_fields_unavailable", ""},
		{domain.ErrDocFindDisplayUnavailable, http.StatusServiceUnavailable, "doc_find_display_unavailable", ""},
		{domain.ErrDocFindFieldsPending, http.StatusServiceUnavailable, "doc_find_fields_pending", "1"},
		{domain.ErrDocDisplayPending, http.StatusServiceUnavailable, "doc_display_pending", "1"},
		{domain.ErrValidation, http.StatusUnprocessableEntity, "validation", ""},
		{domain.ErrDocRangeIndexPending, http.StatusServiceUnavailable, "doc_range_index_pending", "3"},
		{domain.ErrResourceOverloaded, http.StatusServiceUnavailable, "resource_overloaded", "1"},
	} {
		test.Run(testCase.code, func(test *testing.T) {
			server, backend, port := docFindHTTPTestServer()
			port.find = func(context.Context) (domain.DocFindPage, error) {
				return domain.DocFindPage{}, fmt.Errorf("find: %w", testCase.err)
			}
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, docFindHTTPInitialQuery(test, repo, hash, "needle").Encode()))
			if writer.Code != testCase.status || !strings.Contains(writer.Body.String(), `"code":"`+testCase.code+`"`) || writer.Header().Get("Retry-After") != testCase.retry || port.calls != 1 {
				test.Fatalf("error mapping changed: status=%d calls=%d body=%s", writer.Code, port.calls, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		})
	}
}

func TestDocFindHTTPGuardAndDeferredAuthorization(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	queries := []string{docFindHTTPInitialQuery(test, repo, hash, "needle").Encode(), url.Values{"q": {"needle"}, "cursor": {encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, "needle"))}}.Encode()}
	for mode, query := range queries {
		for _, change := range []string{"anonymous", "outsider", "unchanged", "revoked credential", "revoked membership", "foreign repo"} {
			test.Run(fmt.Sprintf("mode-%d/%s", mode, change), func(test *testing.T) {
				policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
				identity := &docVisiblePolicyIdentity{repository: domain.Repository{ID: "repository"}, user: domain.User{ID: "reader"}, role: domain.RoleViewer}
				server, backend, port := docFindHTTPTestServer()
				backend.Backend, server.id = policy, identity
				if change == "outsider" {
					identity.role = ""
				}
				reads := 0
				port.find = func(ctx context.Context) (domain.DocFindPage, error) {
					if policy.calls != 1 || identity.resolveCalls != 1 {
						test.Fatal("HTTP performed deferred authorization before port admission")
					}
					authorize := inbound.DocumentReadAuthorizer(ctx)
					if authorize == nil {
						test.Fatal("missing deferred document authorizer")
					}
					requestedRepo := repo
					switch change {
					case "revoked credential":
						identity.credentialErr = domain.ErrUnauthorized
					case "revoked membership":
						identity.role = ""
					case "foreign repo":
						requestedRepo = domain.HashContent([]byte("foreign repo"))
					}
					current, err := authorize(ctx, requestedRepo)
					if err != nil {
						return domain.DocFindPage{}, err
					}
					actor, system := inbound.RepositoryActor(current)
					if actor != identity.user.ID || system {
						test.Fatal("deferred authorization lost the request actor")
					}
					reads++
					return domain.DocFindPage{Projection: domain.DocFindProjection}, nil
				}
				request := docFindHTTPTestRequest(repo, hash, query)
				if change != "anonymous" {
					request.Header.Set("Authorization", "Bearer session")
				}
				writer := httptest.NewRecorder()
				docFindHTTPTestRouter(server.guard(domain.RoleViewer, server.getDocFindPage)).ServeHTTP(writer, request)
				want, code, calls := http.StatusOK, "", 1
				switch change {
				case "anonymous":
					want, code, calls = http.StatusUnauthorized, "unauthenticated", 0
				case "outsider":
					want, code, calls = http.StatusForbidden, "forbidden", 0
				case "revoked credential":
					want, code = http.StatusUnauthorized, "unauthenticated"
				case "revoked membership", "foreign repo":
					want, code = http.StatusForbidden, "forbidden"
				}
				if writer.Code != want || port.calls != calls || want != http.StatusOK && (reads != 0 || !strings.Contains(writer.Body.String(), `"code":"`+code+`"`)) || want == http.StatusOK && reads != 1 {
					test.Fatalf("guard/deferred access changed: status=%d want=%d calls=%d reads=%d body=%s", writer.Code, want, port.calls, reads, writer.Body)
				}
				if change == "foreign repo" && (policy.calls != 1 || identity.resolveCalls != 1) {
					test.Fatal("foreign repo authorizer performed policy IO")
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			})
		}
	}
}

func TestDocFindHTTPDeadlineAndCancellation(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	queries := []string{docFindHTTPInitialQuery(test, repo, hash, "needle").Encode(), url.Values{"q": {"needle"}, "cursor": {encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, "needle"))}}.Encode()}
	for mode, query := range queries {
		for _, parentLimit := range []time.Duration{0, 30 * time.Second} {
			test.Run(fmt.Sprintf("mode-%d/parent-%s", mode, parentLimit), func(test *testing.T) {
				parent := context.Background()
				if parentLimit != 0 {
					var cancel context.CancelFunc
					parent, cancel = context.WithTimeout(parent, parentLimit)
					defer cancel()
				}
				server, backend, port := docFindHTTPTestServer()
				var observed context.Context
				port.find = func(ctx context.Context) (domain.DocFindPage, error) {
					observed = ctx
					if inbound.DocumentReadAuthorizer(ctx) == nil || ctx.Err() != nil {
						test.Fatal("missing authorizer or already canceled request")
					}
					return domain.DocFindPage{}, nil
				}
				writer := &docVisibleDeadlineWriter{deadlineDocWriter: &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder()}}
				start := time.Now()
				docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query).WithContext(parent))
				if observed == nil || writer.Code != http.StatusOK || writer.readCalls != 1 || writer.writeCalls != 1 {
					test.Fatalf("request was not bounded: status=%d body=%s", writer.Code, writer.Body)
				}
				deadline, present := observed.Deadline()
				if !present || !deadline.Equal(writer.read) || !deadline.Equal(writer.write) {
					test.Fatal("transport and port deadlines differ")
				}
				if parentLimit == 0 {
					if deadline.Before(start.Add(resourceRequestLimit)) || deadline.After(time.Now().Add(resourceRequestLimit)) {
						test.Fatal("find request has an unbounded deadline")
					}
				} else if expected, _ := parent.Deadline(); !deadline.Equal(expected) {
					test.Fatal("find extended the parent deadline")
				}
				if !errors.Is(observed.Err(), context.Canceled) || parent.Err() != nil {
					test.Fatal("request context not released or parent canceled")
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			})
		}
		for _, parentErr := range []error{context.Canceled, context.DeadlineExceeded} {
			parent, cancel := context.WithCancel(context.Background())
			if parentErr == context.DeadlineExceeded {
				cancel()
				parent, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			}
			cancel()
			server, backend, port := docFindHTTPTestServer()
			port.find = func(ctx context.Context) (domain.DocFindPage, error) {
				if !errors.Is(ctx.Err(), parentErr) {
					test.Fatalf("parent cancellation lost: got=%v want=%v", ctx.Err(), parentErr)
				}
				authorize := inbound.DocumentReadAuthorizer(ctx)
				if authorize == nil {
					test.Fatal("missing canceled request authorizer")
				}
				if _, err := authorize(ctx, repo); !errors.Is(err, parentErr) {
					test.Fatal("canceled authorizer performed policy IO", err)
				}
				return domain.DocFindPage{}, ctx.Err()
			}
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(httptest.NewRecorder(), docFindHTTPTestRequest(repo, hash, query).WithContext(parent))
			if port.calls != 1 {
				test.Fatal("port did not receive parent cancellation")
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
		for _, writeFailure := range []bool{false, true} {
			server, backend, port := docFindHTTPTestServer()
			writer := &docFindHTTPDeadlineFailureWriter{deadlineDocWriter: &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder()}, writeFailure: writeFailure}
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query))
			if writer.Code != http.StatusInternalServerError || port.calls != 0 {
				test.Fatalf("deadline failure reached port: status=%d calls=%d body=%s", writer.Code, port.calls, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
	}
}

type docFindHTTPDeadlineFailureWriter struct {
	*deadlineDocWriter
	writeFailure bool
}

func (writer *docFindHTTPDeadlineFailureWriter) SetReadDeadline(deadline time.Time) error {
	if !writer.writeFailure {
		return errors.New("read deadline failed")
	}
	return writer.deadlineDocWriter.SetReadDeadline(deadline)
}

func (writer *docFindHTTPDeadlineFailureWriter) SetWriteDeadline(time.Time) error {
	return errors.New("write deadline failed")
}

func TestDocFindOpenAPISchemaFieldDrift(test *testing.T) {
	const spec = "../../../../../schemas/openapi.yaml"
	for _, testCase := range []struct {
		name   string
		typeOf reflect.Type
	}{
		{"DocFindPage", reflect.TypeOf(domain.DocFindPage{})},
		{"DocFindMatch", reflect.TypeOf(domain.DocFindMatch{})},
		{"DocFindSelector", reflect.TypeOf(domain.DocFindSelector{})},
		{"DocLegacyFindSelector", reflect.TypeOf(domain.DocLegacyFindSelector{})},
		{"DocFindPreparedProgress", reflect.TypeOf(domain.DocFindPreparedProgress{})},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			properties, fields := schemaProps(test, spec, testCase.name), jsonFields(testCase.typeOf)
			for _, field := range fields {
				if !properties[field] {
					test.Errorf("missing schema property %s", field)
				}
			}
			for property := range properties {
				if !contains(fields, property) {
					test.Errorf("phantom schema property %s", property)
				}
			}
		})
	}
	raw, err := os.ReadFile(spec)
	if err != nil {
		test.Fatal(err)
	}
	_, route, present := strings.Cut(string(raw), "\n  /repos/{repoID}/docs/{hash}/find:\n")
	if !present {
		test.Fatal("missing document find route")
	}
	route, _, _ = strings.Cut(route, "\n  /")
	for _, contract := range []string{"    get:", "name: q", "name: selection", "name: from", "name: to", "name: cursor", "minLength: 1", "maxLength: 256", "#/components/schemas/DocFindPage"} {
		if !strings.Contains(route, contract) {
			test.Errorf("missing document find route contract %q", contract)
		}
	}
	for _, status := range []int{200, 401, 403, 409, 422, 503} {
		if !regexp.MustCompile(fmt.Sprintf(`(?m)^        ["']?%d["']?:`, status)).MatchString(route) {
			test.Errorf("missing document find response %d", status)
		}
	}
}
