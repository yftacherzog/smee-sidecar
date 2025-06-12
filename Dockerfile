# Stage 1: Build the Go binary
FROM registry.access.redhat.com/ubi9/go-toolset:9.6-1747333074 AS builder
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

# Build the binary with flags for a small, static executable
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /opt/app-root/smee-sidecar cmd/main.go

# Stage 2: Create the final, minimal image
FROM registry.access.redhat.com/ubi9-micro@sha256:9d241cec87d22681d8e4ffe76d98d960b48d967c108cd627a139df670b793186

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
