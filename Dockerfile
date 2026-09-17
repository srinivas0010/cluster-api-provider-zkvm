# Build the controller binary on an s390x Go builder, then copy into a
# minimal UBI9 runtime image.
#
# Two-stage build keeps the final image small — no Go toolchain in production.
# Base images sourced from Red Hat registry per IBM security policy.

# ── Stage 1: Build ────────────────────────────────────────────────────────────
FROM registry.redhat.io/ubi9/go-toolset:1.21 AS builder

WORKDIR /workspace

# Cache dependency download as a separate layer — only re-runs when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source and build.
COPY . .

# CGO_ENABLED=1 is required for libvirt.org/go/libvirt (uses cgo to call libvirt C library).
# GOARCH=s390x targets IBM Z.
RUN CGO_ENABLED=1 GOOS=linux GOARCH=s390x \
    go build -a -o manager ./main.go

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
FROM registry.redhat.io/ubi9/ubi-minimal:latest

WORKDIR /

# libvirt client library is required at runtime for cgo bindings.
# Install only the client — not the full libvirt daemon.
RUN microdnf install -y libvirt-libs && microdnf clean all

# Copy the compiled binary from the builder stage.
COPY --from=builder /workspace/manager .

# Run as non-root user (UID 65532) per IBM security policy.
USER 65532:65532

ENTRYPOINT ["/manager"]
