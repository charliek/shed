# shed-build-tools

Controlled environment for shed's image-build pipeline. See
[`docs/reference/build-tools.md`](../docs/reference/build-tools.md) for
the user-facing reference.

This directory contains:

- `Dockerfile` — builds the `ghcr.io/charliek/shed-build-tools:<tag>`
  OCI image. The image carries pinned versions of the binaries shed
  invokes during image publish (currently `mkfs.erofs`, `dump.erofs`,
  `fsck.erofs` from `erofs-utils`).

## Building locally

```sh
make build-tools                              # builds shed-build-tools:dev
docker run --rm shed-build-tools:dev --help   # mkfs.erofs help (default entrypoint)
```

## Bumping the erofs-utils pin

1. Pick a release at <https://github.com/erofs/erofs-utils/tags>.
2. Update `ARG EROFS_UTILS_VERSION=` in `Dockerfile`.
3. Rebuild: `make build-tools`.
4. Sanity check the produced binary:

   ```sh
   # Make sure the bug class we care about is fixed: write an erofs
   # with the flags shed publishes with, then confirm dump.erofs
   # reports the big_pcluster feature when inodes use it.
   docker run --rm -v /tmp:/tmp shed-build-tools:dev \
       -b 4096 -z lz4 -E force-inode-compact -T 0 \
       /tmp/test.erofs --tar=f <(echo "")
   docker run --rm --entrypoint /usr/local/bin/dump.erofs \
       -v /tmp:/tmp shed-build-tools:dev /tmp/test.erofs | grep features
   ```

5. Commit the bump alongside any consumer changes that depend on it.
   The bump rides the next shed release.

## Consumers

No live consumers in this commit. The image is being introduced ahead
of the shed image-build pipeline switch that will use it (the change
needs the image to exist on `ghcr.io` first so it can be `FROM`'d /
`docker run`'d from CI). Once consumers land:

- They will pin the image tag to the current shed version via a
  `BUILD_TOOLS_VERSION` build arg, mirroring the `SHED_EXT_VERSION`
  pattern already used for shed-extensions.
- This README should grow a list of every consumer file so binary
  renames / entrypoint changes here can be traced forward.

If you change the `ENTRYPOINT` or rename a binary, update this list
as part of the consumer change.
