package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docLegacyFindHTTPInitialQuery(query string) url.Values {
	return url.Values{"q": {query}, "legacy": {"1"}, "from": {"0"}, "to": {"12"}, "total": {"12"}, "inherited": {"0"}}
}

func docLegacyFindHTTPTestSelector(repo, hash domain.ContentHash, query string) domain.DocLegacyFindSelector {
	return domain.DocLegacyFindSelector{Version: 1, Repo: repo, Hash: hash, Base: domain.HashContent([]byte("base")),
		Total: 12, Inherited: 3, From: 3, To: 12, Index: 4, Field: 1, Byte: 2, QueryHash: domain.HashContent([]byte(query))}
}

func encodeDocLegacyFindHTTPTestSelector(test *testing.T, selector domain.DocLegacyFindSelector) string {
	test.Helper()
	encoded, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return encoded
}

func TestDocLegacyFindHTTPParserContract(test *testing.T) {
	repo, hash, base := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc")), domain.HashContent([]byte("base"))
	for _, text := range []string{"x", " ", "\n", "a+b&c", "\ud55c\uae00🙂", strings.Repeat("\ud55c", domain.MaxDocFindQueryRunes)} {
		test.Run(text, func(test *testing.T) {
			for _, fixture := range []struct {
				base                       domain.ContentHash
				from, to, total, inherited int
			}{
				{"", 0, 12, 12, 0}, {base, 3, 7, 12, 3}, {base, 0, 0, 0, 0}, {base, 12, 12, 12, 12},
				{base, 0, 12, 12, 12}, {"", 0, domain.MaxConversationDocumentBytes, domain.MaxConversationDocumentBytes, 0},
				{base, domain.MaxConversationDocumentBytes, domain.MaxConversationDocumentBytes, domain.MaxConversationDocumentBytes, domain.MaxConversationDocumentBytes},
			} {
				initial := docLegacyFindHTTPInitialQuery(text)
				initial.Set("from", strconv.Itoa(fixture.from))
				initial.Set("to", strconv.Itoa(fixture.to))
				initial.Set("total", strconv.Itoa(fixture.total))
				initial.Set("inherited", strconv.Itoa(fixture.inherited))
				if fixture.base != "" {
					initial.Set("base", string(fixture.base))
				}
				server, backend, port := docFindHTTPTestServer()
				writer := httptest.NewRecorder()
				docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, initial.Encode()))
				want := domain.DocFindPageRequest{Legacy: true, Query: text, Base: fixture.base, From: fixture.from, To: fixture.to, Total: fixture.total, Inherited: fixture.inherited}
				if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request != want {
					test.Fatalf("status=%d calls=%d request=%+v want=%+v body=%s", writer.Code, port.calls, port.request, want, writer.Body)
				}
				selector, err := domain.DocLegacyFindSelection(repo, hash, port.request)
				if err != nil || selector.Base != fixture.base || selector.Total != fixture.total || selector.Inherited != fixture.inherited || selector.Index != fixture.from || selector.QueryHash != domain.HashContent([]byte(text)) {
					test.Fatal("parsed request lost selector identity", selector, err)
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			}
			for _, terminal := range []bool{false, true} {
				selector := docLegacyFindHTTPTestSelector(repo, hash, text)
				if terminal {
					selector.Index, selector.Field, selector.Byte = selector.To, 0, 0
				}
				cursor := encodeDocLegacyFindHTTPTestSelector(test, selector)
				server, backend, port := docFindHTTPTestServer()
				writer := httptest.NewRecorder()
				docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, url.Values{"q": {text}, "legacy": {"1"}, "cursor": {cursor}}.Encode()))
				want := domain.DocFindPageRequest{Legacy: true, Query: text, Cursor: cursor}
				if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request != want {
					test.Fatalf("continuation changed: status=%d request=%+v body=%s", writer.Code, port.request, writer.Body)
				}
				decoded, err := domain.DecodeDocLegacyFindSelector(port.request.Cursor)
				if err != nil || decoded != selector {
					test.Fatal("forwarded cursor did not decode to the bound Go selector", decoded, err)
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			}
		})
	}
}

