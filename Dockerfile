# Build
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o status-check ./cmd/

# Runtime
FROM alpine:3.19

# ca-certificates is required for TLS connections to HTTPS URLs.
RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /app/status-check .
COPY config.yaml .

# AWS credentials are injected at runtime via environment variables or an IAM role.
# Ensure you allow Docker to pass in any relevant AWS client env variables.

CMD ["./status-check"]
