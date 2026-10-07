# earth-edge: one static binary on an empty image, run as uid 65532.
#
#   bin/build-edge.sh     builds this for linux/amd64, pushes it to
#                         ghcr.io/zenopie/earth-network-edge and prints the
#                         digest akash/deploy.yaml pins (RELAUNCH.md 1.4)
#
# Standard library only, so the build downloads nothing. The filter's tests run
# here: an image whose allowlist, bypass or cost tables fail is never produced.
# (conformance/ is not in the build context; it needs the pinned chain and the
# module cache, and runs by hand and in CI.)

# The builder runs on the build machine's own platform and cross-compiles
# (CGO off), so an arm64 laptop builds the amd64 image without emulation.
FROM --platform=$BUILDPLATFORM golang:1.25.10-trixie@sha256:adc12eea75e38959793d88b5c15f9f8ad20fee5ece9d3d5ae05c7d7c2d430f4b AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
COPY go.mod main.go ./
COPY filter/ filter/
RUN go vet ./... && go test ./filter/
ARG TARGETOS=linux TARGETARCH=amd64
# -trimpath and -buildvcs=false: no build-machine path or VCS stamp in the
# binary, so the same commit builds the same bytes.
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
        -ldflags='-s -w' -o /out/earth-edge .

# Nothing but the binary: no shell, no package manager, no setuid helper.
# The SDL sets no command, so this USER is what runs; earth-edge refuses to
# start as root in case an override ever says otherwise.
FROM scratch
COPY --from=build /out/earth-edge /earth-edge
USER 65532:65532
EXPOSE 26657 1317
ENTRYPOINT ["/earth-edge"]