func TestDocLegacyFindHTTPRepresentationParserContract(test *testing.T) {
	repo, hash, base := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc")), domain.HashContent([]byte("base"))
	for _, representation := range []string{"", domain.DocFindDisplayRepresentation, domain.DocFindPreparedRepresentation} {
		for _, sourceBase := range []domain.ContentHash{"", base} {
			for _, bounds := range [][2]int{{0, 12}, {0, 0}, {4, 7}, {12, 12}} {
				query := docLegacyFindHTTPInitialQuery("needle")
				query.Set("from", strconv.Itoa(bounds[0]))
				query.Set("to", strconv.Itoa(bounds[1]))
				if sourceBase != "" {
					query.Set("base", string(sourceBase))
				}
				if representation != "" {
					query.Set("representation", representation)
				}
				server, backend, port := docFindHTTPTestServer()
				writer := httptest.NewRecorder()
				docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query.Encode()))
				want := domain.DocFindPageRequest{Legacy: true, Representation: representation, Query: "needle", Base: sourceBase, Total: 12, From: bounds[0], To: bounds[1]}
				if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request != want {
					test.Fatal("legacy initial representation lost exact counts or bindings", query, writer.Code, port.request, writer.Body)
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			}
		}
		for _, terminal := range []bool{false, true} {
			selector := docLegacyFindHTTPTestSelector(repo, hash, "needle")
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
			query := url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {encodeDocLegacyFindHTTPTestSelector(test, selector)}}
			if representation != "" {
				query.Set("representation", representation)
			}
			server, backend, port := docFindHTTPTestServer()
			writer := httptest.NewRecorder()
			docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query.Encode()))
			want := domain.DocFindPageRequest{Legacy: true, Representation: representation, Query: "needle", Cursor: query.Get("cursor")}
			if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request != want {
				test.Fatal("legacy continuation representation lost exact counts or bindings", query, writer.Code, port.request, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
	}
}

func TestDocLegacyFindHTTPRejectsMixedRepresentationsBeforePort(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	initial := docLegacyFindHTTPInitialQuery("needle")
	initial.Set("representation", domain.DocFindDisplayRepresentation)
	selector := docLegacyFindHTTPTestSelector(repo, hash, "needle")
	selector.Version, selector.Representation, selector.Field = 2, domain.DocFindDisplayRepresentation, 0
	cursor := encodeDocLegacyFindHTTPTestSelector(test, selector)
	continuation := url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {cursor}, "representation": {domain.DocFindDisplayRepresentation}}
	queries := docFindHTTPInvalidRepresentationQueries(test, initial, continuation)
	initial.Set("base", string(selector.Base))
	queries = append(queries, docFindHTTPInvalidRepresentationQueries(test, initial, continuation)...)
	queries = append(queries, url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {cursor}}.Encode())
	sourceCursor := encodeDocLegacyFindHTTPTestSelector(test, docLegacyFindHTTPTestSelector(repo, hash, "needle"))
	queries = append(queries, url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {sourceCursor}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	queries = append(queries, initial.Encode()+"&selection=root", continuation.Encode()+"&selection=root")
	for _, mutate := range []func(*domain.DocLegacyFindSelector){
		func(value *domain.DocLegacyFindSelector) { value.Repo = domain.HashContent([]byte("other repo")) },
		func(value *domain.DocLegacyFindSelector) { value.Hash = domain.HashContent([]byte("other doc")) },
		func(value *domain.DocLegacyFindSelector) { value.QueryHash = domain.HashContent([]byte("NEEDLE")) },
	} {
		foreign := selector
		mutate(&foreign)
		queries = append(queries, url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {encodeDocLegacyFindHTTPTestSelector(test, foreign)}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	}
	root := docFindHTTPTestSelector(repo, hash, "needle")
	root.Version, root.Representation, root.Field = 2, domain.DocFindDisplayRepresentation, 0
	for _, token := range []string{encodeDocFindHTTPTestSelector(test, root), strings.Repeat("A", base64.RawURLEncoding.EncodedLen(domain.MaxDocLegacyFindSelectorBytes)+1)} {
		queries = append(queries, url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {token}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		test.Fatal(err)
	}
	for _, replacement := range [][2]string{{`{`, `{"unknown":0,`}, {`{`, `{"representation":"display-v1",`},
		{`"representation":"display-v1"`, `"representation":"display-v2"`}, {`"version":2`, `"version":1`}} {
		token := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), replacement[0], replacement[1], 1)))
		queries = append(queries, url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {token}, "representation": {domain.DocFindDisplayRepresentation}}.Encode())
	}
	assertDocFindHTTPRepresentationQueriesRejected(test, repo, hash, queries)
}

