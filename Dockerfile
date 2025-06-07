# Multi-stage build for KinD - Fixed with rsync
FROM golang:1.24-bullseye AS builder

ARG KUBERNETES_VERSION=v1.33.1
ARG GOOS=linux
ARG GOARCH=amd64

# Install required tools INCLUDING rsync
RUN apt-get update && apt-get install -y \
    git \
    make \
    bash \
    rsync \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

# Clone and build Kubernetes scheduler
RUN git clone --depth 1 https://github.com/kubernetes/kubernetes.git -b ${KUBERNETES_VERSION} .

ENV GOOS=${GOOS} GOARCH=${GOARCH} CGO_ENABLED=0

RUN make all WHAT=cmd/kube-scheduler

# Final minimal image
FROM gcr.io/distroless/static:latest
COPY --from=builder /workspace/_output/local/bin/linux/amd64/kube-scheduler /usr/local/bin/kube-scheduler
ENTRYPOINT ["/usr/local/bin/kube-scheduler"]
