package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// protocolCall hands one request to the server as a signed-in caller's
// request would arrive, past the transport checks.
func protocolCall(server *Server, user domain.User, method, params string) (any, *rpcError) {
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(systemTestContext())
	call := rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method}
	if params != "" {
		call.Params = json.RawMessage(params)
	}
	return server.dispatch(request, user, call)
}

// protocolJSON is an answer as a client reads it.
func protocolJSON(t *testing.T, answer any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("an answer that is not an object: %s", raw)
	}
	return decoded
}

// A client says which protocol version it speaks. The server answers with
// that version when it speaks it too, and with its newest one otherwise.
func TestInitializeAnswersTheVersionTheServerWillSpeak(t *testing.T) {
	server, _, user, _ := sessionToolFixture(t)
	for name, s := range map[string]struct{ params, want string }{
		"a client that names no version":        {``, latestProtocol},
		"empty parameters":                      {`{}`, latestProtocol},
		"the first version":                     {`{"protocolVersion":"2024-11-05"}`, "2024-11-05"},
		"the second version":                    {`{"protocolVersion":"2025-03-26"}`, "2025-03-26"},
		"the newest version":                    {`{"protocolVersion":"` + latestProtocol + `"}`, latestProtocol},
		"a version from the future":             {`{"protocolVersion":"2099-01-01"}`, latestProtocol},
		"a version that is not text":            {`{"protocolVersion":7}`, latestProtocol},
		"parameters that are not JSON":          {`not json`, latestProtocol},
		"a version with other fields beside it": {`{"clientInfo":{"name":"x"},"protocolVersion":"2024-11-05","capabilities":{}}`, "2024-11-05"},
	} {
		answer, failure := protocolCall(server, user, "initialize", s.params)
		if failure != nil {
			t.Errorf("%s: %+v", name, failure)
			continue
		}
		got := protocolJSON(t, answer)
		want := map[string]any{
			"protocolVersion": s.want,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cxthub-cloud", "version": "1"},
			"instructions":    serverInstructions,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v", name, got)
		}
	}
	if latestProtocol != "2025-06-18" || !strings.Contains(serverInstructions, "read-only") {
		t.Fatalf("the newest version or the instructions changed: %s", latestProtocol)
	}
}

// What the server answers to each kind of request besides a tool call.
func TestProtocolMethodsBesideToolCalls(t *testing.T) {
	server, queries, user, _ := sessionToolFixture(t)
	// A notification has no answer.
	for _, method := range []string{"notifications/initialized", "notifications/cancelled"} {
		if answer, failure := protocolCall(server, user, method, `{"requestId":1}`); answer != nil || failure != nil {
			t.Errorf("%s was answered: %v %+v", method, answer, failure)
		}
	}
	answer, failure := protocolCall(server, user, "ping", ``)
	if failure != nil || len(protocolJSON(t, answer)) != 0 {
		t.Errorf("ping: %v %+v", answer, failure)
	}
	answer, failure = protocolCall(server, user, "tools/list", ``)
	listed, _ := protocolJSON(t, answer)["tools"].([]any)
	if failure != nil || len(listed) != len(toolDefinitions()) || len(listed) == 0 {
		t.Fatalf("tools/list: %d tools %+v", len(listed), failure)
	}
	declared := protocolJSON(t, map[string]any{"tools": toolDefinitions()})["tools"]
	if !reflect.DeepEqual(any(listed), declared) {
		t.Errorf("tools/list does not list the tools as they are declared")
	}
	// Anything else is not a method of this server, and says which one was asked for.
	for _, method := range []string{"resources/list", "prompts/list", "tools/run", "Initialize", "tools/call/", "completion/complete"} {
		answer, failure := protocolCall(server, user, method, `{}`)
		if answer != nil || failure == nil || failure.Code != -32601 || failure.Message != "method not found: "+method {
			t.Errorf("%s: %v %+v", method, answer, failure)
		}
	}
	if len(queries.asked) != 0 {
		t.Errorf("a request that is not a tool call read something: %v", queries.asked)
	}
}

// A tool call that cannot be understood is a protocol error. A tool that
// fails is an answer: text the model reads, marked as an error.
func TestToolCallAnswers(t *testing.T) {
	server, queries, user, repo := sessionToolFixture(t)
	for name, params := range map[string]string{
		"no parameters":            ``,
		"parameters that are null": `null`,
		"parameters without name":  `{"arguments":{}}`,
		"an empty name":            `{"name":"","arguments":{}}`,
		"a name that is not text":  `{"name":7}`,
		"not JSON":                 `{"name":"session_list"`,
		// The name reads as a tool, but the parameters as a whole do not read.
		"a name given twice, once not as text": `{"name":"session_list","name":7,"arguments":{}}`,
	} {
		answer, failure := protocolCall(server, user, "tools/call", params)
		if answer != nil || failure == nil || failure.Code != -32602 || failure.Message != "invalid tool call parameters" {
			t.Errorf("%s: %v %+v", name, answer, failure)
		}
	}
	if len(queries.asked) != 0 {
		t.Fatalf("a call that was not understood read something: %v", queries.asked)
	}

	text := func(name, params string) (string, bool) {
		t.Helper()
		answer, failure := protocolCall(server, user, "tools/call", params)
		if failure != nil {
			t.Fatalf("%s: a tool's failure became a protocol error: %+v", name, failure)
		}
		decoded := protocolJSON(t, answer)
		content, _ := decoded["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("%s: content %v", name, decoded)
		}
		item, _ := content[0].(map[string]any)
		words, _ := item["text"].(string)
		if item["type"] != "text" || len(item) != 2 {
			t.Fatalf("%s: content item %v", name, item)
		}
		failed, marked := decoded["isError"]
		// An answer that is not an error does not carry the mark at all.
		if marked && failed != true || len(decoded) != 1 && !marked || len(decoded) > 2 {
			t.Fatalf("%s: answer %v", name, decoded)
		}
		return words, marked
	}
	const private = "synthetic-private-detail"
	listing := `{"name":"session_list","arguments":{"repository":"` + string(repo.ID) + `"}}`
	for name, s := range map[string]struct {
		params string
		fail   error
		want   string
	}{
		"a tool this server does not have":  {`{"name":"session_delete","arguments":{}}`, nil, `Error: unknown tool "session_delete"`},
		"arguments that are not an object":  {`{"name":"session_list","arguments":[1]}`, nil, "Error: invalid arguments"},
		"a session tool without repository": {`{"name":"session_list","arguments":{}}`, nil, "Error: repository is required; call repository_list first"},
		"a session tool without arguments":  {`{"name":"session_list"}`, nil, "Error: repository is required; call repository_list first"},
		"a read that fails":                 {listing, domain.ErrNotFound, "Error: " + domain.ErrNotFound.Error()},
		// What went wrong with the audit is not the model's to read.
		"an audit that cannot be written": {listing, errors.Join(domain.ErrBreakGlassAuditUnavailable, errors.New(private)), "Error: " + domain.ErrBreakGlassAuditUnavailable.Error()},
	} {
		queries.err = s.fail
		words, marked := text(name, s.params)
		if !marked || words != s.want || strings.Contains(words, private) {
			t.Errorf("%s: %q (marked as an error: %t)", name, words, marked)
		}
	}
	queries.err = nil
	words, marked := text("a tool that works", listing)
	var page struct {
		Notice string `json:"notice"`
	}
	if marked || json.Unmarshal([]byte(words), &page) != nil || page.Notice != sessionNotice {
		t.Errorf("a tool that works: %q (marked as an error: %t)", words, marked)
	}
}
