# cxthub Makefile
#
# Monorepo build/test entry point. Manages two Go modules and a web frontend:
#   cli/          → binary cxt  (local CLI — the context layer beside Git)
#   backend/      → binary cxtd (shared context server)
#   frontend/web/ → React+Vite web UI (cxthub website)
#   frontend/     → framework-independent clean layered core (contract stub)

BINARY_DIR := bin
GO         := go
LDFLAGS    := -ldflags "-s -w"

.DEFAULT_GOAL := help

.PHONY: help
help: ## List available make targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build cxt + cxtd binaries to ./bin/
	@mkdir -p $(BINARY_DIR)
	cd cli && $(GO) build $(LDFLAGS) -o ../$(BINARY_DIR)/cxt ./cmd/cxt
	cd backend && $(GO) build $(LDFLAGS) -o ../$(BINARY_DIR)/cxtd ./cmd/cxtd
	@echo "[build] complete: $(BINARY_DIR)/cxt, $(BINARY_DIR)/cxtd"

.PHONY: install
install: ## Install cxt + cxtd using go install (GOPATH/bin)
	cd cli && $(GO) install ./cmd/cxt
	cd backend && $(GO) install ./cmd/cxtd

.PHONY: test
test: ## Run unit tests for both Go modules
	cd cli && $(GO) test ./...
	cd backend && $(GO) test ./...

.PHONY: test-postgres
test-postgres: ## Compile with the postgres tag and run the PG smoke test when configured
	cd backend && $(GO) build -tags postgres ./...
	cd backend && $(GO) test -tags postgres ./internal/adapters/store/ -run TestPGSmoke

.PHONY: e2e
e2e: ## Run real-binary E2E tests against the FS store (same as CI)
	bash scripts/e2e.sh

.PHONY: e2e-sync
e2e-sync: ## Run advanced sync E2E: grafts, memory inheritance, boundaries, and fetch-only
	bash scripts/e2e-sync.sh

.PHONY: typecheck
typecheck: ## Strictly type-check the web UI and framework-independent TypeScript core
	cd frontend/web && npx tsc --noEmit
	cd frontend && npx tsc --noEmit

.PHONY: web
web: ## Web UI production build (frontend/web/dist)
	cd frontend/web && npx tsc --noEmit && npx vite build

.PHONY: dev-server
dev-server: ## Start cxtd locally with development authentication
	cd backend && $(GO) run ./cmd/cxtd serve --addr 127.0.0.1:8907 --data ../cxt-data

.PHONY: dev-web
dev-web: ## Start Vite on :5173 with /api proxied to :8907
	cd frontend/web && npm run dev

.PHONY: lint
lint: ## go vet (two modules)
	cd cli && $(GO) vet ./...
	cd backend && $(GO) vet ./...

.PHONY: fmt
fmt: ## Format both Go modules with gofmt
	cd cli && $(GO) fmt ./...
	cd backend && $(GO) fmt ./...

.PHONY: tidy
tidy: ## go mod tidy (two modules)
	cd cli && $(GO) mod tidy
	cd backend && $(GO) mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BINARY_DIR) frontend/web/dist frontend/dist

.PHONY: all
all: build test typecheck ## Build, test, and type-check the project

.PHONY: public-check
public-check: ## Check public-tree policy and secrets, excluding existing Git history
	bash scripts/public-preflight.sh tree

.PHONY: public-check-full
public-check-full: ## Check the public tree and complete history in the new public repository
	bash scripts/public-preflight.sh full

.PHONY: deploy-check
deploy-check: ## Fast static check for deployment configuration (no external changes)
	bash scripts/deploy-preflight.sh static

.PHONY: deploy-check-full
deploy-check-full: ## Full test/PG migration/Docker build for deployment (no push)
	bash scripts/deploy-preflight.sh full

.PHONY: deploy-check-accounts
deploy-check-accounts: ## Read-only check for GCP/Vercel account credentials
	bash scripts/deploy-preflight.sh accounts

.PHONY: deploy-check-ready
deploy-check-ready: ## Run final read-only checks for bootstrap resources and the image
	bash scripts/deploy-preflight.sh ready

.PHONY: webhook-check webhook-apply
webhook-check: ## Verify the production signed GitHub PR webhook without changes
	bash scripts/github-webhook.sh check

webhook-apply: ## Create or reconcile the production signed GitHub PR webhook
	bash scripts/github-webhook.sh apply
