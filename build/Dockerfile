ARG GO_VERSION=1.23
ARG ALPINE_VERSION=3.22
ARG BUILDPLATFORM
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION}-alpine AS backend
ARG VERSION=0.1.0
WORKDIR /src/backend
COPY backend/go.mod ./
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /laserbridge ./cmd/laserbridge && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /laserbridged ./cmd/laserbridged

FROM alpine:${ALPINE_VERSION} AS ustreamer
ARG USTREAMER_COMMIT=b5e12a841270fedea75aa03b05de8fda15a3745e
RUN apk add --no-cache build-base ca-certificates libbsd-dev libevent-dev \
    libjpeg-turbo-dev linux-headers tar
WORKDIR /src
RUN wget -q -O source.tar.gz "https://github.com/pikvm/ustreamer/archive/${USTREAMER_COMMIT}.tar.gz" && \
    tar -xzf source.tar.gz --strip-components=1 && \
    make -j2 WITH_PTHREAD_NP=0 WITH_SETPROCTITLE=0 && \
    strip ustreamer

FROM alpine:${ALPINE_VERSION}
ARG VERSION=0.1.0
ARG SOURCE_DATE_EPOCH=1786579200

RUN apk add --no-cache \
    bash coreutils dosfstools e2fsprogs findutils grub-efi mtools sgdisk \
    rsync squashfs-tools xz zip

COPY build/ /workspace/build/
COPY rootfs/ /workspace/rootfs/
COPY web/ /workspace/web/
COPY --from=backend /laserbridge /workspace/laserbridge
COPY --from=backend /laserbridged /workspace/laserbridged
COPY --from=ustreamer /src/ustreamer /workspace/ustreamer

RUN chmod +x /workspace/build/*.sh /workspace/rootfs/etc/init.d/* && \
    VERSION="${VERSION}" SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH}" \
    /workspace/build/prepare-rootfs.sh

ENTRYPOINT ["/workspace/build/build-image.sh"]
