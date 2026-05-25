# ddo-cloudflare / Dockerfile
#
# Pure-Go build. CGO is disabled so the resulting binary runs on
# distroless static without any glibc.
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webhook ./cmd/webhook

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/webhook /usr/local/bin/webhook
USER nonroot:nonroot
EXPOSE 9090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/webhook", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/webhook"]
