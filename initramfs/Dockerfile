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

FROM debian:bookworm-slim AS shed-initramfs-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
        busybox-static \
        e2fsprogs \
        cpio \
        gzip \
        kmod \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /irfs/bin /irfs/sbin /irfs/dev /irfs/proc /irfs/sys \
             /irfs/run /irfs/etc /irfs/newroot /irfs/upper /irfs/lower /irfs/mnt

# busybox provides sh, mount, mkdir, dd, od, etc. Symlink the applets
# we rely on so the initrd stays small.
RUN cp /bin/busybox /irfs/bin/busybox \
    && for cmd in sh mount umount mkdir mknod cat echo sleep chmod chown \
                  ls cp mv rm ln dd od tr grep cut head tail sed find uname \
                  switch_root pivot_root reboot poweroff blkid; do \
        ln -sf busybox /irfs/bin/$cmd; \
    done

# pivot_root applet exists in busybox; symlink in /sbin too for tools
# that look there. mkfs.ext4 ships outside busybox.
RUN ln -sf /bin/busybox /irfs/sbin/pivot_root \
    && cp /usr/sbin/mkfs.ext4 /irfs/sbin/mkfs.ext4 \
    && for lib in $(ldd /usr/sbin/mkfs.ext4 \
            | awk '/=>/ {print $3} /ld-linux/ {print $1}' \
            | grep -v '^$'); do \
        mkdir -p "/irfs$(dirname "$lib")"; \
        cp "$lib" "/irfs$lib"; \
    done

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