func TestDocLegacyFindHTTPRejectsInvalidQueryBeforePort(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	initial := docLegacyFindHTTPInitialQuery("needle")
	withBase := docLegacyFindHTTPInitialQuery("needle")
	withBase.Set("base", string(domain.HashContent([]byte("base"))))
	cursor := encodeDocLegacyFindHTTPTestSelector(test, docLegacyFindHTTPTestSelector(repo, hash, "needle"))
	continuation := url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {cursor}}
	queries := []string{"legacy=1", "legacy=1&q=needle", initial.Encode() + "&unknown=value", initial.Encode() + "&cursor=" + cursor}
	for _, values := range []url.Values{initial, withBase, continuation} {
		for key := range values {
			for mutation := 0; mutation < 4; mutation++ {
				if key == "base" && mutation == 0 {
					continue
				}
				copy, err := url.ParseQuery(values.Encode())
				if err != nil {
					test.Fatal(err)
				}
				switch mutation {
				case 0:
					copy.Del(key)
				case 1:
					copy.Set(key, "")
				case 2:
					copy.Add(key, copy.Get(key))
				case 3:
					copy.Add(key, "")
				}
				queries = append(queries, copy.Encode())
			}
		}
		for _, extra := range []string{"&=value", "&selection=root", "&selection=", "&projection=find-legacy-v1", "&view=all", "&limit=20", "&offset=0", "&q=%zz", "&%zz=value", "&ignored=%zz", "&ignored=value;other=value"} {
			queries = append(queries, values.Encode()+extra)
		}
		for _, flag := range []string{"0", "2", "true", "01", "+1", " 1", "1 "} {
			copy, err := url.ParseQuery(values.Encode())
			if err != nil {
				test.Fatal(err)
			}
			copy.Set("legacy", flag)
			queries = append(queries, copy.Encode())
		}
		for _, text := range []string{strings.Repeat("a", domain.MaxDocFindQueryRunes+1), strings.Repeat("\ud55c", domain.MaxDocFindQueryRunes+1), "\xff", "valid\xc3"} {
			copy, err := url.ParseQuery(values.Encode())
			if err != nil {
				test.Fatal(err)
			}
			copy.Set("q", text)
			queries = append(queries, copy.Encode())
		}
	}
	for _, key := range []string{"from", "to", "total", "inherited", "base", "selection"} {
		for _, value := range []string{"", "0", initial.Get(key), string(hash)} {
			queries = append(queries, continuation.Encode()+"&"+key+"="+url.QueryEscape(value))
		}
	}
	for _, key := range []string{"from", "to", "total", "inherited"} {
		for _, raw := range []string{"-1", "-0", "00", "01", "+1", " 1", "1 ", "1.0", "1e1", "0x1", "１", strconv.Itoa(domain.MaxConversationDocumentBytes + 1), "9999999999999999999999999"} {
			copy := docLegacyFindHTTPInitialQuery("needle")
			copy.Set(key, raw)
			queries = append(queries, copy.Encode())
		}
	}
	for _, fixture := range []struct{ key, value string }{{"from", "13"}, {"to", "13"}, {"total", "11"}, {"inherited", "13"}, {"base", "invalid"}, {"base", strings.ToUpper(string(hash))}} {
		copy := docLegacyFindHTTPInitialQuery("needle")
		copy.Set(fixture.key, fixture.value)
		queries = append(queries, copy.Encode())
	}
	reversed := docLegacyFindHTTPInitialQuery("needle")
	reversed.Set("from", "5")
	reversed.Set("to", "4")
	queries = append(queries, reversed.Encode())
	for _, mutate := range []func(*domain.DocLegacyFindSelector){
		func(selector *domain.DocLegacyFindSelector) { selector.Repo = domain.HashContent([]byte("other repo")) },
		func(selector *domain.DocLegacyFindSelector) { selector.Hash = domain.HashContent([]byte("other doc")) },
		func(selector *domain.DocLegacyFindSelector) {
			selector.QueryHash = domain.HashContent([]byte("NEEDLE"))
		},
	} {
		selector := docLegacyFindHTTPTestSelector(repo, hash, "needle")
		mutate(&selector)
		queries = append(queries, url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {encodeDocLegacyFindHTTPTestSelector(test, selector)}}.Encode())
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		test.Fatal(err)
	}
	malformed := []string{"not-a-selector", "e30", cursor + "=", " " + cursor, cursor + "\n", strings.Repeat("a", 10000),
		encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, "needle")),
		encodeDocVisibleTestSelector(test, docVisibleTestSelector(repo, hash, "units"))}
	for _, replacement := range [][2]string{{`{`, `{"extra":true,`}, {`{`, `{"version":1,`}, {`"version":1`, `"version":2`},
		{`"event_index":4`, `"event_index":13`}, {`"field":1`, `"field":-1`}, {`"byte_offset":2`, `"byte_offset":-1`},
		{`"from":3`, `"from":-1`}, {`"to":12`, `"to":13`}, {`"inherited":3`, `"inherited":13`}, {`"total":12`, `"total":-1`}} {
		malformed = append(malformed, base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), replacement[0], replacement[1], 1))))
	}
	malformed = append(malformed, base64.RawURLEncoding.EncodeToString(append(append([]byte{}, raw...), ' ')))
	for _, value := range malformed {
		queries = append(queries, url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {value}}.Encode())
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
	for _, values := range []url.Values{initial, continuation} {
		for _, invalid := range []domain.ContentHash{"invalid", "", domain.ContentHash(strings.ToUpper(string(repo)))} {
			if invalid == "" {
				for _, route := range [][2]domain.ContentHash{{invalid, hash}, {repo, invalid}} {
					if _, err := docFindPageRequest(values, route[0], route[1]); err == nil {
						test.Fatal("parser accepted empty route hash")
					}
				}
				continue
			}
			for _, route := range [][2]domain.ContentHash{{invalid, hash}, {repo, invalid}} {
				server, backend, port := docFindHTTPTestServer()
				writer := httptest.NewRecorder()
				docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(route[0], route[1], values.Encode()))
				if writer.Code != http.StatusUnprocessableEntity || port.calls != 0 {
					test.Fatalf("invalid route reached port: status=%d calls=%d", writer.Code, port.calls)
				}
				assertDocFindHTTPNoLegacyReads(test, backend)
			}
		}
	}
}

