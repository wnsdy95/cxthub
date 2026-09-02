package mcp

import (
	"encoding/json"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) mcpMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST")
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "stateless MCP endpoint accepts POST only"})
}

func (s *Server) mcpPost(w http.ResponseWriter, r *http.Request) {
	user, err := s.requestUser(r, false)
	if err != nil {
		s.mcpUnauthorized(w)
		return
	}
	if !hasMediaType(r, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
			"jsonrpc": "2.0", "error": rpcError{Code: -32600, Message: "Content-Type application/json required"}, "id": nil,
		})
		return
	}
	var req rpcRequest
	if err := decodeSingleJSON(w, r, 1<<20, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"jsonrpc": "2.0", "error": rpcError{Code: -32600, Message: "invalid JSON-RPC request"}, "id": nil,
		})
		return
	}
	result, rpcErr := s.dispatch(r, user, req)
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID)}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) dispatch(r *http.Request, user domain.User, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		protocol := latestProtocol
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		switch params.ProtocolVersion {
		case "2024-11-05", "2025-03-26", latestProtocol:
			protocol = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cxthub-cloud", "version": "1"},
			"instructions":    serverInstructions,
		}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefinitions()}, nil
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil || call.Name == "" {
			return nil, &rpcError{Code: -32602, Message: "invalid tool call parameters"}
		}
		text, err := s.runTool(r.Context(), user, call.Name, call.Arguments)
		if err != nil {
			return toolText("Error: "+err.Error(), true), nil
		}
		return toolText(text, false), nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func toolText(text string, isError bool) map[string]any {
	result := map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
	if isError {
		result["isError"] = true
	}
	return result
}

func schema(properties map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func readAnnotations() map[string]any {
	return map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
}

func repositoryProperty() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "Repository as namespace/workspace/repository, its CXTHub URL, or an exact repo ID. Call repository_list first when unknown.",
	}
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name": "repository_list", "description": "List a bounded page of CXTHub repositories whose context the signed-in user may read.",
			"annotations": readAnnotations(), "inputSchema": schema(map[string]any{
				"query": map[string]any{"type": "string", "maxLength": 128, "description": "Optional case-insensitive repository path filter."},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum rows; default 50."},
			}),
		},
		{
			"name": "context_list", "description": "List committed agent-session context snapshots in one repository, newest first.",
			"annotations": readAnnotations(), "inputSchema": schema(map[string]any{
				"repository": repositoryProperty(),
				"branch":     map[string]any{"type": "string", "description": "Optional Git branch filter."},
				"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum rows; default 20."},
			}, "repository"),
		},
		{
			"name": "context_fetch", "description": "Fetch metadata, a bounded memory summary, and a bounded recent conversation tail for a ref.",
			"annotations": readAnnotations(), "inputSchema": schema(map[string]any{
				"repository": repositoryProperty(),
				"ref":        map[string]any{"type": "string", "description": "Branch, tag, full hash, short hash, or HEAD; defaults to the repository default branch."},
				"events":     map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Recent readable messages; default 12."},
			}, "repository"),
		},
		{
			"name": "memory_load", "description": "Load the bounded project-memory digest attached to a ref or its nearest reachable ancestor.",
			"annotations": readAnnotations(), "inputSchema": schema(map[string]any{
				"repository": repositoryProperty(), "ref": map[string]any{"type": "string", "description": "Branch, tag, or hash; defaults to the repository default branch."},
			}, "repository"),
		},
		{
			"name": "context_search", "description": "Search commit messages and readable conversation text inside one authorized repository.",
			"annotations": readAnnotations(), "inputSchema": schema(map[string]any{
				"repository": repositoryProperty(), "query": map[string]any{"type": "string", "minLength": 2, "maxLength": 256},
			}, "repository", "query"),
		},
	}
}
