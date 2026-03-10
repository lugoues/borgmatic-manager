# Build stage: compile static binary
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache dependency downloads
COPY go.mod go.sum ./
RUN go mod download

# Build static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /borgmatic-manager ./cmd/borgmatic-manager

# Runtime stage: minimal scratch image
FROM scratch

COPY --from=builder /borgmatic-manager /borgmatic-manager

ENTRYPOINT ["/borgmatic-manager"]