func TestDocLegacyFindHTTPDoesNotExtendRootQueries(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	rootInitial := docFindHTTPInitialQuery(test, repo, hash, "needle")
	rootCursor := encodeDocFindHTTPTestSelector(test, docFindHTTPTestSelector(repo, hash, "needle"))
	legacyCursor := encodeDocLegacyFindHTTPTestSelector(test, docLegacyFindHTTPTestSelector(repo, hash, "needle"))
	queries := []string{url.Values{"q": {"needle"}, "cursor": {legacyCursor}}.Encode(), url.Values{"q": {"needle"}, "legacy": {"1"}, "cursor": {rootCursor}}.Encode()}
	for _, root := range []url.Values{rootInitial, {"q": {"needle"}, "cursor": {rootCursor}}} {
		for _, extra := range []string{"&legacy", "&legacy=", "&legacy=1", "&legacy=0", "&legacy=&legacy=1", "&total=12", "&inherited=0", "&base=" + string(hash)} {
			queries = append(queries, root.Encode()+extra)
		}
	}
	for _, query := range queries {
		server, backend, port := docFindHTTPTestServer()
		writer := httptest.NewRecorder()
		docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query))
		if writer.Code != http.StatusUnprocessableEntity || port.calls != 0 {
			test.Fatalf("root/legacy modes mixed: query=%q status=%d calls=%d body=%s", query, writer.Code, port.calls, writer.Body)
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
	}
}

