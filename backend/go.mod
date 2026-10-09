// Module cxt-backend: server-only Go module for cxt (binaries cxtd and cxt-mcp).
//
// Complete-separation principle (the module boundary): the CLI and backend share no Go module.
// Each side owns its domain types (intentional duplication); schemas/ is the contract source of truth.
// External dependencies cover PostgreSQL, compression and maintained OIDC/JOSE and SAML/XML-signature verification.
module github.com/wnsdy95/cxthub/backend

go 1.26.9

require (
	github.com/beevik/etree v1.7.0
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/crewjam/saml v0.5.1
	github.com/jackc/pgx/v5 v5.11.0
	github.com/klauspost/compress v1.20.0
	github.com/mattermost/xml-roundtrip-validator v0.1.0
	github.com/russellhaering/goxmldsig v1.6.1
	golang.org/x/oauth2 v0.36.0
)

require (
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
