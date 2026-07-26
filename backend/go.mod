// Module cxt-backend: server-only Go module for cxt (binary cxtd).
//
// Complete-separation principle (docs/_ARCHITECTURE-R2.md §5): the CLI and backend share no Go module.
// Each side owns its domain types (intentional duplication); schemas/ is the contract source of truth.
// External dependencies are limited to the PostgreSQL adapter (pgx) and at-rest compression.
module github.com/wnsdy95/cxthub/backend

go 1.26.5

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/klauspost/compress v1.19.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
