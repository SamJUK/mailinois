# syntax=docker/dockerfile:1
# Bases pinned by digest; Dependabot moves them. The build stage runs on the builder's own platform and
# cross-compiles, so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mailinois ./cmd/mailinois \
 && mkdir -p /out/data

# One static binary: SQLite is pure Go, and the templates, styles and migrations are embedded. No shell, no
# package manager, CA certificates and tzdata only.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG VERSION=dev
LABEL org.opencontainers.image.title=Mailinois \
      org.opencontainers.image.description="Postfix observability: delivery, bounces, deferrals and DNS checks" \
      org.opencontainers.image.source=https://github.com/SamJUK/mailinois \
      org.opencontainers.image.licenses=AGPL-3.0-only \
      org.opencontainers.image.version=$VERSION
COPY --from=build /out/mailinois /mailinois
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
ENV MAILINOIS_DATABASE_URL=sqlite:/data/mailinois.db
VOLUME /data
EXPOSE 7480 5514 5514/udp 8892
# ponytail: no HEALTHCHECK. The same image runs the agent, which has no /healthz, and Swarm restarts tasks that
# fail one. Compose files add `mailinois healthcheck` to the server's service instead.
ENTRYPOINT ["/mailinois"]
CMD ["serve"]
