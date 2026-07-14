# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the versions adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-07-14
### Added
- Added explicit safe `copy` and destructive `mirror` transfer operations with confirmation, deletion limits, source guards, and automatic destination backup directories.
- Added strict versioned sync-config validation, collision-resistant state filenames, JSON status output with stable exit codes, and build/version metadata.
- Added authenticated macOS WebDAV mounts with private logs and process identity tracking.
- Added Linux/macOS CI, security scanners, cross-builds, packaging validation, Dependabot, reproducible GoReleaser archives, SPDX SBOMs, Sigstore checksum signatures, and GitHub build attestations.
- Added security, contribution, notice, and full GPL-3.0 documentation.

### Changed
- Updated the supported Go baseline to 1.25 and refreshed all Go dependencies and vendored sources.
- Validated the private session bridge against Proton Drive CLI 0.5.0 while retaining 0.4.x compatibility and fail-closed handling for future format series.
- Changed rclone transfers to non-destructive copy semantics by default; exact destination mirroring now requires `--operation mirror --confirm-mirror`.
- Watch mode now retries failures with exponential backoff and remains available for later changes.
- Persistent systemd/OpenRC services now use atomic files, exact artifact/state rollback, update restarts, readiness checks, verified removal, and propagated errors.
- Flatpak release builds now use an immutable Git tag; local source builds use a separate development manifest. Flatpak host/home permissions are documented as a distribution tradeoff rather than a sandbox boundary.
- Flatpak release builds now embed the tagged application version and deterministic release metadata instead of reporting a development build.

### Fixed
- Fixed documented rclone passthrough by consuming the wrapper's standalone `--` delimiter.
- Fixed `status` returning success for missing remotes or failed authentication.
- Kept machine-readable status output single-shot by suppressing duplicate top-level error rendering.
- Prevented transient network errors from triggering destructive credential reconfiguration.
- Prevented one-time 2FA codes from being persisted or reused by staging them in a private temporary rclone config, and migrates legacy vaults when unlocked.
- Made rclone config, session, vault, and state writes atomic with cooperative locks, backups, and verification rollback.
- Refused transactional edits of encrypted rclone configs instead of risking plaintext replacement or corruption.
- Bounded ZIP extraction by bytes actually read instead of trusting archive metadata, and escaped systemd specifier and environment expansion in generated service paths.
- Blocked passthrough/config overrides of dry-run, deletion limits, backup handling, error handling, and other wrapper-owned mirror safeguards.
- Validated remote names before config rendering so colon ambiguity and INI section injection cannot redirect or corrupt rclone configuration.
- Rejected mirror backup directories nested inside the destination, including root-mirror recursion on the same remote.
- Removed the local vault passphrase from every child-process environment, including Flatpak host bridges.
- Enforced HTTPS-only dependency downloads and redirects, constrained Proton CLI assets to the official download path, and strictly validated rclone release versions.
- Made binaries, archives, native packages, and SPDX SBOMs reproducible from commit-derived timestamps and artifact digests; CI/release builds pin patched Go 1.25.12 and 1.26.5 toolchains, and tags fail unless they are SemVer, reachable from `main`, represented in the changelog, and matched by the immutable Flatpak source.
- Rejected conflicting credential sources, session-import modes, and unexpected positional arguments instead of silently ignoring them.
- Made empty-source checks ignore the sentinel itself, require regular-file sentinels, count actual remote files, and resolve symlink aliases before root/backup safety decisions.
- Expanded state/vault filename hashes to 64 bits and removes legacy unhashed files after a successful migration.
- Resolved sync, mount, unmount, backup, and persistent-service config paths to stable absolute locations.
- Prevented mount passthrough from overriding daemon/state, authentication, cache, read-only, config, and remote-control settings managed by the wrapper.
- Restricted recorded mount cleanup to generated direct-child bcrypt files so corrupted metadata cannot remove unrelated files.
- Removed the macOS Keychain fallback that exposed session JSON through process arguments.
- Prevented stale mount PIDs from signaling unrelated processes and verifies WebDAV shutdown after unmount.
- Removed the macOS WebDAV password from `mount_webdav` arguments by supplying it through a private pseudo-terminal conversation instead.
- Canonicalized mount-table paths so macOS `/var`/`/private/var` and other symlinked mount points are recognized correctly.

## [0.2.5] - 2026-06-29
### Fixed
- Prevented `auto` sync from silently falling back to `rclone sync` when Proton's official CLI is unavailable, avoiding unintended mirror deletion semantics.
- Avoided passing Proton credentials through rclone process arguments during configuration.
- Fixed Flatpak host-tool environment forwarding so host rclone and proton-drive can use the intended config and cache paths.
- Fixed Flatpak unmount handling by routing host unmount helpers through `flatpak-spawn --host`.
- Made persistent mount removal idempotent when the user service is already stopped.
- Increased bootstrap download timeout for slow GitHub release asset downloads while retaining download and archive size limits.

### Changed
- Renamed the Flatpak app ID and metadata files to the lowercase ID `io.github.colinmario.protondriveforlinux`.

## [0.2.4] - 2026-06-29
### Added
- Added OpenRC user-service support for persistent Linux mounts via `--persist-manager openrc`.

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
- Added Flatpak packaging metadata for `io.github.colinmario.protondriveforlinux`, including AppStream metadata, a desktop entry, and a neutral project icon.
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
