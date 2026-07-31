# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy the entire source code first (fixes missing go.sum issues)
COPY . .

# Download dependencies
RUN go mod download

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o wowzers .

# Run stage
FROM alpine:latest

WORKDIR /app

# Copy the binary from the builder stage
COPY --from=builder /app/wowzers .

# Copy the web assets
COPY --from=builder /app/web ./web

# Expose the port the app runs on
EXPOSE 7654

# Command to run the binary
CMD ["./wowzers"]