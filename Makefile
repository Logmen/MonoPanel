VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X monopanel/internal/buildinfo.Version=$(VERSION) -X monopanel/internal/buildinfo.Commit=$(COMMIT) -X monopanel/internal/buildinfo.Date=$(DATE)
GO      ?= go
ARCH    ?= amd64
DEV_HOST ?= ubuntu@185.253.8.5
DEV_SSH  ?= ssh -i ~/.ssh/monopanel-dev -o IdentitiesOnly=yes

.PHONY: build build-arm64 test vet fmt web web-stub deb rpm deploy-dev clean

# web/build is generated (and git-ignored) but go:embed needs it to exist;
# this stubs it with the placeholder page so go vet/test/build work on a fresh clone.
web-stub:
	@[ -f web/build/index.html ] || (mkdir -p web/build && cp web/placeholder/index.html web/build/index.html && echo "web/build missing: embedded placeholder UI (run make web)")

build: web-stub
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/monopanel ./cmd/monopanel

build-arm64:
	$(MAKE) build ARCH=arm64

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w cmd internal templates

web:
	cd web && pnpm install && node_modules/.bin/svelte-kit sync && node_modules/.bin/vite build

deb rpm: build
	VERSION=$(VERSION) ARCH=$(ARCH) nfpm package -f packaging/nfpm.yaml -p $@ -t dist/

# Dev deploy without a package: copy the binary and run setup on the test host.
deploy-dev: build
	scp -i ~/.ssh/monopanel-dev -o IdentitiesOnly=yes dist/monopanel $(DEV_HOST):/tmp/monopanel
	$(DEV_SSH) $(DEV_HOST) 'sudo install -m 0755 /tmp/monopanel /usr/bin/monopanel && sudo ln -sf /usr/bin/monopanel /usr/bin/mp && sudo mp setup'

clean:
	rm -rf dist
