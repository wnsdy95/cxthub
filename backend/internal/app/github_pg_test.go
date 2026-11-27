//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGGitHubSyncContract(t *testing.T) {
	core, st, _ := collaborationPG(t)
	f := makeTeamFixture(t, st)
	remote := &githubFake{proof: outbound.GitHubAuthorization{Identity: domain.GitHubIdentity{ExternalID: time.Now().UnixMicro(), Login: "operator"}, Installation: domain.GitHubInstallation{ID: time.Now().UnixMicro(), AccountID: 456, Kind: "Organization", Login: "acme"}}}
	g := NewGitHubConnections(f.identity, core, st, remote)
	runGitHubSyncContract(t, g, f, remote)
	// Every write shares the identity transaction; aborted mappings never leak.
	ctx := context.Background()
	sentinel := errors.New("rollback")
	err := st.WithinIdentity(ctx, func(tx context.Context) error {
		c, e := st.GetGitHubConnection(tx, f.organization.NamespaceID)
		if e != nil {
			return e
		}
		c.Enabled = true
		if e = st.PutGitHubConnection(tx, c); e != nil {
			return e
		}
		if e = st.ReplaceGitHubTeamMembers(tx, c.NamespaceID, []domain.GitHubTeamMember{}); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	c, err := st.GetGitHubConnection(ctx, f.organization.NamespaceID)
	if err != nil || c.Enabled {
		t.Fatalf("rollback leaked: %+v %v", c, err)
	}
}
