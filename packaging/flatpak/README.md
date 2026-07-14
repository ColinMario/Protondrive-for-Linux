# Flatpak packaging

This directory contains Flatpak packaging metadata for Protondrive-for-Linux.

The Flatpak packages only this GPLv3 wrapper. It does not redistribute Proton's
official `proton-drive` binary and it does not vendor rclone. When the wrapper
runs inside Flatpak, it can call host-installed `proton-drive` and `rclone`
through `flatpak-spawn --host`, or it can use helper binaries installed by
`protondrive bootstrap`.

## Build locally

Install the Flatpak builder tooling and the Go SDK extension:

```bash
flatpak install flathub org.freedesktop.Platform//25.08 org.freedesktop.Sdk//25.08 org.freedesktop.Sdk.Extension.golang//25.08
```

Build from the repository root:

```bash
flatpak-builder --force-clean --user --install-deps-from=flathub \
  build-dir packaging/flatpak/io.github.colinmario.protondriveforlinux.devel.yml
```

Install locally:

```bash
flatpak-builder --user --install --force-clean --install-deps-from=flathub \
  build-dir packaging/flatpak/io.github.colinmario.protondriveforlinux.devel.yml
```

Run:

```bash
flatpak run io.github.colinmario.protondriveforlinux --help
```

Bootstrap helper tools after installation:

```bash
flatpak run io.github.colinmario.protondriveforlinux bootstrap --all --yes
```

This downloads Proton's official `proton-drive` binary from Proton's download
index and verifies the published SHA-512 checksum. It downloads rclone from the
current upstream GitHub release and verifies the release `SHA256SUMS` file. The
tools are installed into a managed per-user directory and are picked up
automatically when they are not available on the host `PATH`.

## Runtime requirements

For the Proton backend, either run `protondrive bootstrap --proton-drive --yes`
or install Proton's official CLI on the host:

```bash
proton-drive version
```

For mounts and rclone-specific sync flags, either run
`protondrive bootstrap --rclone --yes` or install rclone on the host:

```bash
rclone version
```

The wrapper resolves those host tools through `flatpak-spawn --host` when they
are not available inside the Flatpak sandbox.

## Notes for Flathub

- The app ID is `io.github.colinmario.protondriveforlinux`.
- The manifest uses `-mod=vendor`; keep `vendor/` up to date with
  `go mod vendor` before submitting or tagging Flatpak builds.
- The package is intentionally not branded as an official Proton package.
- The package does not run network downloads during Flatpak installation. The
  explicit `bootstrap` command is used for dependency downloads so the user can
  see and approve executable code being installed.
- The main manifest builds the immutable `v0.3.0` Git tag. The `.devel.yml`
  manifest is intentionally local-only and must not be submitted to Flathub.

## Security boundary

This command-line application needs read/write access to user-selected files.
The default manifest therefore grants home-directory access. It also grants
access to `org.freedesktop.Flatpak` so the wrapper can run explicitly selected
host helpers through `flatpak-spawn --host`. Such host commands run outside the
Flatpak sandbox. This package is a distribution format, not a security boundary.

Users who only transfer a dedicated directory can reduce access after install:

```bash
flatpak override --user --nofilesystem=home \
  --filesystem="$HOME/ProtonTransfers" \
  io.github.colinmario.protondriveforlinux
```

FUSE mounts and persistent systemd/OpenRC services integrate more reliably when
the native archive, Debian, or RPM package is used instead of Flatpak.
