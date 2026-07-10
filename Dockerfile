FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src
# Copy the module manifests first so `go mod download` caches dependency resolution in its own layer,
# separate from the source (a code-only change reuses the cached deps). go.sum is required now that the
# app has deps (OTel) — a -mod=readonly build fails without it in the context.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/

# Cross-compile to the target arch (buildx sets TARGETOS/TARGETARCH). The build runs natively on the arm64
# runner — no QEMU. Cache mounts persist the module + Go build/compile cache across builds so a small code
# change recompiles incrementally in seconds (trusted-ci#22).
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /app ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /app /app

EXPOSE 8080

# Run as the distroless nonroot user explicitly (uid:gid 65532). The base already defaults to nonroot,
# but an explicit USER makes it auditable and satisfies the image-runs-as-root scanners
# (Trivy DS-0002 / Semgrep missing-user-entrypoint).
USER 65532:65532

ENTRYPOINT ["/app"]
