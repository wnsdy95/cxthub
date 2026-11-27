//go:build postgres

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const repoAddressUsage = `usage: cxt-admin repo-address [--dir DIR] --list
       cxt-admin repo-address [--dir DIR] --from URL --to URL [--apply]
Lists the stored repositories with their addresses, or gives the repository
stored under --from the address its workspace is reached at now, --to.
Without --apply nothing is changed and the answer says what would be. Stop
the API service while it is applied. Requires CXT_POSTGRES_DSN with the
migration credential; directory: --dir, CXT_MIGRATIONS_DIR, schemas/db/migrations.`

type repoAddressOptions struct {
	directory string
	list      bool
	from, to  string
	apply     bool
}

func parseRepoAddressOptions(args []string, directory string) (repoAddressOptions, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		directory = "schemas/db/migrations"
	}
	var options repoAddressOptions
	flags := flag.NewFlagSet("repo-address", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.directory, "dir", directory, "migration bundle directory")
	flags.BoolVar(&options.list, "list", false, "list the stored repositories with their addresses")
	flags.StringVar(&options.from, "from", "", "the address the repository is stored under")
	flags.StringVar(&options.to, "to", "", "the address its workspace is reached at now")
	flags.BoolVar(&options.apply, "apply", false, "apply after reviewing what would change")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return repoAddressOptions{}, flag.ErrHelp
		}
		return repoAddressOptions{}, errors.New("invalid repo-address arguments\n" + repoAddressUsage)
	}
	switch {
	case flags.NArg() != 0:
		return repoAddressOptions{}, errors.New("unexpected repo-address arguments\n" + repoAddressUsage)
	case strings.TrimSpace(options.directory) == "":
		return repoAddressOptions{}, errors.New("repo-address --dir requires a nonempty migration directory")
	case options.list && (options.from != "" || options.to != "" || options.apply):
		return repoAddressOptions{}, errors.New("repo-address --list takes no other option\n" + repoAddressUsage)
	case !options.list && (strings.TrimSpace(options.from) == "" || strings.TrimSpace(options.to) == ""):
		return repoAddressOptions{}, errors.New("repo-address needs --list, or --from and --to\n" + repoAddressUsage)
	}
	return options, nil
}

// repoAddressListed is one stored repository: its identifier, its address and
// the workspace it belongs to.
type repoAddressListed struct {
	ID        domain.ContentHash `json:"id"`
	Address   string             `json:"address"`
	Workspace string             `json:"workspace,omitempty"`
	Path      string             `json:"path,omitempty"`
}

func runRepoAddress(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	options, err := parseRepoAddressOptions(args, os.Getenv("CXT_MIGRATIONS_DIR"))
	if errors.Is(err, flag.ErrHelp) {
		_, err := fmt.Fprintln(stderr, repoAddressUsage)
		return err
	}
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("CXT_POSTGRES_DSN"))
	if dsn == "" {
		return errors.New("CXT_POSTGRES_DSN is required for repo-address; PostgreSQL only")
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return errors.New("CXT_POSTGRES_DSN must be a valid PostgreSQL connection string")
	}
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		return errors.New("repo-address PostgreSQL connection failed; check CXT_POSTGRES_DSN")
	}
	defer storage.Close()
	// The move knows this release's tables: the database must be exactly it.
	if err := storage.VerifyMigrations(ctx, options.directory); err != nil {
		return fmt.Errorf("repo-address needs the database of this release; run cxt-admin migrate first: %w", err)
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if options.list {
		repos, err := storage.ListRepos(ctx, "default")
		if err != nil {
			return err
		}
		listed := make([]repoAddressListed, 0, len(repos))
		for _, repo := range repos {
			entry := repoAddressListed{ID: repo.ID, Address: repo.RemoteURL, Workspace: repo.RepositoryID}
			if repo.RepositoryID != "" {
				if workspace, err := storage.GetRepository(ctx, repo.RepositoryID); err == nil {
					entry.Path = workspace.OwnerUsername + "/" + workspace.Slug
				}
			}
			listed = append(listed, entry)
		}
		return encoder.Encode(listed)
	}
	moved, err := app.NewService(storage, storage, storage).MoveRepoAddress(ctx, options.from, options.to, options.apply)
	if err != nil {
		return fmt.Errorf("repo-address: nothing was changed: %w", err)
	}
	if err := encoder.Encode(moved); err != nil {
		return err
	}
	if !moved.Applied {
		_, err = fmt.Fprintln(stderr, "nothing was changed; run it again with --apply to move the repository")
		return err
	}
	_, err = fmt.Fprintln(stderr, "moved; start the API service again, then run on each computer: cxt setup "+moved.ToURL)
	return err
}
