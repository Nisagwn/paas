# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/minipaas ./cmd/minipaas

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/minipaas /minipaas
USER nonroot
EXPOSE 8080
ENTRYPOINT ["/minipaas"]
