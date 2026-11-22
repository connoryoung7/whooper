# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/whooper ./cmd/whooper

# Alpine rather than distroless: Dokploy schedules run commands with `docker exec`, and a
# shell plus wget (for HEALTHCHECK) make that and debugging straightforward.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 whooper
COPY --from=build /out/whooper /usr/local/bin/whooper
USER whooper
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${PORT}/health" > /dev/null || exit 1
CMD ["whooper", "serve"]
