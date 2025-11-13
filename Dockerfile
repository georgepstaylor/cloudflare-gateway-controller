# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /workspace

# Copy go mod files first (these change less frequently)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code (changes more frequently)
COPY cmd/ cmd/
COPY internal/ internal/
COPY api/ api/

# Build (remove -a flag to use cache)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o manager cmd/main.go

# Runtime stage
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
