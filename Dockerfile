# Host-build approach for s390x IBM Z KVM provider.
#
# The manager binary is compiled directly on the s390x RHEL host
# (which has libvirt-devel + gcc available) and then copied into
# a minimal UBI9 runtime image.
#
# Build steps (run on the s390x host before podman build):
#   1. CGO_ENABLED=1 GOOS=linux GOARCH=s390x go build -o bin/manager ./main.go
#   2. podman build -t quay.io/rh-ee-dveerabh/cluster-api-provider-zkvm:v0.1.0 .
#   3. podman push quay.io/rh-ee-dveerabh/cluster-api-provider-zkvm:v0.1.0
#
# Why host-build?
#   libvirt-devel (C headers for CGo) is only available via the
#   CodeReady Linux Builder repo on a subscribed RHEL s390x host.
#   It is not available inside UBI9 builder containers.
#
# Base image sourced from Red Hat registry per IBM security policy.

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

# libvirt-libs provides the libvirt.so shared library that the
# manager binary is dynamically linked against at runtime.
# openssh-clients provides ssh/scp used by the libvirt SSH transport.
RUN microdnf install -y libvirt-libs openssh-clients && \
    microdnf clean all

WORKDIR /

# Copy the pre-compiled manager binary from the host build.
# The binary must exist at bin/manager before running podman build.
COPY bin/manager /manager

# Run as non-root user (UID 65532) per IBM security policy.
USER 65532:65532

ENTRYPOINT ["/manager"]