func TestDocLegacyFindHTTPWireAndNoReaderFallback(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	selector := docLegacyFindHTTPTestSelector(repo, hash, "needle")
	cursor := encodeDocLegacyFindHTTPTestSelector(test, selector)
	for _, terminal := range []bool{false, true} {
		page := domain.DocFindPage{Projection: domain.DocLegacyFindProjection, Hash: hash, From: 3, To: 12, Index: 4, Field: 1, Byte: 2,
			Matches: []domain.DocFindMatch{{Index: 3, Field: 0, Offset: 4, Length: 6, Preview: "raw needle result"}}}
		if terminal {
			page.Index, page.Field, page.Byte, page.Matches = page.To, 0, 0, []domain.DocFindMatch{}
		} else {
			page.NextCursor = &cursor
		}
		server, backend, port := docFindHTTPTestServer()
		port.find = func(context.Context) (domain.DocFindPage, error) { return page, nil }
		writer := httptest.NewRecorder()
		docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, docLegacyFindHTTPInitialQuery("needle").Encode()))
		want, err := json.Marshal(page)
		if err != nil {
			test.Fatal(err)
		}
		if writer.Code != http.StatusOK || port.calls != 1 || !port.request.Legacy || strings.TrimSpace(writer.Body.String()) != string(want) {
			test.Fatalf("legacy wire changed: status=%d calls=%d body=%s want=%s", writer.Code, port.calls, writer.Body, want)
		}
		var decoded domain.DocFindPage
		if err := json.Unmarshal(writer.Body.Bytes(), &decoded); err != nil || decoded.Projection != domain.DocLegacyFindProjection || decoded.Generation != "" {
			test.Fatal("legacy response invented a visible generation", decoded, err)
		}
		for _, match := range decoded.Matches {
			if match.RevealCursor != "" {
				test.Fatal("legacy response invented a root reveal cursor", match)
			}
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
	}
	for _, query := range []url.Values{docLegacyFindHTTPInitialQuery("needle"), {"q": {"needle"}, "legacy": {"1"}, "cursor": {cursor}}} {
		server, backend, port := docFindHTTPTestServer()
		port.find = func(context.Context) (domain.DocFindPage, error) {
			return domain.DocFindPage{}, domain.ErrDocFindFieldsUnavailable
		}
		writer := httptest.NewRecorder()
		docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query.Encode()))
		if writer.Code != http.StatusServiceUnavailable || port.calls != 1 || !port.request.Legacy {
			test.Fatalf("legacy error fell back: status=%d calls=%d body=%s", writer.Code, port.calls, writer.Body)
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
		server.docFind = nil
		writer = httptest.NewRecorder()
		docFindHTTPTestRouter(server.getDocFindPage).ServeHTTP(writer, docFindHTTPTestRequest(repo, hash, query.Encode()))
		if writer.Code != http.StatusServiceUnavailable || port.calls != 1 {
			test.Fatalf("missing find port fell back: status=%d calls=%d body=%s", writer.Code, port.calls, writer.Body)
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
	}
}
