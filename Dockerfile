# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder

WORKDIR /src

# GOPROXY is the standard knob for corporate module mirrors; the default is
# unchanged. GOTOOLCHAIN=local keeps the build hermetic on the pinned image
# toolchain.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
# Cache mounts keep the module and compiler caches across builds, so a
# one-line source change recompiles only what changed instead of the whole
# dependency tree.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/order-engine ./cmd/api

FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

COPY --from=builder /out/order-engine /usr/local/bin/order-engine

# Numeric UID:GID (distroless "nonroot") on purpose: kubelet can only verify
# runAsNonRoot against a numeric image user; a named USER fails admission
# with CreateContainerConfigError.
USER 65532:65532
EXPOSE 50051
ENTRYPOINT ["/usr/local/bin/order-engine"]
