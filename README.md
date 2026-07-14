# Protondrive-for-Linux

![Go Version](https://img.shields.io/badge/Go-1.25%2B-00ADD8.svg) ![Proton Drive CLI](https://img.shields.io/badge/Proton%20Drive%20CLI-0.5.x-6d4aff.svg) ![rclone](https://img.shields.io/badge/rclone-copy%2Fmirror%2Fmount-5d2fbe.svg) ![License](https://img.shields.io/badge/License-GPLv3-green.svg)

> This is an unofficial community project and is not affiliated with Proton AG or Proton Drive.

Go-based convenience CLI for Proton Drive on Linux and other POSIX shells. The tool prefers Proton's official `proton-drive` CLI for login, browsing, uploads, and downloads, while retaining rclone for mounts and controlled copy/mirror workflows. Transfers are non-destructive by default; destination deletion is never implied by the command name.

## What changed

Proton released an official Drive CLI backed by the Proton Drive SDK. It supports browser-based login, filesystem listing, upload/download, sharing, invitations, JSON output, and OS secret-store sessions.

This repository now supports three backend modes:

| Backend | Purpose | When it is used |
| --- | --- | --- |
| `auto` | Default. Prefer official `proton-drive` where the command maps cleanly. | `configure`, `status`, `browse`, and simple `sync` transfers when `proton-drive` is installed. |
| `proton` | Force Proton's official CLI. | Use this when you want SDK-backed auth and transfers without rclone config. |
| `rclone` | Force the legacy rclone backend. | Required for `mount`, custom rclone remotes, `--dry-run`, extra rclone flags, and explicit mirroring. |

The upstream Proton Drive SDK is still evolving and Proton documents direct SDK usage as not yet ready for commercial/production third-party apps. For that reason this project integrates through the official `proton-drive` CLI instead of compiling directly against unstable SDK internals.

## Requirements

- Go 1.25 or 1.26 to build this wrapper.
- Proton's official Drive CLI for the modern backend. You can install it manually from [proton.me/download/drive/cli](https://proton.me/download/drive/cli/index.html) or run `protondrive bootstrap --proton-drive`.
- A Proton account and browser access for `proton-drive auth login`.
- Linux secret storage for Proton's CLI sessions, for example libsecret with GNOME Keyring or KWallet.
- Linux `secret-tool` from `libsecret-tools` when importing Proton's official CLI session into rclone.
- rclone only when you need `mount`, named rclone remotes, rclone dry-runs, or rclone-specific sync flags.
- Linux: FUSE/fusermount when using `mount`.
- macOS: no macFUSE is required for the default mount path; `mount` uses rclone WebDAV plus the system `mount_webdav` and `expect` tools in `auto` mode. Install/approve macFUSE only if you force `--mount-method fuse`.

## Installation

```bash
git clone https://github.com/ColinMario/Protondrive-for-Linux.git
cd Protondrive-for-Linux
go build ./cmd/protondrive
sudo install -m 0755 protondrive /usr/local/bin/protondrive
```

Install helper dependencies automatically:

```bash
# Downloads proton-drive with Proton's SHA-512 checksum verification.
# Downloads rclone from its GitHub release and verifies SHA256SUMS.
protondrive bootstrap --all --yes
```

Or install Proton's official CLI manually:

```bash
# Example for Linux x64. Verify the current version and checksum on Proton's download page.
curl -L -o proton-drive https://proton.me/download/drive/cli/0.5.0/linux-x64/proton-drive
chmod +x proton-drive
sudo install -m 0755 proton-drive /usr/local/bin/proton-drive
```

## Flatpak

Flatpak packaging metadata is available under `packaging/flatpak`.

The Flatpak packages only this GPLv3 wrapper. It does not redistribute Proton's official `proton-drive` binary or rclone. Inside the sandbox, the wrapper can call host-installed `proton-drive` and `rclone` through `flatpak-spawn --host`.

Build locally:

```bash
flatpak install flathub org.freedesktop.Platform//25.08 org.freedesktop.Sdk//25.08 org.freedesktop.Sdk.Extension.golang//25.08
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
flatpak run io.github.colinmario.protondriveforlinux bootstrap --all --yes
```

`bootstrap` installs `proton-drive` and rclone into a managed per-user directory
and future runs use those binaries automatically if they are not available on
the host `PATH`. Proton's binary is resolved from Proton's download index and
verified against Proton's published SHA-512 checksum. rclone is resolved from
the current upstream release and verified against the release `SHA256SUMS`.

See [packaging/flatpak/README.md](packaging/flatpak/README.md) for packaging notes and Flathub submission details.

The Flatpak needs home-directory access for arbitrary CLI transfer paths and
uses `flatpak-spawn --host` for explicitly selected host helpers. Host commands
run outside the sandbox, so the package is a distribution format rather than a
security boundary. Native release archives/packages are recommended for FUSE
mounts and persistent system services.

## Quick start

```bash
# Browser login through Proton's official CLI
protondrive configure

# Optional: reuse that Proton CLI session for rclone mounts
protondrive --backend rclone configure --from-proton-cli-session

# Check CLI version and auth/listing state
protondrive status --details
protondrive status --json

# Inspect the wrapper build itself
protondrive version --json

# Browse folders in /my-files
protondrive browse

# Browse files in a folder
protondrive browse --remote-path Documents --files

# Upload a local folder using Proton's CLI backend
protondrive sync ~/Documents --remote-path /my-files/backups --conflict-strategy merge

# Download from Proton Drive
protondrive sync ~/Downloads/ProtonMirror --direction download --remote-path /my-files/backups/Documents --conflict-strategy merge
```

## Backend selection

```bash
protondrive --backend auto status
protondrive --backend proton browse --remote-path /my-files
protondrive --backend rclone sync ~/Documents --remote-path backups --dry-run
```

Global options:

| Option | Environment variable | Default |
| --- | --- | --- |
| `--backend auto\|proton\|rclone` | `PROTONDRIVE_BACKEND` | `auto` |
| `--proton-drive-bin <path>` | `PROTONDRIVE_PROTON_BIN` | `proton-drive` |
| `--rclone-bin <path>` | `PROTONDRIVE_RCLONE_BIN` | `rclone` |
| `--remote <name>` | n/a | `protondrive` |

Using a custom `--remote` selects the rclone backend automatically because Proton's official CLI manages its own account session instead of named remotes.

`status` is suitable for monitoring. It verifies authentication even without
`--details` and uses stable exit codes: `0` healthy, `3` not configured, `4`
authentication failed, and `5` backend/tool failure. Use `--json` for structured
output or `--informational` when a human-readable check must always exit zero.

## CLI usage

```text
protondrive [--backend auto|proton|rclone] [--remote name] <command> [options]
```

| Command | Proton backend | rclone backend |
| --- | --- | --- |
| `configure` | Runs `proton-drive auth login`. | Creates/updates an rclone Proton Drive remote. |
| `status` | Prints official CLI version and verifies Drive listing. | Verifies the configured rclone remote and authentication. |
| `browse` | Runs `proton-drive filesystem list`. Defaults to `/my-files`. | Runs `rclone lsd` or `rclone ls`. |
| `sync` | Runs non-destructive `filesystem upload` or `filesystem download`. | Runs `rclone copy` by default; `rclone sync` only for explicit mirror operations. |
| `mount` | Not supported by Proton's CLI. | Runs `rclone mount` on Linux and rclone WebDAV + `mount_webdav` by default on macOS. |
| `unmount` | Uses OS unmount helpers. | Uses OS unmount helpers. |
| `configs` | Backend-independent JSON templates. | Backend-independent JSON templates. |

### Configure

Modern login:

```bash
protondrive configure
```

Legacy rclone remote:

```bash
protondrive --backend rclone configure --email alice@proton.me --store-credentials
```

Two-password Proton accounts can pass the mailbox password explicitly:

```bash
{
  printf '%s\n' "$PROTON_PASSWORD"
  printf '%s\n' "$PROTON_MAILBOX_PASSWORD"
} | protondrive --backend rclone configure --email alice@proton.me \
    --password-stdin --mailbox-password-stdin --non-interactive
```

Browserless/headless setup for servers:

```bash
printf '%s\n' "$PROTON_PASSWORD" |
  protondrive configure --headless --email alice@proton.me --password-stdin
```

With 2FA and the local credential vault:

```bash
{
  printf '%s\n' "$PROTON_PASSWORD"
  printf '%s\n' "$PROTON_MAILBOX_PASSWORD"
  printf '%s\n' "$PROTON_2FA_CODE"
  printf '%s\n' "$PROTONDRIVE_VAULT_PASSPHRASE"
} | protondrive configure --headless --email alice@proton.me \
    --password-stdin --mailbox-password-stdin --twofa-stdin \
    --store-credentials --vault-passphrase-stdin
```

`--headless` never starts Proton's browser login. In `auto` mode it uses rclone's Proton password-auth flow to initialize cached Proton tokens, then writes a compatible official Proton CLI session into the OS secret store (`ch.proton.drive/drive-sdk-cli` / `auth-session`). That gives you both a working rclone mount remote and a browserless `proton-drive` CLI session for later `browse`/`sync` commands. The command behaves like `--non-interactive` and fails if required values are missing. Use `--skip-verify` only when `proton-drive` is not installed yet; rclone still performs one real listing so the session tokens can be captured.

One-time 2FA codes are used only through a private temporary rclone config for
the current verification request; the file is removed immediately afterward,
and the code is never retained in the normal rclone config or credential vault.
Unlocking an older vault once rewrites it with the current schema and removes
any legacy stored TOTP value.

The transactional config editor preserves unknown rclone remote options and
backs up plaintext configs before changing them. It deliberately refuses to
edit an rclone-obscured `RCLONE_ENCRYPT_V0` config (including configurations
unlocked through `RCLONE_CONFIG_PASS`) because rewriting encrypted content as
plaintext would corrupt the file. Import the session into a separate plaintext
config with mode `0600`, or manage that encrypted remote directly with rclone.

On headless Linux where no Secret Service is available, you can opt into Proton CLI's official plaintext file session mode:

```bash
export PROTON_DRIVE_UNSAFE_SECRETS=true
export PROTON_DRIVE_CACHE_DIR=/var/lib/proton-drive-cli
printf '%s\n' "$PROTON_PASSWORD" |
  protondrive configure --headless --email alice@proton.me --password-stdin --skip-verify
```

This writes `${PROTON_DRIVE_CACHE_DIR}/auth-session.json` with mode `0600`. Only use it on a machine/account where that file is protected by OS permissions.

Reuse Proton's official CLI session for rclone mounts without entering the account password again:

```bash
protondrive --backend proton configure
protondrive --backend rclone configure --from-proton-cli-session
```

Export an already-initialized rclone session back into Proton's official CLI secret store:

```bash
protondrive --backend rclone configure --from-rclone-session
```

The session import reads Proton's OS secret-store entry (`ch.proton.drive/drive-sdk-cli` / `auth-session`) and writes an rclone Proton Drive remote containing the SDK session fields rclone needs. It uses macOS Keychain on macOS and `secret-tool`/Secret Service on Linux. On macOS, private-session export requires Bun's Keychain API; the wrapper fails closed instead of putting session JSON in `security` process arguments. Private session bridging is version-gated to the validated Proton CLI 0.4.x/0.5.x format and fails closed for future format series. The old encrypted credential vault remains available for password-based rclone auto-refresh.

### Browse

```bash
protondrive browse                         # folders in /my-files
protondrive browse --files                 # files in /my-files
protondrive browse --all --remote-path /   # top-level Proton sections
protondrive --backend rclone browse --remote-path Shares/Photos --files
```

For the Proton backend, relative remote paths are resolved under `/my-files`. Absolute Proton CLI paths such as `/`, `/my-files`, `/shared-with-me`, and `/trash` are passed through.

### Sync and transfer workflows

The Proton backend maps to the official CLI transfer commands:

```bash
protondrive sync ~/Pictures/ToProton --remote-path /my-files/Photos --conflict-strategy merge
protondrive sync ~/Mirror --direction download --remote-path /my-files/Photos --folder-conflict-strategy merge --file-conflict-strategy replace
```

Proton conflict strategies:

- `--conflict-strategy merge|keep-both|replace|skip`
- `--file-conflict-strategy merge|keep-both|replace|skip` for uploads
- `--file-conflict-strategy keep-both|replace|skip` for downloads
- `--folder-conflict-strategy merge|keep-both|replace|skip`
- `--skip-thumbnails` for uploads

Use rclone when you need dry-runs, passthrough flags, or an explicitly destructive mirror. The safe default is `rclone copy`:

```bash
# Non-destructive copy; files missing locally remain on Proton Drive
protondrive --backend rclone sync ~/Documents --remote-path backups --dry-run

# Inspect an exact mirror first
protondrive --backend rclone sync ~/Documents --remote-path backups \
  --operation mirror --dry-run

# Apply it with deletion limit, source sentinel, and automatic backup directory
protondrive --backend rclone sync ~/Documents --remote-path backups \
  --operation mirror --confirm-mirror --max-delete 25 \
  --source-sentinel .protondrive-source
```

In `auto` mode, simple upload/download transfers require Proton's official CLI.
When rclone is selected, the wrapper still uses non-destructive copy semantics
unless `--operation mirror` is present. A live mirror additionally requires
`--confirm-mirror`; remote/filesystem roots and empty sources are rejected by
default. Each mirror is limited to 25 deletions unless changed with
`--max-delete`, and replaced/deleted destination files are moved to an automatic
timestamped backup directory unless `--no-backup-dir` is explicitly supplied.
The backup must be outside the destination; a same-remote root mirror therefore
requires either a backup on another remote or explicit `--no-backup-dir`.
Sentinel files must be regular files and do not count as source content, so a
marker-only or directory-only source is still blocked as empty.

The standalone `--` delimiter is consumed by the wrapper, so passthrough flags
are forwarded correctly. Destructive rclone deletion flags are rejected for
copy operations. Passthrough cannot override wrapper-owned dry-run, deletion
limit, backup, error-handling, or in-place safeguards.

Watch mode works with both upload backends by rerunning the selected transfer
after filesystem changes settle. Failed runs retry with bounded exponential
backoff; a later filesystem event can recover the watcher after retries are
exhausted. Proton watch mode requires an explicit conflict strategy.

```bash
protondrive sync ~/Paperless/export --remote-path /my-files/Backups/Paperless --watch --watch-debounce 45s
```

### Mounting Proton Drive

Proton's official CLI does not currently expose a mount command. Mounting still requires rclone.

If you authenticated through Proton's official CLI, create the rclone mount remote from that session first:

```bash
protondrive --backend rclone configure --from-proton-cli-session
```

If you authenticated headlessly through rclone first, create the official Proton CLI session from that cached rclone session:

```bash
protondrive --backend rclone configure --from-rclone-session
```

On macOS, `auto` mode avoids stale or blocked macFUSE installations by serving
the rclone remote over authenticated localhost WebDAV and mounting it with
macOS `mount_webdav`. Each mount gets random credentials, a private log under
the wrapper config directory, a mode-`0600` bcrypt authentication file (removed
during cleanup or `unmount`), and recorded process/start identity so unmounting cannot
signal a reused PID. The password is supplied through a private pseudo-terminal
conversation and never appears in process arguments or environment variables:

```bash
protondrive --backend rclone mount ~/ProtonDrive
protondrive unmount ~/ProtonDrive
```

Force the classic FUSE path when you have a working macFUSE install:

```bash
protondrive --backend rclone mount ~/ProtonDrive --mount-method fuse
```

Linux continues to use FUSE/fusermount by default. `--mount-method webdav` is currently macOS-only.

### Persistent Linux mounts

Linux can keep Proton Drive mounted across user sessions through a generated user service. The CLI supports `systemd --user` and OpenRC user services. The service runs the rclone mount in foreground mode, restarts it if it crashes, and is enabled for future user sessions.

First make sure the rclone remote exists. The recommended path is to import the official Proton CLI session:

```bash
protondrive --backend proton configure
protondrive --backend rclone configure --from-proton-cli-session
```

Password-based rclone configuration remains available with `protondrive --backend rclone configure --email alice@proton.me --store-credentials`. For two-password Proton accounts, add `--mailbox-password` or `--mailbox-password-stdin`.
For fully browserless servers, use `protondrive configure --headless --email ... --password-stdin`; it writes the official Proton CLI session and the rclone remote in one run.

Install and start a persistent mount:

```bash
protondrive --backend rclone mount ~/ProtonDrive --persist --persist-name main-drive
```

By default, `--persist` auto-detects the service manager. You can choose one explicitly:

```bash
# systemd user service
protondrive --backend rclone mount ~/ProtonDrive --persist --persist-manager systemd --persist-name main-drive

# OpenRC user service
protondrive --backend rclone mount ~/ProtonDrive --persist --persist-manager openrc --persist-name main-drive
```

Useful variants:

```bash
# Mount only a subfolder
protondrive --backend rclone mount ~/ProtonBackups --remote-path Backups --persist --persist-name backups

# systemd only: allow the user service to start at boot before an interactive login
protondrive --backend rclone mount ~/ProtonDrive --persist --enable-linger

# Pass rclone mount tuning flags
protondrive --backend rclone mount ~/ProtonDrive --persist --rclone-flag --dir-cache-time=10m
```

Inspect the generated Linux service:

```bash
systemctl --user status protondrive-mount-main-drive.service
journalctl --user -u protondrive-mount-main-drive.service -f
```

Inspect the generated OpenRC user service:

```bash
rc-service --user protondrive-mount-main-drive status
tail -f "${XDG_RUNTIME_DIR}/protondrive-mount-main-drive.log"
```

Remove a persistent mount:

```bash
protondrive unmount ~/ProtonDrive --remove-persist --persist-name main-drive --force
```

Use `--persist-manager systemd` or `--persist-manager openrc` with `unmount --remove-persist` if you need to remove a specific service-manager backend.

Notes:

- `--persist` is Linux-only and requires either systemd user services or OpenRC user services.
- Without `--enable-linger`, a systemd mount starts when the user session starts. With lingering enabled, systemd can start the user service after boot without an active graphical/SSH login.
- OpenRC mounts use OpenRC user services under `${XDG_CONFIG_HOME:-~/.config}/rc/init.d`. They require `rc-service`, `rc-update`, `openrc-run`, and an OpenRC user session with `XDG_RUNTIME_DIR` set.
- `--persist` uses FUSE/fusermount on Linux. Install `fuse3`/`fusermount3` or the equivalent package for your distribution.
- Use `--rclone-flag` for extra rclone mount options. Positional passthrough flags are intentionally rejected for persistent units so the generated service stays deterministic.
- Wrapper-owned daemon, config, authentication, cache, read-only, and rclone remote-control flags cannot be overridden through mount passthrough.

## Reusable sync configs

The CLI stores JSON configs under `${XDG_CONFIG_HOME:-~/.config}/protondrive/sync-configs`.

```bash
protondrive configs list
protondrive configs init paperless-ngx-export
protondrive configs show paperless-ngx-export
protondrive sync --config paperless-ngx-export
```

Schema version 1 accepts `name`, `description`, `local_path`, `remote_path`,
`direction`, `operation`, `watch`, `watch_debounce`, `max_delete`, `backup_dir`,
`source_sentinel`, `allow_empty_source`, and `extra_rclone_args`. Unknown fields,
trailing JSON, unsafe sentinel paths, and destructive copy flags are rejected.
Version-less files from releases through 0.2.5 remain readable but migrate to
schema version 1 and safe `copy` behavior. Wrapper-owned mirror safety flags
cannot be overridden in `extra_rclone_args`. If `extra_rclone_args` is present,
`auto` selects rclone.

## Configuration locations

| Path | Purpose |
| --- | --- |
| `${XDG_CONFIG_HOME:-~/.config}/protondrive` | Wrapper metadata. |
| `*-<hash>.creds` | Encrypted credential vault for rclone mode. Legacy names are migrated on use. |
| `*-<hash>.state` | Last auth and mount metadata. The hash prevents remote-name collisions. |
| `sync-configs/*.json` | User sync config files. |

Proton's official CLI stores cache, app data, logs, and credentials in its own OS-specific locations. See [Using Proton Drive CLI](https://proton.me/support/drive-cli) for the current upstream details.

## Development

```bash
go fmt ./...
go vet ./...
go test -race ./...
go build ./cmd/protondrive
```

GitHub CI runs the suite on macOS and Linux with Go 1.25/1.26, checks the race
detector, formatting, vet, Staticcheck, Gosec, govulncheck, dependency/vendor
consistency, cross-builds, coverage floor, and Flatpak/AppStream metadata.

Manual integration checks:

```bash
proton-drive version
protondrive --backend proton status --details
protondrive --backend rclone configure --from-proton-cli-session --skip-verify
protondrive --backend rclone status --details
```

## Troubleshooting

- `proton-drive not found`: install Proton's official CLI or set `--proton-drive-bin`.
- Proton backend says it is not authenticated: run `protondrive --backend proton configure`.
- Headless Proton CLI setup fails before writing a session: rclone must complete one real listing so it can cache `client_uid`, access token, refresh token, and key password. Check credentials, 2FA, network access, and Proton rate limits.
- `--from-proton-cli-session` cannot find a session: run `protondrive --backend proton configure` first, then ensure macOS Keychain or Linux Secret Service is unlocked. On Linux, install `libsecret-tools` so `secret-tool` is available.
- `rclone not found`: install rclone or set `--rclone-bin`; rclone is still required for mounts.
- Encrypted rclone config is refused: this wrapper never rewrites `RCLONE_ENCRYPT_V0` files. Select a separate plaintext `RCLONE_CONFIG` protected with mode `0600`, or configure the encrypted remote directly with rclone.
- macOS mount fails with macFUSE errors: use the default `--mount-method auto` or explicitly pass `--mount-method webdav`; this avoids macFUSE.
- macOS WebDAV mount reports that `expect` is unavailable: restore the system `/usr/bin/expect` tool or use a working `--mount-method fuse`; the wrapper will not fall back to exposing the WebDAV password in arguments.
- Linux/FUSE mounts never become ready: rerun with `protondrive --backend rclone mount --foreground` and check FUSE/fusermount.
- Proton upload/download prompts for conflicts: pass one of the conflict strategy flags for non-interactive automation.

## License

GPLv3. See [LICENSE](LICENSE).
