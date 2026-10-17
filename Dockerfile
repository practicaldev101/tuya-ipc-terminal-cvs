# Reproducible build of the CVS fork of tuya-ipc-terminal.
# Base images are pinned by digest (multi-arch index digests); bump them
# deliberately and record the change in CVS-CHANGES.md.
ARG GO_IMAGE=golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Build on the native platform and cross-compile (no emulation needed).
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
ARG VERSION=0.0.6-cvs.5
ARG TARGETOS
ARG TARGETARCH
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.VERSION=${VERSION}" \
      -o /out/tuya-ipc-terminal . \
 && mkdir -p /out/data

# distroless/static: no shell, CA certificates + tzdata included, runs as
# the unprivileged "nonroot" user (65532).
FROM ${RUNTIME_IMAGE}
COPY --from=build /out/tuya-ipc-terminal /usr/local/bin/tuya-ipc-terminal
# Session cookies and the camera registry live in ./.tuya-data under the
# working directory; mount a persistent volume at /data.
COPY --from=build --chown=65532:65532 /out/data /data
WORKDIR /data
USER 65532:65532
VOLUME ["/data"]
EXPOSE 8554
ENTRYPOINT ["/usr/local/bin/tuya-ipc-terminal"]
# Loopback only by default: run with host networking (WebRTC/ICE needs it)
# and point the recorder at rtsp://127.0.0.1:8554/<camera>/sd.
CMD ["rtsp", "start", "--listen", "127.0.0.1", "--port", "8554"]
