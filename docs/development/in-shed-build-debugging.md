# In-shed build debugging

The publish-images workflow only runs on tag push to GitHub. When the
flow regresses (a stale `--source-ref`, a `--platform` mistake, a
buildx-driver compatibility break) the only feedback signal is a slow
CI failure. This page documents how to reproduce that flow locally
inside a shed so you can iterate in minutes instead of hours.

## Why a shed (and not the host)?

The publish flow targets `linux/arm64` rootfs images. Building those on
an Apple Silicon Mac through `docker buildx --platform linux/arm64`
works in theory, but the rest of the flow assumes a Linux host: the
shed CLI shells out to `mkfs.ext4`, `mksquashfs`, and other tools that
either don't exist or behave differently on macOS. Running everything
inside a Linux shed eliminates that drift; the only difference between
`scripts/publish-images-local.sh` and the GitHub Actions job is the
target registry.

## The buildx network-host trick

`docker buildx create` defaults to the `docker-container` driver, which
runs BuildKit inside a container with its own network namespace. When
the build needs to reach a registry on the shed's loopback (the local
`registry:2` we spin up to receive the push), the default driver
cannot: `localhost:5050` inside BuildKit refers to BuildKit's own
container, not the shed.

The one-line fix is to ask the driver to share the shed's network
namespace:

```bash
docker buildx create \
  --driver docker-container \
  --driver-opt network=host \
  --use
```

That option does two things:

1. Sets `--network host` on the BuildKit container so it shares the
   shed's loopback with the registry.
2. Boots BuildKit with `--allow-insecure-entitlement=network.host`,
   which is also needed for any `RUN --network=host` directives in the
   Dockerfile.

You can confirm both by running `docker buildx inspect <name>` and
looking for `Driver Options: network="host"` plus
`BuildKit daemon flags: --allow-insecure-entitlement=network.host`.

## One-shot validation

The publish-flow simulation is wrapped up in
`scripts/publish-images-local.sh`. It boots a buildx builder, a
local `registry:2`, builds the shed-overlay initramfs, and then for
each variant:

1. Runs `shed image build` with `--source-ref` and `--platform` set
   exactly the way the publish workflow does.
2. Asserts the on-disk manifest's `io.shed.source-ref` annotation
   matches the registry ref we're about to push to.
3. Runs `shed image push --local` to stream the OCI layout to the
   in-shed registry.
4. Fetches the manifest back from the registry over HTTP and asserts
   the annotation survived.
5. For the first variant, runs `shed image pull` into a brand-new OCI
   store and re-checks the annotation; this is what a remote
   shed-server does when serving `shed create foo --image base` after
   a publish.

The exit code is the verdict: `0` for PASS, `1` for FAIL. The line
preceding `FAIL:` identifies which variant tripped.

## Setup

1. Boot a validation shed with enough disk for buildx + intermediate
   layers. The `full` image variant has docker + buildx pre-installed;
   `--upper-size 40G` is the minimum that comfortably fits a single
   variant build (a buildx builder image, `registry:2`, the ~3 GB
   intermediate ext4 rootfs, plus headroom). For all three variants
   in one run, use 80 GB.

   ```bash
   shed create publish-test --image full --cpus 4 --memory 8192 \
                            --upper-size 40G
   ```

2. Open SSH to the shed:

   ```bash
   shed ssh-config publish-test > /tmp/publish-test-ssh
   ssh -F /tmp/publish-test-ssh shed-publish-test
   ```

3. Neutralise the docker credential helper. The shed-agent inside the
   shed installs a `credsStore` that talks to the host over vsock;
   inside a validation shed there is no host to answer, so any
   `docker pull` / `docker push` hangs forever waiting for creds.

   ```bash
   echo '{}' > "$HOME/.docker/config.json"
   ```

4. Cross-build the in-VM binaries on the host and stage everything
   into the shed:

   ```bash
   # On the host:
   GOOS=linux GOARCH=arm64 go build -o vz/shed-agent     ./cmd/shed-agent
   GOOS=linux GOARCH=arm64 go build -o vz/shed-firstboot ./cmd/shed-firstboot
   GOOS=linux GOARCH=arm64 go build -o /tmp/shed         ./cmd/shed
   scp -F /tmp/publish-test-ssh -r . shed-publish-test:/home/shed/work
   scp -F /tmp/publish-test-ssh /tmp/shed shed-publish-test:/tmp/shed
   ssh -F /tmp/publish-test-ssh shed-publish-test \
       'sudo install -m 0755 /tmp/shed /usr/local/bin/shed'
   ```

5. Run the validation script inside the shed:

   ```bash
   ssh -F /tmp/publish-test-ssh shed-publish-test \
       'cd /home/shed/work && ./scripts/publish-images-local.sh'
   ```

   To exercise only one variant (faster on a small upper layer):

   ```bash
   ssh -F /tmp/publish-test-ssh shed-publish-test \
       'cd /home/shed/work && VARIANTS=base ./scripts/publish-images-local.sh'
   ```

## Interpreting results

PASS line:

```
PASS: built, pushed, and round-tripped 3 variant(s) at v0.0.0-local
```

means: every requested variant built successfully, the OCI manifest
written by `shed image build` carried the right
`io.shed.source-ref` annotation, the registry received the manifest
byte-perfect, and a fresh `shed image pull` into a clean store still
sees the same annotation.

FAIL lines come in a few flavours:

- `FAIL: base: local manifest source-ref mismatch ...` - the bug PR
  #94 fixed has come back; `shed image build` is baking the wrong
  annotation into the manifest. Check `--source-ref` handling in
  `cmd/shed/image.go` and the OCI conversion path in
  `internal/vmimage/convert.go`.
- `FAIL: base: registry-side source-ref mismatch ...` - the on-disk
  manifest is right but `shed image push` is rewriting it on the way
  out. Check the push handler in `internal/api/server.go` and
  `internal/vmimage/manager.go`.
- `FAIL: base: round-trip source-ref mismatch ...` - the push succeeded
  but `shed image pull` is dropping or rewriting the annotation. Check
  the OCI ingest path in `internal/vmimage/registry.go`.
- `FAIL: ... gzip: stdin: Input/output error` or
  `EXT4-fs error ... Remounting filesystem read-only` - your shed ran
  out of disk in the writable upper layer. Delete the shed and create
  a new one with a larger `--upper-size`.

## Gotchas

- The buildx builder and `registry:2` are torn down on script exit,
  even on failure. The OCI store at `/tmp/publish-images-local-store/`
  is left in place so you can inspect blobs and manifests after a
  failed run.
- If you forget the `network=host` driver-opt, the symptom is
  `dial tcp 127.0.0.1:5050: connect: connection refused` during the
  `shed image build` phase - BuildKit reaches its own loopback, not
  the shed's.
- The publish script runs its `registry:2` with `--network host` so the
  in-shed BuildKit (also `network=host`) reaches it at `127.0.0.1:5050`.
  (The `full` image now enables the default `docker0` bridge, so general
  `docker run` works without `--network host` — but this loopback-registry
  pattern still needs it.)
- The 'full' image variant has docker + buildx, but it does NOT have
  Go installed. Cross-build the shed CLI on the host and `scp` it in
  rather than trying to `go build` inside the shed.
- Don't be tempted to point the script at a real ghcr.io path with a
  fake credential; the `--local` push path doesn't authenticate, but
  the assertion step does an unauthenticated HTTP `GET` on the
  manifest. Run it against a local `registry:2` and only flip to
  `ghcr.io` once you actually want to publish.
