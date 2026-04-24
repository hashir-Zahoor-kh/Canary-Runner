# syntax=docker/dockerfile:1.7

# ---------- Stage 1: build ----------
# Pinned to the Go toolchain version in go.mod. Alpine keeps the builder
# layer small and ships a recent enough git for module fetches.
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Copy go.mod / go.sum first so the module-download layer is cached
# independently of source changes — re-running `docker build` after editing
# Go files won't re-download dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary that runs on the distroless
# `static` image (no libc). -trimpath strips local paths from the binary so
# stack traces don't leak the build environment. -ldflags="-s -w" drops the
# symbol table and DWARF info; canary-runner doesn't need them in production
# and removing them shaves a couple of MB.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/canary-runner ./

# ---------- Stage 2: runtime ----------
# distroless/static is the smallest practical Linux base for static Go
# binaries: no shell, no package manager, no anything except a CA bundle and
# /etc/passwd entries. The :nonroot tag runs as uid/gid 65532 by default.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/canary-runner /app/canary-runner

# Ship the example config so `docker run` works out of the box. Operators
# override it with a bind mount: -v $(pwd)/config.yaml:/app/config.yaml
COPY config.yaml /app/config.yaml

# /metrics + /health server. The probe loop has no listening socket of its
# own — only the metrics endpoint needs to be reachable.
EXPOSE 9090

USER nonroot:nonroot

ENTRYPOINT ["/app/canary-runner"]
CMD ["-config", "/app/config.yaml", "-metrics-addr", ":9090"]
