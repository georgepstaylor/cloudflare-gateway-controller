# Build stage. --platform=$BUILDPLATFORM keeps this stage on the builder's native
# architecture so Go cross-compiles via TARGETOS/TARGETARCH below; without it buildx
# emulates the whole toolchain under QEMU for foreign targets, which took ~26 minutes
# per release.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

WORKDIR /workspace

# Copy go mod files first (these change less frequently)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code (changes more frequently)
COPY cmd/ cmd/
COPY internal/ internal/

# Build using build args from buildx
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o manager cmd/main.go

# Runtime stage
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
