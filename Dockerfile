# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
RUN go test ./cmd/ && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags '-s -w' -o intel-gpu-exporter ./cmd

# Static binary only: every metric comes from files in /proc and /sys, no intel_gpu_top
FROM scratch

COPY --from=builder /build/intel-gpu-exporter /intel-gpu-exporter

EXPOSE 8080

ENTRYPOINT ["/intel-gpu-exporter"]
