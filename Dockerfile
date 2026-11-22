# syntax=docker/dockerfile:1

# Keep in step with the `go` line in go.mod (the image sets GOTOOLCHAIN=local, so an older
# Go here fails at `go mod download` instead of fetching a newer toolchain).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/whooper ./cmd/whooper

# Alpine rather than distroless: Dokploy schedules run `docker exec <container> <shell> -c
# "<command>"`, with bash as the default shell, so the image needs bash (and wget for
# HEALTHCHECK).
FROM alpine:3.22
RUN apk add --no-cache bash ca-certificates tzdata \
    && adduser -D -H -u 10001 whooper
COPY --from=build /out/whooper /usr/local/bin/whooper
USER whooper
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${PORT}/health" > /dev/null || exit 1
CMD ["whooper", "serve"]
