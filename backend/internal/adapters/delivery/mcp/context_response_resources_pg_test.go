//go:build postgres

package mcp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func requireContextResponseLocalPG(test *testing.T) {
	test.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable local CXT_TEST_DSN")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("invalid local PostgreSQL test configuration")
	}
	local := func(host string) bool {
		address := net.ParseIP(host)
		return strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") || address != nil && address.IsLoopback()
	}
	if !local(config.ConnConfig.Host) {
		test.Fatal("context response tests require local PostgreSQL")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !local(fallback.Host) {
			test.Fatal("context response tests require local PostgreSQL fallbacks")
		}
	}
}

func TestPGContextResponseColdWarmProductionParity(test *testing.T) {
	requireContextResponseLocalPG(test)
	fixture := newPRContributionParityFixture(test)
	queries, err := fixture.service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
	server, err := NewServer(fixture.service, identity, fixture.storage, "https://parity.example.test")
	if err != nil {
		test.Fatal(err)
	}
	if err := server.SetContextQuery(queries, inbound.ContextQueryPinnedSnapshots); err != nil {
		test.Fatal(err)
	}
	fixture.mcpHandler = server.Handler()
	before := fixture.repositoryState(test)
	for _, mode := range []string{"", "segments"} {
		test.Run("mode="+mode, func(test *testing.T) {
			args := toolArgs{Repository: string(fixture.repo.ID), Scope: "current", Branch: "main", Position: "main", Mode: mode, Limit: 1}
			coldResponse, cold := fixture.toolRequest(test, fixture.mcpToken, "context_list", args)
			warmResponse, warm := fixture.toolRequest(test, fixture.mcpToken, "context_list", args)
			for _, response := range []prParityRPCResponse{cold, warm} {
				if response.Error != nil || response.Result.IsError || len(response.Result.Content) != 1 || response.Result.Content[0].Type != "text" {
					test.Fatal("production context response failed", response)
				}
			}
			if coldResponse.Code != http.StatusOK || warmResponse.Code != http.StatusOK || !reflect.DeepEqual(cold, warm) {
				test.Fatal("cold/warm transport responses differ")
			}
			selection := domain.ContextSelection{Scope: args.Scope, Branch: args.Branch, Position: args.Position}
			if mode == "segments" {
				selection.SegmentLimit = args.Limit
			}
			view, err := queries.QueryContext(fixture.ctx, fixture.repo.ID, selection)
			if err != nil {
				test.Fatal(err)
			}
			warmView, err := queries.QueryContext(fixture.ctx, fixture.repo.ID, selection)
			if err != nil || !reflect.DeepEqual(view, warmView) || len(view.Snapshots) == 0 || view.Position != fixture.current.ID || view.StateHash == "" {
				test.Fatal("production context generation changed", err)
			}
			var expected string
			if mode == "segments" {
				if view.Segments == nil || len(view.Segments.Entries) == 0 || view.Segments.PageHash == "" {
					test.Fatal("production segment coverage is empty")
				}
				if !strings.Contains(cold.Result.Content[0].Text, string(view.Segments.PageHash)) {
					test.Fatal("transport lost the production segment page hash")
				}
				expected, err = server.contextSegmentPage(fixture.ctx, fixture.repo, args)
			} else {
				expected, err = server.contextPage(fixture.ctx, fixture.repo, args)
			}
			if err != nil || expected != cold.Result.Content[0].Text || !strings.Contains(expected, string(view.StateHash)) {
				test.Fatal("JSON-RPC content differs from the production context page or generation", err)
			}
		})
	}
	if !reflect.DeepEqual(before, fixture.repositoryState(test)) {
		test.Fatal("context reads changed repository business state")
	}
}

func TestPGContextResponseWarmCacheRequiresCurrentAuthority(test *testing.T) {
	requireContextResponseLocalPG(test)
	fixture := newPRContributionParityFixture(test)
	before := fixture.repositoryState(test)
	for _, mode := range []string{"", "segments"} {
		args := toolArgs{Scope: "current", Branch: "main", Position: "main", Mode: mode, Limit: 1}
		for read := 0; read < 2; read++ {
			var page map[string]any
			fixture.readTool(test, "context_list", args, &page)
		}
		response, envelope := fixture.toolRequest(test, fixture.outsiderMCP, "context_list", args)
		if response.Code != http.StatusOK || envelope.Error != nil || !envelope.Result.IsError || !strings.Contains(response.Body.String(), "not authorized") || strings.Contains(response.Body.String(), string(fixture.current.ID)) {
			test.Fatal("warm context cache bypassed current repository permission")
		}
	}
	identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
	if err := identity.RevokeMCPToken(fixture.ctx, fixture.mcpToken, "contribution-parity-client"); err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"", "segments"} {
		args := toolArgs{Repository: string(fixture.repo.ID), Scope: "current", Branch: "main", Position: "main", Mode: mode, Limit: 1}
		request := documentEventRequest(test, fixture.ctx, fixture.mcpToken, "context_list", args)
		response := httptest.NewRecorder()
		fixture.mcpHandler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), string(fixture.current.ID)) {
			test.Fatal("revoked credential retained warm context access")
		}
	}
	if !reflect.DeepEqual(before, fixture.repositoryState(test)) {
		test.Fatal("denied context reads changed repository business state")
	}
}

