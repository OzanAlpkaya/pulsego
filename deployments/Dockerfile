# Multi-stage build shared by all three binaries. Select one with
#   docker build --build-arg BINARY=api -f deployments/Dockerfile .
# GO_VERSION tracks the latest Go 1.27 patch (the golang:1.27-alpine tag moves
# with each patch release), so the standard library carries every security
# fix. It is independent of go.mod's `go` directive, which only sets the
# minimum Go version the module needs.
ARG GO_VERSION=1.27

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies are copied first so that source-only edits reuse this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BINARY=api
# CGO_ENABLED=0 produces a static binary, which is what lets the final stage
# stay this small. -trimpath keeps build paths out of the artifact.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/app ./cmd/${BINARY}

FROM alpine:3.22 AS runtime

# CA certificates are required to check HTTPS targets. The shell and busybox
# wget stay in the image: wget serves the health check below, and the shell
# helps when debugging. Distroless would need a separate probe binary.
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 pulsego

COPY --from=build /out/app /usr/local/bin/app

# Numeric, so that runtimes enforcing runAsNonRoot can verify it without
# resolving names from /etc/passwd.
USER 10001:10001

# A real GET (not --spider, which sends HEAD) against the DB-free liveness
# endpoint on the internal listener. api and worker both serve it on :9090;
# the one-shot migrate image never does, so Compose disables it there.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["wget", "-q", "-O", "/dev/null", "-T", "2", "http://127.0.0.1:9090/healthz"]

ENTRYPOINT ["/usr/local/bin/app"]
