package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitHubPRHistory = (*Client)(nil)

const prHeadHistoryQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String){
  repository(owner:$owner,name:$name){
    databaseId
    pullRequest(number:$number){
      id fullDatabaseId number baseRefName headRefName headRefOid merged mergedAt
      headRepository{databaseId}
      mergeCommit{oid}
      timelineItems(first:100,after:$after,itemTypes:[HEAD_REF_FORCE_PUSHED_EVENT]){
        pageInfo{hasNextPage endCursor}
        nodes{
          __typename
          ... on HeadRefForcePushedEvent{id createdAt beforeCommit{oid} afterCommit{oid}}
        }
      }
    }
  }
}`

type githubHistoryCommit struct {
	OID string `json:"oid"`
}

type githubHistoryRepository struct {
	ID int64 `json:"databaseId"`
}

func (c *Client) PullRequestHeadHistory(ctx context.Context, installation int64, repository domain.GitHubRepository, number int, cursor string) (domain.GitHubPRHistoryPage, error) {
	var page domain.GitHubPRHistoryPage
	if installation <= 0 || repository.ID <= 0 || !validRepositoryName(repository.FullName) || number <= 0 || len(cursor) > 4096 {
		return page, domain.ErrValidation
	}
	var response struct {
		Errors []json.RawMessage `json:"errors"`
		Data   *struct {
			Repository *struct {
				ID          int64 `json:"databaseId"`
				PullRequest *struct {
					ID             string                   `json:"id"`
					DatabaseID     json.Number              `json:"fullDatabaseId"`
					Number         int                      `json:"number"`
					BaseBranch     string                   `json:"baseRefName"`
					HeadBranch     string                   `json:"headRefName"`
					HeadSHA        string                   `json:"headRefOid"`
					Merged         *bool                    `json:"merged"`
					MergedAt       *time.Time               `json:"mergedAt"`
					HeadRepository *githubHistoryRepository `json:"headRepository"`
					MergeCommit    *githubHistoryCommit     `json:"mergeCommit"`
					Timeline       *struct {
						PageInfo *struct {
							HasNextPage *bool  `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							Kind      string               `json:"__typename"`
							ID        string               `json:"id"`
							CreatedAt time.Time            `json:"createdAt"`
							Before    *githubHistoryCommit `json:"beforeCommit"`
							After     *githubHistoryCommit `json:"afterCommit"`
						} `json:"nodes"`
					} `json:"timelineItems"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	token, err := c.InstallationToken(ctx, installation)
	if err != nil {
		return page, err
	}
	parts := strings.Split(repository.FullName, "/")
	variables := map[string]any{"owner": parts[0], "name": parts[1], "number": number, "after": nil}
	if cursor != "" {
		variables["after"] = cursor
	}
	err = c.request(ctx, http.MethodPost, c.api+"/graphql", token, "installation:"+strconv.FormatInt(installation, 10), map[string]any{"query": prHeadHistoryQuery, "variables": variables}, &response)
	if err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			c.Invalidate(installation)
		}
		return page, err
	}
	if len(response.Errors) != 0 || response.Data == nil || response.Data.Repository == nil {
		return page, domain.ErrIntegrity
	}
	result := response.Data.Repository
	pr := result.PullRequest
	if result.ID != repository.ID || pr == nil || pr.Number != number || pr.Merged == nil || pr.HeadRepository == nil || pr.Timeline == nil || pr.Timeline.Nodes == nil || pr.Timeline.PageInfo == nil || pr.Timeline.PageInfo.HasNextPage == nil {
		return page, domain.ErrIntegrity
	}
	prDatabaseID, err := pr.DatabaseID.Int64()
	if err != nil {
		return page, domain.ErrIntegrity
	}
	page = domain.GitHubPRHistoryPage{RepositoryID: result.ID, HeadRepositoryID: pr.HeadRepository.ID, PullRequestID: pr.ID, PullRequestDatabaseID: prDatabaseID, Number: pr.Number, BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, Merged: *pr.Merged, Rewrites: []domain.GitHubHeadRewrite{}, EndCursor: pr.Timeline.PageInfo.EndCursor, HasNextPage: *pr.Timeline.PageInfo.HasNextPage}
	if page.HasNextPage && (page.EndCursor == cursor || len(pr.Timeline.Nodes) == 0) {
		return domain.GitHubPRHistoryPage{}, domain.ErrIntegrity
	}
	if page.Merged {
		if pr.MergeCommit == nil || pr.MergedAt == nil {
			return domain.GitHubPRHistoryPage{}, domain.ErrIntegrity
		}
		page.MergeSHA, page.MergedAt = pr.MergeCommit.OID, *pr.MergedAt
	} else if pr.MergedAt != nil {
		return domain.GitHubPRHistoryPage{}, domain.ErrIntegrity
	}
	for _, event := range pr.Timeline.Nodes {
		if event.Kind != "HeadRefForcePushedEvent" {
			return domain.GitHubPRHistoryPage{}, domain.ErrIntegrity
		}
		rewrite := domain.GitHubHeadRewrite{ID: event.ID, CreatedAt: event.CreatedAt}
		if event.Before != nil {
			rewrite.Before = event.Before.OID
		}
		if event.After != nil {
			rewrite.After = event.After.OID
		}
		page.Rewrites = append(page.Rewrites, rewrite)
	}
	if err := page.Validate(); err != nil {
		return domain.GitHubPRHistoryPage{}, err
	}
	return page, nil
}