func TestPGContextResponseOwnsActualQueryAndProtocolWrite(test *testing.T) {
	for _, mode := range []string{"", "segments"} {
		for _, outcome := range []string{"complete", "cancel", "write failure"} {
			test.Run(mode+"/"+outcome, func(test *testing.T) {
				requireContextResponseLocalPG(test)
				fixture := newPRContributionParityFixture(test)
				queries, err := fixture.service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
				if err != nil {
					test.Fatal(err)
				}
				identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
				server, err := NewServer(fixture.service, identity, fixture.storage, "https://parity.example.test")
				if err != nil {
					test.Fatal(err)
				}
				if err := server.SetContextQuery(queries, inbound.ContextQueryPinnedSnapshots); err != nil {
					test.Fatal(err)
				}
				before := fixture.repositoryState(test)
				_, releaseOther, err := app.AcquireResources(context.Background(), app.ResourceInteractive)
				if err != nil {
					test.Fatal(err)
				}
				defer releaseOther()
				requestContext, cancel := context.WithCancel(fixture.ctx)
				defer cancel()
				writes := 0
				writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() {
					writes++
					contextResponseBulkHeld(test)
					if outcome == "cancel" {
						cancel()
						contextResponseBulkHeld(test)
					}
				}}
				if outcome == "write failure" {
					writer.cause = io.ErrClosedPipe
				}
				request := documentEventRequest(test, requestContext, fixture.mcpToken, "context_list", toolArgs{Repository: string(fixture.repo.ID), Scope: "current", Branch: "main", Position: "main", Mode: mode, Limit: 1})
				server.Handler().ServeHTTP(writer, request)
				if writes != 1 || writer.Code != http.StatusOK || len(writer.deadlines) != 2 {
					test.Fatal("actual PG query did not reach the resource-owned protocol writer", writes, writer.Code, writer.deadlines)
				}
				contextResponseBulkAvailable(test)
				if !reflect.DeepEqual(before, fixture.repositoryState(test)) {
					test.Fatal("response lifetime changed repository business state")
				}
			})
		}
	}
}

func TestPGContextResponseReadyReadDoesNotWaitForBulk(test *testing.T) {
	for _, mode := range []string{"", "segments"} {
		test.Run(mode, func(test *testing.T) {
			requireContextResponseLocalPG(test)
			fixture := newPRContributionParityFixture(test)
			queries, err := fixture.service.ContextQueriesForProfile(inbound.ContextQueryPinnedSnapshots)
			if err != nil {
				test.Fatal(err)
			}
			identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
			server, err := NewServer(fixture.service, identity, fixture.storage, "https://parity.example.test")
			if err != nil {
				test.Fatal(err)
			}
			if err := server.SetContextQuery(queries, inbound.ContextQueryPinnedSnapshots); err != nil {
				test.Fatal(err)
			}
			fixture.mcpHandler = server.Handler()
			args := toolArgs{Repository: string(fixture.repo.ID), Scope: "current", Branch: "main", Position: "main", Mode: mode, Limit: 1}
			_, warm := fixture.toolRequest(test, fixture.mcpToken, "context_list", args)
			if warm.Error != nil || warm.Result.IsError {
				test.Fatal("could not prepare actual context metadata", warm)
			}
			_, releaseOther, err := app.AcquireResources(context.Background(), app.ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer releaseOther()
			ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
			defer cancel()
			writes := 0
			writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() {
				writes++
				contextResponseReadHeld(test)
			}}
			server.Handler().ServeHTTP(writer, documentEventRequest(test, ctx, fixture.mcpToken, "context_list", args))
			if writes != 1 || writer.Code != http.StatusOK || strings.Contains(writer.Body.String(), `"isError":true`) || ctx.Err() != nil {
				test.Fatal("ready actual PG context metadata waited for unrelated bulk work", writes, writer.Code, writer.Body, ctx.Err())
			}
			releaseOther()
			contextResponseBulkAvailable(test)
		})
	}
}
