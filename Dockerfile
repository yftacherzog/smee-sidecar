# Stage 1: Build the Go binary
FROM registry.access.redhat.com/ubi9/go-toolset:9.6-1760420453 AS builder
ARG TARGETOS
ARG TARGETARCH

ENV GOTOOLCHAIN=auto
WORKDIR /workspace

# Copy go.mod and go.sum files to download dependencies
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as
# much and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the rest of the source code
COPY cmd/main.go cmd/main.go
COPY cmd/scripts/ cmd/scripts/

# Build the binary with flags for a small, static executable
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /opt/app-root/smee-sidecar cmd/main.go

# Stage 2: Create the final, minimal image
FROM registry.access.redhat.com/ubi9-minimal@sha256:7c5495d5fad59aaee12abc3cbbd2b283818ee1e814b00dbc7f25bf2d14fa4f0c

# Copy the static binary from the builder stage
WORKDIR /
COPY --from=builder /opt/app-root/smee-sidecar .
COPY LICENSE /licenses/
USER 65532:65532

# It is mandatory to set these labels
LABEL name="Smee health check Sidecar"
LABEL description="Smee health check Sidecar"
LABEL com.redhat.component="Smee health check Sidecar"
LABEL io.k8s.description="Smee health check Sidecar"
LABEL io.k8s.display-name="smee-sidecar"

# Set the entrypoint for the container
ENTRYPOINT ["/smee-sidecar"]
