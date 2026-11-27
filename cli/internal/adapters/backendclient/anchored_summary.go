package backendclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// PutAnchoredSummary stores the summary of one session at one commit with
// PUT /repos/{repoID}/anchored-sessions/{provider}/{sessionID}/summaries/{sha}.
// The same text again changes nothing on the server.
func (c *BackendClient) PutAnchoredSummary(ctx context.Context, repoID string, provider domain.ProviderKind, sessionID, commit string, body domain.AnchoredSummaryBody) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if !domain.ValidAnchoredSessionID(sessionID) || !fullCommitNumber(commit) {
		return fmt.Errorf("%w: a summary is stored for a session at a full commit number", domain.ErrInvalidRef)
	}
	path := c.reposPath(repoID) + "/anchored-sessions/" + url.PathEscape(string(provider)) + "/" + url.PathEscape(sessionID) + "/summaries/" + url.PathEscape(commit)
	var receipt struct {
		BodyHash string `json:"body_hash"`
		Changed  bool   `json:"changed"`
	}
	return c.doLimited(ctx, http.MethodPut, path, body, &receipt, 1<<20)
}

// ReadAnchoredMemory reads the summaries along a branch, the summary of the
// newest commit first, with GET /repos/{repoID}/anchored-memory. local names
// the newest commits of the branch as this folder has it; the server looks at
// them before what it read of the branch itself.
func (c *BackendClient) ReadAnchoredMemory(ctx context.Context, repoID, branch string, local []string, limit int) (domain.AnchoredMemory, error) {
	var memory domain.AnchoredMemory
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return memory, err
	}
	if len(local) > domain.AnchoredMemoryLocalCommits {
		local = local[:domain.AnchoredMemoryLocalCommits]
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if branch != "" {
		query.Set("branch", branch)
	}
	for _, commit := range local {
		if !fullCommitNumber(commit) {
			return memory, fmt.Errorf("%w: a commit of this folder is named by its full number", domain.ErrInvalidRef)
		}
		query.Add("local", commit)
	}
	err := c.doLimited(ctx, http.MethodGet, c.reposPath(repoID)+"/anchored-memory?"+query.Encode(), nil, &memory, 4<<20)
	return memory, err
}

// fullCommitNumber reports whether sha is a whole SHA-1 or SHA-256 commit
// number in lower case.
func fullCommitNumber(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, digit := range sha {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
}
