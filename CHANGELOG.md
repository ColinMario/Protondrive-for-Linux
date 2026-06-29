# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the versions adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.3] - 2026-06-29
### Added
- Added rclone configuration support for Proton two-password accounts via `--mailbox-password` and `--mailbox-password-stdin`.
- Added encrypted credential-vault storage and refresh support for the optional Proton mailbox password.

## [0.2.2] - 2026-06-29
### Added
- Added `protondrive bootstrap` to download helper dependencies into a managed user directory with checksum verification.
- Added automatic discovery of managed `proton-drive` and `rclone` binaries when they are not available on `PATH` or through the Flatpak host.

### Changed
- Missing dependency errors now point to the bootstrap command as the fastest setup path.

## [0.2.1] - 2026-06-29
### Added
- Added Flatpak packaging metadata for `io.github.ColinMario.ProtondriveForLinux`, including AppStream metadata, a desktop entry, and a neutral project icon.
- Added vendored Go dependencies so Flatpak builds can run with `-mod=vendor` and without fetching Go modules during the build.

### Changed
- The wrapper can resolve host-installed `proton-drive` and `rclone` through `flatpak-spawn --host` when running inside a Flatpak sandbox.

## [0.2.0] - 2026-06-29
### Added
- Added backend selection with `auto`, `proton`, and `rclone` modes plus binary overrides for `proton-drive` and `rclone`.
- Added support for Proton's official `proton-drive` CLI for browser login, status checks, browsing, and upload/download workflows.
- Added `configure --from-proton-cli-session` to import Proton's official CLI session from the OS secret store into an rclone Proton Drive remote for mounts.
- Added `configure --from-rclone-session` to export an initialized rclone Proton Drive session back into Proton's official CLI secret store.
- Added `configure --headless` for browserless password/2FA setup that initializes rclone tokens and writes a compatible official Proton CLI session to the OS secret store.
- Added support for Proton CLI's `PROTON_DRIVE_UNSAFE_SECRETS` plaintext session file mode for headless systems without Secret Service.
- Added Proton transfer conflict flags and thumbnail control for `sync` when using the official CLI backend.
- Added macOS WebDAV mounting through rclone `serve webdav` plus `mount_webdav`, avoiding stale or blocked macFUSE installs in default mount mode.
- Added Linux persistent mounts with `mount --persist`, generated `systemd --user` services, optional `--enable-linger`, and `unmount --remove-persist`.

### Changed
- Commands no longer require rclone globally before dispatch; backend checks now happen only for commands that need them.
- `auto` mode keeps rclone for mounts, custom remotes, dry-runs, and rclone passthrough flags while preferring Proton's CLI where supported.
- `sync` now accepts documented flag ordering such as `protondrive sync ~/Docs --remote-path /my-files/backups`.
- macOS force unmount now uses the modern `diskutil unmount force <mountpoint>` syntax.
- README now documents the official Proton Drive CLI, SDK status, backend tradeoffs, and rclone's remaining role.

## [0.1.0] - 2025-11-15
### Added
- Initial Protondrive CLI that wraps rclone for configuring, syncing, browsing, mounting, and unmounting Proton Drive remotes.
- Encrypted credential vault with automatic refresh plus remote state tracking for auth history and mounts.
- Watch-enabled sync workflows, reusable JSON config templates, and bundled examples (paperless-ngx-export, photo-drop-upload, shared-media-downloader).
- First comprehensive README covering installation, usage, templates, troubleshooting, and contribution guidelines.
