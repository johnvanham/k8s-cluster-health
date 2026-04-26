# Build stage: full Go toolchain only at build time.
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled so the binary is fully static and runs on distroless/static.
# -trimpath strips local paths from the binary; -s -w drops the symbol table.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/k8s-cluster-health \
    .

# Runtime stage: distroless/static — TLS roots, /etc/passwd, tzdata, nothing else.
# No shell, no package manager, ~2 MB on top of the binary.
FROM gcr.io/distroless/static-debian12:latest
COPY --from=builder /out/k8s-cluster-health /usr/local/bin/k8s-cluster-health

# Defaults that make the most sense in a container:
# - logs go to stdout (no -log-dir mount required for the basic case)
# - state lives at /state (mount a volume there to persist across restarts)
# - container detection auto-disables -no-bell / -no-notify
ENV HOME=/tmp
WORKDIR /tmp

ENTRYPOINT ["/usr/local/bin/k8s-cluster-health"]
CMD ["-log-dir", "/tmp", "-state-dir", "/state"]
