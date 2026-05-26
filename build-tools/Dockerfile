# shed-build-tools — controlled environment for shed's image-build pipeline.
#
# Why this image exists
# ---------------------
# shed-fc-* and shed-vz-* images contain an erofs blob built from the
# rootfs at image-publish time (see PR 2b for the consumer side). The
# tool that builds that blob — mkfs.erofs from erofs-utils — has had
# correctness bugs in the past where certain inode layouts (notably
# the big-pcluster header set without the matching superblock feature
# flag) produce filesystems that no kernel can read. Ubuntu noble and
# Pop!_OS 24.04 ship erofs-utils 1.7.1-1build2, which is one of the
# affected versions.
#
# To remove host erofs-utils as a variable in shed-fc / shed-vz image
# builds, we pin the version centrally here, distributed as a regular
# OCI image. CI runs `docker run --rm shed-build-tools:vX.Y.Z
# mkfs.erofs ...` to produce every published shed-* erofs blob, which
# means the on-disk format is identical regardless of where the
# publish workflow runs.
#
# Versioning
# ----------
# This image is tagged in lockstep with shed-server releases
# (ghcr.io/charliek/shed-build-tools:v0.5.2, :v0.5.3, ...). The image
# content rarely changes — only when EROFS_UTILS_VERSION bumps or new
# tools land — but every shed release publishes a new tag so consumers
# can pin to the same version they're already pinning shed-server to.
# See docs/reference/build-tools.md for the version-bump procedure.
#
# Building from source vs apt-get install
# ---------------------------------------
# We build from the upstream erofs-utils git tag rather than installing
# debian's package: pinning the version explicitly decouples us from
# distro release cadence (if a critical bug is found in 1.9.1, we can
# bump immediately rather than wait for debian to update). Build is
# ~30s; not worth the loss of control to skip it.

# ---- builder stage ----
FROM debian:trixie-slim AS builder

ARG EROFS_UTILS_VERSION=v1.9.1

# Build deps for erofs-utils:
#   - autoconf/automake/libtool/pkg-config: build system
#   - libuuid1/uuid-dev: required by mkfs.erofs
#   - liblz4-dev/liblzma-dev/zlib1g-dev: compression backends
#   - libselinux1-dev: optional but enables selinux label support
#   - ca-certificates: TLS for the source fetch
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        autoconf \
        automake \
        build-essential \
        ca-certificates \
        curl \
        libselinux1-dev \
        liblz4-dev \
        liblzma-dev \
        libtool \
        libuuid1 \
        pkg-config \
        uuid-dev \
        zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN curl -fsSL "https://github.com/erofs/erofs-utils/archive/refs/tags/${EROFS_UTILS_VERSION}.tar.gz" \
        -o erofs-utils.tar.gz \
    && tar -xzf erofs-utils.tar.gz --strip-components=1 \
    && rm erofs-utils.tar.gz

# --enable-* flags mirror what debian's package enables, minus the
# fuse runtime (we only need the offline tools). Static-link the lz4
# backend so the resulting binaries don't depend on the runtime
# image's lz4 ABI version.
RUN ./autogen.sh \
    && ./configure \
        --prefix=/usr/local \
        --enable-lz4 \
        --enable-lzma \
        --enable-static \
    && make -j"$(nproc)" \
    && make install-strip

# ---- runtime stage ----
FROM debian:trixie-slim

# Runtime deps that the dynamically-linked tools need at execution time.
# liblz4 and liblzma are the compression backends; libuuid1 provides
# libuuid which mkfs.erofs uses for filesystem UUIDs.
#
# e2fsprogs supplies mkfs.ext4, used to pre-format the per-shed writable
# upper template on the host. Cloning that template into a new shed's
# upper lets the guest mount it directly and skip the multi-second
# in-guest mkfs.ext4 on first boot (see internal/vz/uppertemplate.go).
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        e2fsprogs \
        liblz4-1 \
        liblzma5 \
        libuuid1 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/mkfs.erofs /usr/local/bin/mkfs.erofs
COPY --from=builder /usr/local/bin/dump.erofs /usr/local/bin/dump.erofs
COPY --from=builder /usr/local/bin/fsck.erofs /usr/local/bin/fsck.erofs

# Quick smoke test embedded in the image: building it confirms the
# binaries at least launch (catches obvious link errors at image-build
# time rather than at first downstream use).
RUN mkfs.erofs --help >/dev/null 2>&1 || (echo "mkfs.erofs smoke test failed" && exit 1)
RUN mkfs.ext4 -V >/dev/null 2>&1 || (echo "mkfs.ext4 smoke test failed" && exit 1)

# Default to mkfs.erofs so `docker run --rm shed-build-tools:tag <args>`
# is the common case. Callers needing dump.erofs / fsck.erofs override
# the entrypoint.
ENTRYPOINT ["/usr/local/bin/mkfs.erofs"]
