// Command cxt-mcp serves remote read-only MCP and OAuth independently of cxtd.
// Both binaries use the same PostgreSQL database and public Vercel origin.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/memorypreparation"
	"github.com/wnsdy95/cxthub/backend/internal/mcpserver"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: cxt-mcp serve [--addr :8908]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "cxt-mcp:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, args []string) error {
	if err := serverruntime.LoadEnv(); err != nil {
		return err
	}
	addr := os.Getenv("CXT_MCP_ADDR")
	if addr == "" {
		addr = ":8908"
	}
	flags := flag.NewFlagSet("cxt-mcp serve", flag.ContinueOnError)
	flags.StringVar(&addr, "addr", addr, "HTTP bind address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	// No FS fallback, including on loopback: API and MCP are separate processes
	// and must share transactional database state rather than a filesystem cache.
	if os.Getenv("CXT_POSTGRES_DSN") == "" {
		return fmt.Errorf("CXT_POSTGRES_DSN is required for the independent MCP server")
	}
	if os.Getenv("CXT_PUBLIC_URL") == "" {
		return fmt.Errorf("CXT_PUBLIC_URL must match the API's public frontend origin")
	}
	resourceOrigin := os.Getenv("CXT_MCP_PUBLIC_URL")
	if resourceOrigin == "" {
		resourceOrigin = os.Getenv("CXT_PUBLIC_URL")
	}
	resourceOrigin, err := serverruntime.PublicOrigin(addr, resourceOrigin)
	if err != nil {
		return fmt.Errorf("CXT_MCP_PUBLIC_URL: %w", err)
	}
	preparationOrigin, err := readPreparationAPIOrigin(os.Getenv("CXT_READ_PREPARATION_API_ORIGIN"), os.Getenv("CXT_MEMORY_PREPARATION_API_ORIGIN"))
	if err != nil {
		return err
	}
	requester, conversation, err := readPreparationRequesters(os.Getenv("CXT_MEMORY_READ_REPOS"), preparationOrigin)
	if err != nil {
		return err
	}
	runtime, err := serverruntime.OpenMCP(ctx, addr, "", true)
	if err != nil {
		return err
	}
	defer runtime.Close()
	ctx = runtime.ProcessContext(ctx)
	handler, err := mcpserver.HandlerWithReadPreparation(runtime, resourceOrigin, requester, conversation)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cxt-mcp: listening on %s (store=postgres, auth=%s)\n", addr, runtime.AuthMode)
	return serverruntime.Serve(ctx, addr, handler)
}

func memoryPreparationRequester(readRepos, origin string) (outbound.MemoryPreparationRequester, error) {
	memory, _, err := readPreparationRequesters(readRepos, origin)
	return memory, err
}

func readPreparationAPIOrigin(current, legacy string) (string, error) {
	if current != "" && legacy != "" && current != legacy {
		return "", fmt.Errorf("CXT_READ_PREPARATION_API_ORIGIN conflicts with the legacy memory preparation origin")
	}
	if current != "" {
		return current, nil
	}
	return legacy, nil
}

func readPreparationRequesters(readRepos, origin string) (outbound.MemoryPreparationRequester, outbound.ConversationFindPreparationRequester, error) {
	if origin == "" {
		if readRepos != "" {
			return nil, nil, fmt.Errorf("CXT_READ_PREPARATION_API_ORIGIN is required when MCP prepared memory reads are enabled")
		}
		return nil, nil, nil
	}
	client, err := memorypreparation.NewClient(origin)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid CXT_READ_PREPARATION_API_ORIGIN")
	}
	return client, client, nil
}
