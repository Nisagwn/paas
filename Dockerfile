# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/paas ./cmd/paas

# Only the buildctl client; buildkitd itself runs as a separate rootless pod.
FROM moby/buildkit:v0.20.2 AS buildkit

# Not distroless: the builder shells out to git and buildctl.
FROM alpine:3.21
RUN apk add --no-cache git ca-certificates \
    && adduser -D -u 65532 paas
COPY --from=buildkit /usr/bin/buildctl /usr/local/bin/buildctl
COPY --from=build /out/paas /usr/local/bin/paas
USER 65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/paas"]
