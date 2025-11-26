# Build stage
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum* ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
# Use TARGETOS and TARGETARCH for cross-compilation
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -a -installsuffix cgo -o sync .

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /build/sync .

# Copy CA certificates for HTTPS requests
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Distroless images run as non-root by default (uid/gid 65532)
# No need to create user - already done by :nonroot variant

# Run the application
ENTRYPOINT ["/app/sync"]
