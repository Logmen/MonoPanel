# The live demo's container image (demo.monopanel.app). Built by Workers
# Builds from the repository root (image_build_context in wrangler.jsonc):
# the web UI, then the panel with the demo build tag, then an Ubuntu 24.04 with
# the demo prepared inside, so a container starts serving at once.

FROM node:24-bookworm-slim AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm exec svelte-kit sync && pnpm exec vite build

FROM golang:1.27-bookworm AS panel
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/build ./web/build
# Workers Builds clones without tags: then the version is the release named in
# README.en.md plus the commit.
RUN version=$(git describe --tags --always 2>/dev/null | sed 's/^v//'); \
    case "$version" in *.*) ;; *) version="$(sed -n 's/^Status: \*\*\([0-9.]*\)\*\*.*/\1/p' README.en.md)+${version}" ;; esac; \
    CGO_ENABLED=0 go build -trimpath -tags demo \
      -ldflags "-s -w -X monopanel/internal/buildinfo.Version=${version:-demo} -X monopanel/internal/buildinfo.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo none) -X monopanel/internal/buildinfo.Repo=Logmen/MonoPanel" \
      -o /out/monopanel ./cmd/monopanel

FROM ubuntu:24.04
COPY --from=panel /out/monopanel /usr/bin/monopanel
RUN ln -s monopanel /usr/bin/mp \
 && monopanel demo prepare --dir /opt/monopanel-demo --www /var/www
EXPOSE 8080
ENTRYPOINT ["/usr/bin/monopanel", "demo", "serve", "--dir", "/opt/monopanel-demo", "--www", "/var/www", "--listen", ":8080"]
