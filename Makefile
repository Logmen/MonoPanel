VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
# Releases are tagged v0.6.0 but the version in package names, in `mp version`
# and in the update check is plain semver.
VERSION := $(patsubst v%,%,$(VERSION))
RPMARCH  = $(if $(filter arm64,$(ARCH)),aarch64,x86_64)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# REPO is baked into the binary as the place to look for releases; the
# release workflow passes the repository it runs in, a local build takes origin.
REPO ?= $(shell git remote get-url origin 2>/dev/null | sed -nE 's#^(git@|https://)github\.com[:/]([^/]+/[^/.]+)(\.git)?$$#\2#p')
LDFLAGS := -s -w -X monopanel/internal/buildinfo.Version=$(VERSION) -X monopanel/internal/buildinfo.Commit=$(COMMIT) -X monopanel/internal/buildinfo.Date=$(DATE) -X monopanel/internal/buildinfo.Repo=$(REPO)
GO      ?= go
ARCH    ?= amd64
# Where `make deploy-dev` and `make e2e` point. Keep your own host out of the
# repository: put DEV_HOST/DEV_SSH/HOST in .dev/config.mk, which is git-ignored.
-include .dev/config.mk
DEV_HOST ?= root@panel.example.com
DEV_SSH  ?= ssh
DEV_SCP  ?= scp
GOLANGCI_VERSION ?= v2.13.2

.PHONY: build build-arm64 test test-race test-short cover cover-html lint vet fmt fmt-check check web web-check web-stub e2e deb rpm packages arch-artifacts sign release keygen deploy-dev clean help testbed-up testbed-reset testbed-down testbed-status testbed-deploy testbed-e2e testbed-matrix testbed-migrate testbed-dns testbed-cms

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
e2e: web-stub ## Run end-to-end tests against a live panel (HOST=<ssh alias>, or MONOPANEL_* in the environment)
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
		echo "  make e2e HOST=<ssh alias>"; \
		echo "  MONOPANEL_URL=https://panel:8443 MONOPANEL_LOGIN=admin MONOPANEL_PASSWORD=… make e2e"; exit 2; }
	$(GO) test -tags e2e -count=1 -v ./e2e/
endif

deb rpm: build
	VERSION=$(VERSION) ARCH=$(ARCH) nfpm package -f packaging/nfpm.yaml -p $@ -t dist/

# Release artefacts. The names are part of the update protocol: the panel asks
# a release for exactly these files, so nothing here may be renamed casually.
packages: ## Build the release packages and binaries for amd64 and arm64
	@command -v nfpm >/dev/null 2>&1 || (echo "nfpm is missing: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest" && exit 1)
	$(MAKE) arch-artifacts ARCH=amd64
	$(MAKE) arch-artifacts ARCH=arm64
	cd dist && sha256sum monopanel_$(VERSION)_*.deb monopanel-$(VERSION).*.rpm monopanel-linux-* > SHA256SUMS
	@echo "dist/: $$(cd dist && ls monopanel_* monopanel-* SHA256SUMS | tr '\n' ' ')"

arch-artifacts: build
	cp dist/monopanel dist/monopanel-linux-$(ARCH)
	VERSION=$(VERSION) ARCH=$(ARCH) nfpm package -f packaging/nfpm.yaml -p deb -t dist/monopanel_$(VERSION)_$(ARCH).deb
	VERSION=$(VERSION) ARCH=$(ARCH) nfpm package -f packaging/nfpm.yaml -p rpm -t dist/monopanel-$(VERSION).$(RPMARCH).rpm

keygen: ## Generate a release signing key pair (private key goes into the repository secret)
	$(GO) run ./scripts/release keygen

# Signing needs MONOPANEL_RELEASE_KEY; without it the release still installs,
# but only on servers that have no key pinned.
sign: ## Sign dist/SHA256SUMS with $MONOPANEL_RELEASE_KEY
	$(GO) run ./scripts/release sign dist/SHA256SUMS

release: ## Tag the current commit and let CI publish the release (VERSION=0.6.0)
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.]+)?$$' || \
		{ echo "pass a release version: make release VERSION=0.6.0"; exit 2; }
	@git diff --quiet || { echo "working tree is dirty"; exit 2; }
	git tag -a v$(VERSION) -m "MonoPanel $(VERSION)"
	git push origin v$(VERSION)
	@echo "release workflow: gh run watch \$$(gh run list --workflow=release.yml -L1 --json databaseId -q '.[0].databaseId')"

# Dev deploy without a package: copy the binary and run setup on the test host.
deploy-dev: build
	$(DEV_SCP) dist/monopanel $(DEV_HOST):/tmp/monopanel
	$(DEV_SSH) $(DEV_HOST) 'sudo install -m 0755 /tmp/monopanel /usr/bin/monopanel && sudo ln -sf /usr/bin/monopanel /usr/bin/mp && sudo mp setup'

clean:
	rm -rf dist

# Testbed: one VM per supported distribution on a Proxmox host, built from the
# official cloud images (scripts/testbed/). The host and the network live in
# the git-ignored .dev/testbed.env. VM=<name> picks a distribution, e.g.
# debian13, ubuntu2404, alma10; without it every VM is meant.
testbed-up: ## Create the missing testbed VMs and boot them (VM=<name> for one)
	scripts/testbed/testbed.sh up $(VM)

testbed-reset: ## Roll the testbed VMs back to the clean snapshot
	scripts/testbed/testbed.sh reset $(VM)

testbed-down: ## Destroy the testbed VMs
	scripts/testbed/testbed.sh down $(VM)

testbed-sources: ## BitrixVM / FASTPANEL sources for the migration: ARGS="install|seed|migrate ..."
	scripts/testbed/testbed.sh sources $(ARGS)

testbed-status: ## Show the testbed VMs
	scripts/testbed/testbed.sh status

testbed-deploy: ## Build the package, install the panel and the stack on VM=<name>
	@[ -n "$(VM)" ] || { echo "make testbed-deploy VM=debian13"; exit 2; }
	scripts/testbed/testbed.sh deploy $(VM)

testbed-e2e: ## Run the end-to-end tests against VM=<name>
	@[ -n "$(VM)" ] || { echo "make testbed-e2e VM=debian13"; exit 2; }
	scripts/testbed/testbed.sh e2e $(VM)

testbed-matrix: ## reset + deploy + e2e on every testbed VM in parallel, then a summary
	scripts/testbed/testbed.sh matrix $(VM)

testbed-dns: ## Create the VMs' A records in the Cloudflare zone from .dev/testbed.env
	scripts/testbed/testbed.sh dns up

testbed-cms: ## Install CMSs (CMS="wordpress joomla opencart bitrix") into preset sites on NAME=<vm> and check them
	@[ -n "$(NAME)" ] || { echo "make testbed-cms NAME=debian12 [CMS=\"wordpress bitrix\"]"; exit 2; }
	scripts/testbed/testbed.sh cms $(NAME) $(CMS)

testbed-migrate: ## Move an account from SRC=<name> to DST=<name> and verify it arrived
	@[ -n "$(SRC)" ] && [ -n "$(DST)" ] || { echo "make testbed-migrate SRC=ubuntu2404 DST=debian13"; exit 2; }
	scripts/testbed/testbed.sh migrate $(SRC) $(DST)
