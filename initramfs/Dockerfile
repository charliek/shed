# Builds the shed initramfs: a minimal busybox-based initrd whose only
# job is to mount the overlay (writable upper on /dev/vda + read-only
# lower on /dev/vdb) and pivot_root into the merged image.
#
# Build context must be the project root so `COPY initramfs/init`
# resolves. Output: /initrd.img (cpio.gz), exposed via the final
# `shed-initramfs` scratch stage so `--output type=local` produces only
# the artifact.
#
#   docker buildx build --platform linux/arm64 \
#     -f initramfs/Dockerfile \
#     --target shed-initramfs \
#     --output type=local,dest=./out .

# Stage: pull Ubuntu's linux-image-virtual kernel modules so the
# initramfs can insmod erofs.ko before mounting the per-layer erofs
# lowers. Ubuntu ships erofs as a *module* (=m) in linux-image-virtual,
# not built-in, and overlay.ko alone isn't enough — without erofs
# loaded, the lower mounts fail with EINVAL.
FROM ubuntu:24.04 AS ubuntu-modules

RUN apt-get update && apt-get install -y --no-install-recommends \
        linux-image-virtual \
    && rm -rf /var/lib/apt/lists/*

# Stage erofs.ko AND its dependency libcrc32c.ko (erofs uses crc32c for
# block-level checksums). insmod doesn't auto-resolve module deps
# (unlike modprobe), so we have to load libcrc32c first. Naming
# matters: the init script loads libcrc32c BEFORE erofs.
#
# Filenames in /stage/ get a numeric prefix so the init script can
# load them in sorted order (libcrc32c=10, erofs=20). Without this,
# erofs.ko's "Unknown symbol crc32c" fails the load and erofs mounts
# return EINVAL.
RUN mkdir -p /stage \
    && erofs_ko=$(find /lib/modules -name 'erofs.ko*' 2>/dev/null | tail -1) \
    && crc32c_ko=$(find /lib/modules -name 'libcrc32c.ko*' 2>/dev/null | tail -1) \
    && if [ -z "$erofs_ko" ]; then echo "ERROR: erofs.ko not found in linux-image-virtual" >&2; exit 1; fi \
    && if [ -z "$crc32c_ko" ]; then echo "ERROR: libcrc32c.ko not found" >&2; exit 1; fi \
    && cp "$crc32c_ko" "/stage/10-$(basename "$crc32c_ko")" \
    && cp "$erofs_ko"  "/stage/20-$(basename "$erofs_ko")" \
    && kver=$(basename "$(dirname "$(dirname "$(dirname "$(dirname "$erofs_ko")")")")") \
    && echo "$kver" > /stage/kver.txt \
    && echo "Staged: 10-$(basename "$crc32c_ko"), 20-$(basename "$erofs_ko") for kver $kver"

# Trixie (Debian 13) for erofs-utils 1.8.6: needed for `mkfs.erofs
# --tar=f`, which the materialize-mode init pipeline streams the layer
# tar.gz through. Bookworm ships erofs-utils 1.5 which predates --tar.
FROM debian:trixie-slim AS shed-initramfs-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
        busybox-static \
        e2fsprogs \
        erofs-utils \
        cpio \
        gzip \
        kmod \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /irfs/bin /irfs/sbin /irfs/dev /irfs/proc /irfs/sys \
             /irfs/run /irfs/etc /irfs/newroot /irfs/upper /irfs/lower /irfs/mnt

# busybox provides sh, mount, mkdir, dd, od, etc. Symlink the applets
# we rely on so the initrd stays small.
#
# `switch_root` is the canonical initramfs exit; `pivot_root` was
# tried first but EINVAL'd on the kernel rootfs (see initramfs/init
# and commit ceafa1e). Kept in the applet list defensively in case
# a future init variant needs it; `switch_root` is what runs today.
RUN cp /bin/busybox /irfs/bin/busybox \
    && for cmd in sh mount umount mkdir mknod cat echo sleep chmod chown \
                  ls cp mv rm ln dd od tr grep cut head tail sed find uname \
                  switch_root pivot_root reboot poweroff blkid \
                  tar gunzip gzip sync; do \
        ln -sf busybox /irfs/bin/$cmd; \
    done

# mkfs.ext4 ships outside busybox.
RUN cp /usr/sbin/mkfs.ext4 /irfs/sbin/mkfs.ext4 \
    && for lib in $(ldd /usr/sbin/mkfs.ext4 \
            | awk '/=>/ {print $3} /ld-linux/ {print $1}' \
            | grep -v '^$'); do \
        mkdir -p "/irfs$(dirname "$lib")"; \
        cp "$lib" "/irfs$lib"; \
    done

# mkfs.erofs: used by the materializer-mode boot path
# (shed.mode=materialize) to convert OCI layer tar.gz blobs into
# read-only lower images without launching a Docker container on the
# host. lz4 compression is supported by Linux 5.4+; the shed kernels
# already include CONFIG_EROFS_FS=y plus CONFIG_EROFS_FS_ZIP=y.
#
# libgcc_s.so.1 is dlopen'd at runtime for pthread_exit cleanup —
# `ldd` doesn't see it because it's not linked at build time. Without
# it mkfs.erofs aborts with "libgcc_s.so.1 must be installed for
# pthread_exit to work" when shutting down its worker pool. Copy it
# explicitly.
RUN cp /usr/bin/mkfs.erofs /irfs/sbin/mkfs.erofs \
    && for lib in $(ldd /usr/bin/mkfs.erofs \
            | awk '/=>/ {print $3} /ld-linux/ {print $1}' \
            | grep -v '^$'); do \
        mkdir -p "/irfs$(dirname "$lib")"; \
        cp "$lib" "/irfs$lib"; \
    done \
    && libgcc=$(find /usr/lib /lib -name 'libgcc_s.so.1' 2>/dev/null | head -1) \
    && if [ -n "$libgcc" ]; then \
         mkdir -p "/irfs$(dirname "$libgcc")"; \
         cp "$libgcc" "/irfs$libgcc"; \
       else \
         echo "WARNING: libgcc_s.so.1 not found — mkfs.erofs may abort on shutdown" >&2; \
       fi

# kmod's insmod uses finit_module(2) via libkmod, which decompresses
# .ko.gz / .ko.xz / .ko.zst transparently (busybox's insmod doesn't).
# Ubuntu 24.04 ships overlayfs as a zstd-compressed module under the
# lower's /lib/modules; we need libkmod-aware insmod to load it before
# the overlay mount in init.
RUN cp /usr/sbin/insmod /irfs/sbin/insmod \
    && for lib in $(ldd /usr/sbin/insmod \
            | awk '/=>/ {print $3} /ld-linux/ {print $1}' \
            | grep -v '^$'); do \
        mkdir -p "/irfs$(dirname "$lib")"; \
        cp "$lib" "/irfs$lib"; \
    done

# Copy the Ubuntu kernel modules needed for early-boot lower mounts.
# Files are prefixed 10-/20- so the init script can load them in
# sorted order (libcrc32c first, erofs second — erofs has a hard
# dependency on libcrc32c that insmod can't auto-resolve).
COPY --from=ubuntu-modules /stage/10-libcrc32c.ko* /irfs/lib/modules-extra/
COPY --from=ubuntu-modules /stage/20-erofs.ko*     /irfs/lib/modules-extra/
COPY --from=ubuntu-modules /stage/kver.txt         /irfs/lib/modules-extra/kver.txt

COPY initramfs/init /irfs/init
RUN chmod 0755 /irfs/init

RUN cd /irfs \
    && find . -print0 \
    | cpio --null --create --format=newc --quiet \
    | gzip -9 > /initrd.img

# Final tiny stage: just the artifact, so `--output type=local` produces
# only initrd.img without the build root tree.
FROM scratch AS shed-initramfs
COPY --from=shed-initramfs-builder /initrd.img /initrd.img
