# ---- build ----
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY . .
# go mod tidy resolves go.sum on first build; commit the generated go.sum afterwards.
RUN go mod tidy && \
    go vet ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backlottracker . && \
    mkdir -p /out/data

# ---- run ----
# distroless/static: ~2 MB, includes CA certs and tzdata, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/backlottracker /backlottracker
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data \
    LISTEN=:8787 \
    GOMEMLIMIT=96MiB \
    GOGC=50
EXPOSE 8787
VOLUME ["/data"]
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s CMD ["/backlottracker", "healthcheck"]
ENTRYPOINT ["/backlottracker"]
