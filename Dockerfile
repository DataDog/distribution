# syntax=docker/dockerfile:1

ARG GO_VERSION=1.22.2
ARG XX_VERSION=1.2.1
ARG BASE_IMAGE

FROM --platform=$BUILDPLATFORM tonistiigi/xx:${XX_VERSION} AS xx
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS base
COPY --from=xx / /
RUN apt update && apt install -y --no-install-recommends coreutils file && rm -rf /var/lib/apt
ENV GO111MODULE=auto
ENV CGO_ENABLED=0
WORKDIR /go/src/github.com/docker/distribution

FROM base AS version
ARG PKG="github.com/docker/distribution"
RUN --mount=target=. \
  VERSION=$(git describe --match 'v[0-9]*' --dirty='.m' --always --tags) REVISION=$(git rev-parse HEAD)$(if ! git diff --no-ext-diff --quiet --exit-code; then echo .m; fi); \
  echo "-X ${PKG}/version.Version=${VERSION#v} -X ${PKG}/version.Revision=${REVISION} -X ${PKG}/version.Package=${PKG}" | tee /tmp/.ldflags; \
  echo -n "${VERSION}" | tee /tmp/.version;

FROM base AS build
ARG TARGETPLATFORM
ARG LDFLAGS="-s -w"
ARG BUILDTAGS="include_oss,include_gcs"
RUN --mount=type=bind,target=/go/src/github.com/docker/distribution,rw \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=target=/go/pkg/mod,type=cache \
    --mount=type=bind,source=/tmp/.ldflags,target=/tmp/.ldflags,from=version \
      set -x ; xx-go build -tags "${BUILDTAGS}" -trimpath -ldflags "$(cat /tmp/.ldflags) ${LDFLAGS}" -o /usr/bin/registry ./cmd/registry \
      && xx-verify --static /usr/bin/registry

FROM scratch AS binary
COPY --from=build /usr/bin/registry /

FROM base AS releaser
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /work
RUN --mount=from=binary,target=/build \
    --mount=type=bind,target=/src \
    --mount=type=bind,source=/tmp/.version,target=/tmp/.version,from=version \
      VERSION=$(cat /tmp/.version) \
      && mkdir -p /out \
      && cp /build/registry /src/README.md /src/LICENSE . \
      && tar -czvf "/out/registry_${VERSION#v}_${TARGETOS}_${TARGETARCH}${TARGETVARIANT}.tar.gz" * \
      && sha256sum -z "/out/registry_${VERSION#v}_${TARGETOS}_${TARGETARCH}${TARGETVARIANT}.tar.gz" | awk '{ print $1 }' > "/out/registry_${VERSION#v}_${TARGETOS}_${TARGETARCH}${TARGETVARIANT}.tar.gz.sha256"

FROM scratch AS artifact
COPY --from=releaser /out /

FROM $BASE_IMAGE
COPY --from=binary /registry /bin/registry
EXPOSE 5000
ENTRYPOINT ["registry"]
CMD ["serve", "/etc/docker/registry/config.yml"]
