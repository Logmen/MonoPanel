VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X monopanel/internal/buildinfo.Version=$(VERSION) -X monopanel/internal/buildinfo.Commit=$(COMMIT) -X monopanel/internal/buildinfo.Date=$(DATE)
GO      ?= go
ARCH    ?= amd64
DEV_HOST ?= ubuntu@185.253.8.5
DEV_SSH  ?= ssh -i ~/.ssh/monopanel-dev -o IdentitiesOnly=yes
GOLANGCI_VERSION ?= v2.13.2

.PHONY: build build-arm64 test test-race test-short cover cover-html lint vet fmt fmt-check check web web-check web-stub e2e deb rpm deploy-dev clean help

help: ## Show the available targets
	@grep -hE '^[a-z0-9-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*## "} {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# web/build is generated (and git-ignored) but go:embed needs it to exist;
# this stubs it with the placeholder page so go vet/test/build work on a fresh clone.
web-stub:
	@[ -f web/build/index.html ] || (mkdir -p web/build && cp web/placeholder/index.html web/build/index.html && echo "web/build missing: embedded placeholder UI (run make web)")

build: web-stub ## Build the panel binary into dist/
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/monopanel ./cmd/monopanel

build-arm64:
	$(MAKE) build ARCH=arm64

test: web-stub ## Run the unit tests (a couple of seconds)
	$(GO) test ./...

test-short: web-stub ## Run only the fast tests (-short)
	$(GO) test -short ./...

test-race: web-stub ## Run the tests with the race detector
	$(GO) test -race -count=1 ./...

cover: web-stub ## Run the tests and print coverage per package and total
	$(GO) test -coverprofile=dist/coverage.out -covermode=atomic ./... >/dev/null
	@$(GO) tool cover -func=dist/coverage.out | tail -1
	@$(GO) test -cover ./... 2>/dev/null | grep -v 'no test files' | \
		awk '{pkg = ""; for (i = 1; i <= NF; i++) { if ($$i ~ /^monopanel\//) pkg = $$i; if ($$i == "coverage:") cov = $$(i + 1) } if (pkg != "") print pkg, cov}' | column -t

cover-html: cover ## Open the coverage report in a browser
	$(GO) tool cover -html=dist/coverage.out

vet: web-stub
	$(GO) vet ./...

lint: web-stub ## Run golangci-lint (installs it into dist/ when missing)
	@command -v golangci-lint >/dev/null 2>&1 || [ -x dist/golangci-lint ] || \
		(echo "installing golangci-lint $(GOLANGCI_VERSION) into dist/" && \
		 GOBIN=$(CURDIR)/dist $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION))
	@$$(command -v golangci-lint || echo dist/golangci-lint) run

fmt: ## Format the Go sources
	gofmt -l -w cmd internal e2e templates

fmt-check:
	@out=$$(gofmt -l cmd internal e2e); [ -z "$$out" ] || (echo "not gofmt'ed:"; echo "$$out"; exit 1)

web-check: ## Type-check the web UI
	cd web && pnpm install --frozen-lockfile && node_modules/.bin/svelte-kit sync && node_modules/.bin/svelte-check --tsconfig ./tsconfig.json

check: fmt-check vet lint test ## Everything CI runs, locally

web: ## Build the web UI into web/build
	cd web && pnpm install --frozen-lockfile && node_modules/.bin/svelte-kit sync && node_modules/.bin/vite build

# End-to-end against a real panel: creates a user, a site with a preset and a
# database, checks that nginx and PHP actually answer, then removes them again.
# With HOST=<ssh alias> a token is minted over ssh and revoked afterwards;
# otherwise MONOPANEL_URL plus a token or login/password come from the caller.
e2e: web-stub ## Run end-to-end tests against a live panel (HOST=toolkit, or MONOPANEL_* in the environment)
ifdef HOST
	@set -e; \
	token=$$(ssh $(HOST) 'mp token create --name e2e --json' | $(GO) run ./scripts/jsonfield token); \
	id=$$(ssh $(HOST) 'mp token list --json' | $(GO) run ./scripts/jsonfield last-id); \
	url=$$(ssh $(HOST) 'mp config show --json' | $(GO) run ./scripts/jsonfield panel-url); \
	echo "e2e against $$url (token #$$id)"; \
	MONOPANEL_URL=$$url MONOPANEL_TOKEN=$$token $(GO) test -tags e2e -count=1 -v ./e2e/ ; status=$$?; \
	ssh $(HOST) "mp token revoke $$id" >/dev/null 2>&1 || true; \
	exit $$status
else
	@[ -n "$$MONOPANEL_URL" ] || { \
		echo "pass a host with an ssh alias, or set the environment yourself:"; \
		echo "  make e2e HOST=toolkit"; \
		echo "  MONOPANEL_URL=https://panel:8443 MONOPANEL_LOGIN=admin MONOPANEL_PASSWORD=… make e2e"; exit 2; }
	$(GO) test -tags e2e -count=1 -v ./e2e/
endif

deb rpm: build
	VERSION=$(VERSION) ARCH=$(ARCH) nfpm package -f packaging/nfpm.yaml -p $@ -t dist/

# Dev deploy without a package: copy the binary and run setup on the test host.
deploy-dev: build
	scp -i ~/.ssh/monopanel-dev -o IdentitiesOnly=yes dist/monopanel $(DEV_HOST):/tmp/monopanel
	$(DEV_SSH) $(DEV_HOST) 'sudo install -m 0755 /tmp/monopanel /usr/bin/monopanel && sudo ln -sf /usr/bin/monopanel /usr/bin/mp && sudo mp setup'

clean:
	rm -rf dist
