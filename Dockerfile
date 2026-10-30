# Build the manager binary
# Pinned by tag, not digest: quay.io deletes each superseded 1.26 digest, which
# broke every uncached build twice (2026-08-14, 2026-10-20). The tag fixes the
# Go minor; Dependabot's docker updater (.github/dependabot.yml) proposes the
# next one. The runtime stage below stays digest-pinned: gcr.io keeps old ones.
FROM quay.io/projectquay/golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY internal/ internal/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
# The commit, recorded in learner recordings (learnrec.BuildCommit).
ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -ldflags "-X github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec.BuildCommit=${GIT_COMMIT}" -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

# The repository this image is built from. The workflows pass their own
# (github.server_url/github.repository), so a fork's image names the fork.
ARG IMAGE_SOURCE=https://github.com/ev-shindin/llm-scaling-manager
LABEL org.opencontainers.image.source="${IMAGE_SOURCE}"
LABEL org.opencontainers.image.description="Workload Variant Autoscaler (WVA) - GPU-aware autoscaler for LLM inference workloads"
LABEL org.opencontainers.image.licenses="Apache-2.0"

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
