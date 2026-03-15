FROM golang:1.26-alpine3.22 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o gorge-webhook ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /src/gorge-webhook /usr/local/bin/gorge-webhook

EXPOSE 8160

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8160/healthz || exit 1

ENTRYPOINT ["gorge-webhook"]
