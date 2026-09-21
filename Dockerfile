FROM golang:1.25-alpine AS builder

ARG VERSION=0.0.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-w -s -X github.com/starcat-app/starcat-history-api/internal/version.Version=${VERSION}" \
    -o /out/server ./cmd/server

FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata \
 && addgroup -S app \
 && adduser -S app -G app \
 && mkdir -p /data \
 && chown app:app /data
USER app
WORKDIR /app
COPY --from=builder /out/server /app/server
EXPOSE 5014
ENTRYPOINT ["/app/server"]
