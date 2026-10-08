package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ReadAgentHistoryPage always performs a fresh authorized read. It does not
// register repositories, reuse cached bodies, or fall back to full documents.
func (c *BackendClient) ReadAgentHistoryPage(ctx context.Context, repo string, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	var page domain.AgentHistoryPage
	if domain.ValidateContentHash(domain.ContentHash(repo)) != nil || domain.ValidateContentHash(hash) != nil {
		return page, domain.ErrHashMismatch
	}
	if err := domain.ValidateAgentHistoryPageRequest(req); err != nil {
		return page, err
	}
	q := url.Values{"before": {strconv.Itoa(req.Before)}, "limit": {strconv.Itoa(req.Limit)}, "max_bytes": {strconv.Itoa(req.MaxBytes)}}
	if req.DocIdentity != "" {
		q.Set("doc_identity", string(req.DocIdentity))
	}
	if req.CoveredByIdentity != "" {
		q.Set("covered_by_identity", string(req.CoveredByIdentity))
	}
	if req.CoveredBy != "" {
		q.Set("covered_by", string(req.CoveredBy))
	}
	if req.IncompleteTail != "" {
		q.Set("incomplete_tail", req.IncompleteTail)
	}
	var raw json.RawMessage
	// A separate transport ceiling accommodates JSON escaping and response
	// framing. The validator enforces the smaller requested event-body bound.
	err := c.doLimited(ctx, http.MethodGet, c.reposPath(repo)+"/docs/"+url.PathEscape(string(hash))+"/turns?"+q.Encode(), nil, &raw, 32<<20)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusUnprocessableEntity && httpErr.Code == "context_budget_exceeded" {
			return page, fmt.Errorf("%w: %s", domain.ErrContextBudgetExceeded, httpErr.Message)
		}
		return page, err
	}
	if err := historyPageWireShape(raw); err != nil {
		return page, err
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return domain.AgentHistoryPage{}, err
	}
	if err := domain.ValidateAgentHistoryPage(hash, req, page); err != nil {
		return domain.AgentHistoryPage{}, err
	}
	if err := verifyHistoryEventWire(raw, page); err != nil {
		return domain.AgentHistoryPage{}, err
	}
	return page, nil
}

// Event.MarshalJSON normalizes some fields (for example cross_replayable and
// nil tool output). Check that decoding did not discard or normalize corrupted
// wire values before accepting the typed body hash. Object ordering and JSON
// escaping are immaterial; unknown fields and changed union values are not.
func verifyHistoryEventWire(raw []byte, page domain.AgentHistoryPage) error {
	var body struct {
		Turns []struct {
			Events json.RawMessage `json:"events"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	normalize := func(raw []byte) ([]byte, error) {
		var value any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
	for i, turn := range page.Turns {
		wire, err := normalize(body.Turns[i].Events)
		if err != nil {
			return err
		}
		typed, err := json.Marshal(turn.Events)
		if err != nil {
			return err
		}
		typed, err = normalize(typed)
		if err != nil {
			return err
		}
		if !bytes.Equal(wire, typed) {
			return domain.ErrHashMismatch
		}
	}
	return nil
}

func historyPageWireShape(raw []byte) error {
	check := func(raw []byte, keys []string, optional ...string) (map[string]json.RawMessage, error) {
		fields := map[string]json.RawMessage{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
			return nil, domain.ErrHashMismatch
		}
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return nil, domain.ErrHashMismatch
			}
			if _, seen := fields[key]; seen {
				return nil, domain.ErrHashMismatch
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return nil, err
			}
			fields[key] = value
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			return nil, domain.ErrHashMismatch
		}
		if _, err := decoder.Token(); err != io.EOF {
			return nil, domain.ErrHashMismatch
		}
		for _, key := range keys {
			if value, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, domain.ErrHashMismatch
			}
		}
		count := len(keys)
		for _, key := range optional {
			if value, ok := fields[key]; ok {
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return nil, domain.ErrHashMismatch
				}
				count++
			}
		}
		if len(fields) != count {
			return nil, domain.ErrHashMismatch
		}
		return fields, nil
	}
	fields, err := check(raw, []string{"version", "hash", "provider", "session_id", "total", "before", "next_before", "covered", "turns"}, "omitted_tail", "doc_identity")
	if err != nil {
		return err
	}
	if tail, ok := fields["omitted_tail"]; ok {
		var version int
		if err := json.Unmarshal(fields["version"], &version); err != nil || version != domain.AgentHistoryProjectionVersion {
			return domain.ErrHashMismatch
		}
		if _, err := check(tail, []string{"start", "end", "reason"}); err != nil {
			return err
		}
	}
	var turns []json.RawMessage
	if err := json.Unmarshal(fields["turns"], &turns); err != nil {
		return err
	}
	for _, turn := range turns {
		if _, err := check(turn, []string{"start", "end", "hash", "events"}); err != nil {
			return err
		}
	}
	return nil
}
