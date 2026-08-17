# Multi-stage build shared by all three binaries. Select one with
#   docker build --build-arg BINARY=api -f deployments/Dockerfile .
ARG GO_VERSION=1.26

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

# CA certificates are required to check HTTPS targets; wget serves the
# container health check.
RUN apk add --no-cache ca-certificates wget \
    && adduser -D -H -u 10001 pulsego

COPY --from=build /out/app /usr/local/bin/app

USER pulsego:pulsego

ENTRYPOINT ["/usr/local/bin/app"]
