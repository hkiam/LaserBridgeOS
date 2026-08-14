# Build

## Supported host

The image build runs in an amd64 Docker container and therefore works on Linux
and Docker Desktop on macOS, including Apple Silicon through emulation.

```sh
make image
```

The build assembles an Alpine root with `apk`, creates a slot-aware initramfs,
compresses the root as SquashFS, creates standalone FAT32/ext4 filesystem
images, and places two initial root slots into a deterministic GPT disk image.
It does not require loop devices, privileged containers, or host mounts beyond
the output directory.

Set `VERSION` and `SOURCE_DATE_EPOCH` for a release:

```sh
make image VERSION=0.1.0 SOURCE_DATE_EPOCH=1786579200
```

Build inputs are pinned in `build/versions.env`. Alpine repositories are
version-scoped, but exact package archives remain controlled by the upstream
repository. Alpine does not package ustreamer, so the builder compiles its
pinned upstream commit and copies only the stripped binary plus runtime
libraries into the image. Filesystem timestamps, GUIDs, the ext4 directory
hash seed, SquashFS creation time, and gzip metadata are fixed by
`SOURCE_DATE_EPOCH`; release checksums identify the resulting artifact.

The output directory contains the raw disk image, its gzip-compressed form, an
unsigned per-version update bundle, and checksums:

```text
dist/LaserBridgeOS-x86_64.img
dist/LaserBridgeOS-x86_64.img.gz
dist/LaserBridgeOS-x86_64-VERSION.lbu
dist/SHA256SUMS
```

An update cannot be installed merely because it was produced by the build.
Sign the exact bundle with an SSH key trusted by the target appliance:

```sh
scripts/sign-update.sh \
  dist/LaserBridgeOS-x86_64-VERSION.lbu \
  laserbridge_ed25519
```

This creates the adjacent `.sig` file expected by the System page. Signing is
intentionally separate from the reproducible build, so no private release or
device-owner key enters the container or artifact.

## Developer checks

```sh
make test       # Go unit/API tests
make check      # go vet and shell syntax checks
make shellcheck # Dockerized ShellCheck
```
