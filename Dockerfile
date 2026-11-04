# Multi-arch image (linux/amd64, linux/arm64, linux/arm/v7). The Go build
# cross-compiles on the build machine, so no emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT=
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w -X github.com/DrSeussFreak/BackTrackarr/internal/model.Version=${VERSION}" \
    -o /out/backtrackarr . && \
    mkdir -p /out/data

# distroless/static: ~2 MB, includes CA certs and tzdata, runs as non-root (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/backtrackarr /backtrackarr
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data \
    LISTEN=:8787 \
    GOMEMLIMIT=96MiB \
    GOGC=50
EXPOSE 8787
VOLUME ["/data"]
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s CMD ["/backtrackarr", "healthcheck"]
ENTRYPOINT ["/backtrackarr"]
