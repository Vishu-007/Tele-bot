# -------- Build stage --------
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Install CA certificates (needed for HTTPS calls to Telegram & Firestore)
RUN apk add --no-cache ca-certificates

# Copy go mod files first (better caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o app

# -------- Runtime stage --------
FROM gcr.io/distroless/base-debian12

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/app /app/app

# Cloud Run uses PORT env variable
ENV PORT=8080

EXPOSE 8080

# Run the binary
CMD ["/app/app"]
