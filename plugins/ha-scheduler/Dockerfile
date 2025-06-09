# Dockerfile for standalone scheduler
FROM golang:1.24-bullseye AS builder

WORKDIR /workspace

# Copy go module files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY main.go ./

# Build the scheduler
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -o ha-scheduler main.go

# Final image
FROM gcr.io/distroless/static:latest

COPY --from=builder /workspace/ha-scheduler /usr/local/bin/ha-scheduler

USER 65534:65534

ENTRYPOINT ["/usr/local/bin/ha-scheduler"]
