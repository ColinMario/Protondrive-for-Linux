# Flatpak packaging

This directory contains Flatpak packaging metadata for Protondrive-for-Linux.

The Flatpak packages only this GPLv3 wrapper. It does not redistribute Proton's
official `proton-drive` binary and it does not vendor rclone. When the wrapper
runs inside Flatpak, it can call host-installed `proton-drive` and `rclone`
through `flatpak-spawn --host`.

## Build locally

Install the Flatpak builder tooling and the Go SDK extension:

```bash
flatpak install flathub org.freedesktop.Platform//24.08 org.freedesktop.Sdk//24.08 org.freedesktop.Sdk.Extension.golang//24.08
```

Build from the repository root:

```bash
flatpak-builder --force-clean --user --install-deps-from=flathub \
  build-dir packaging/flatpak/io.github.ColinMario.ProtondriveForLinux.yml
```

Install locally:

```bash
flatpak-builder --user --install --force-clean --install-deps-from=flathub \
  build-dir packaging/flatpak/io.github.ColinMario.ProtondriveForLinux.yml
```

Run:

```bash
flatpak run io.github.ColinMario.ProtondriveForLinux --help
```

## Runtime requirements

For the Proton backend, install Proton's official CLI on the host:

```bash
proton-drive version
```

For mounts and rclone-specific sync flags, install rclone on the host:

```bash
rclone version
```

The wrapper resolves those host tools through `flatpak-spawn --host` when they
are not available inside the Flatpak sandbox.

## Notes for Flathub

- The app ID is `io.github.ColinMario.ProtondriveForLinux`.
- The manifest uses `-mod=vendor`; keep `vendor/` up to date with
  `go mod vendor` before submitting or tagging Flatpak builds.
- The package is intentionally not branded as an official Proton package.
- If a Flathub submission prefers a remote source instead of the local `dir`
  source, replace the source with a tagged Git source that contains `vendor/`.
