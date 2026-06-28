# Protondrive-for-Linux

![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8.svg) ![Proton Drive CLI](https://img.shields.io/badge/Proton%20Drive%20CLI-0.4.x-6d4aff.svg) ![rclone](https://img.shields.io/badge/rclone-optional%20mount%2Fsync-5d2fbe.svg) ![License](https://img.shields.io/badge/License-GPLv3-green.svg)

> This is an unofficial community project and is not affiliated with Proton AG or Proton Drive.

Go-based convenience CLI for Proton Drive on Linux and other POSIX shells. The tool now prefers Proton's official `proton-drive` CLI for login, browsing, uploads, and downloads, while keeping the existing rclone backend for features Proton's CLI does not yet provide, especially FUSE mounts and exact `rclone sync` workflows.

## What changed

Proton released an official Drive CLI backed by the Proton Drive SDK. It supports browser-based login, filesystem listing, upload/download, sharing, invitations, JSON output, and OS secret-store sessions.

This repository now supports three backend modes:

| Backend | Purpose | When it is used |
| --- | --- | --- |
| `auto` | Default. Prefer official `proton-drive` where the command maps cleanly. | `configure`, `status`, `browse`, and simple `sync` transfers when `proton-drive` is installed. |
| `proton` | Force Proton's official CLI. | Use this when you want SDK-backed auth and transfers without rclone config. |
| `rclone` | Force the legacy rclone backend. | Required for `mount`, custom rclone remotes, `--dry-run`, extra rclone flags, and exact rclone mirroring behavior. |

The upstream Proton Drive SDK is still evolving and Proton documents direct SDK usage as not yet ready for commercial/production third-party apps. For that reason this project integrates through the official `proton-drive` CLI instead of compiling directly against unstable SDK internals.

## Requirements

- Go 1.21+ to build this wrapper.
- Proton's official Drive CLI for the modern backend. Download it from [proton.me/download/drive/cli](https://proton.me/download/drive/cli/index.html) and place `proton-drive` on your `PATH`.
- A Proton account and browser access for `proton-drive auth login`.
- Linux secret storage for Proton's CLI sessions, for example libsecret with GNOME Keyring or KWallet.
- Linux `secret-tool` from `libsecret-tools` when importing Proton's official CLI session into rclone.
- rclone only when you need `mount`, named rclone remotes, rclone dry-runs, or rclone-specific sync flags.
- Linux: FUSE/fusermount when using `mount`.
- macOS: no macFUSE is required for the default mount path; `mount` uses rclone WebDAV plus macOS `mount_webdav` in `auto` mode. Install/approve macFUSE only if you force `--mount-method fuse`.

## Installation

```bash
git clone https://github.com/ColinMario/Protondrive-for-Linux.git
cd Protondrive-for-Linux
go build ./cmd/protondrive
sudo install -m 0755 protondrive /usr/local/bin/protondrive
```

Install Proton's official CLI separately:

```bash
# Example for Linux x64. Verify the current version and checksum on Proton's download page.
curl -L -o proton-drive https://proton.me/download/drive/cli/0.4.6/linux-x64/proton-drive
chmod +x proton-drive
sudo install -m 0755 proton-drive /usr/local/bin/proton-drive
```

## Quick start

```bash
# Browser login through Proton's official CLI
protondrive configure

# Optional: reuse that Proton CLI session for rclone mounts
protondrive --backend rclone configure --from-proton-cli-session

# Check CLI version and auth/listing state
protondrive status --details

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

## CLI usage

```text
protondrive [--backend auto|proton|rclone] [--remote name] <command> [options]
```

| Command | Proton backend | rclone backend |
| --- | --- | --- |
| `configure` | Runs `proton-drive auth login`. | Creates/updates an rclone Proton Drive remote. |
| `status` | Prints official CLI version and verifies Drive listing. | Checks the rclone remote and optional details. |
| `browse` | Runs `proton-drive filesystem list`. Defaults to `/my-files`. | Runs `rclone lsd` or `rclone ls`. |
| `sync` | Runs `filesystem upload` or `filesystem download`. | Runs `rclone sync`. |
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

Browserless/headless setup for servers:

```bash
printf '%s\n' "$PROTON_PASSWORD" |
  protondrive configure --headless --email alice@proton.me --password-stdin
```

With 2FA and the local credential vault:

```bash
{
  printf '%s\n' "$PROTON_PASSWORD"
  printf '%s\n' "$PROTON_2FA_CODE"
  printf '%s\n' "$PROTONDRIVE_VAULT_PASSPHRASE"
} | protondrive configure --headless --email alice@proton.me \
    --password-stdin --twofa-stdin --store-credentials --vault-passphrase-stdin
```

`--headless` never starts Proton's browser login. In `auto` mode it uses rclone's Proton password-auth flow to initialize cached Proton tokens, then writes a compatible official Proton CLI session into the OS secret store (`ch.proton.drive/drive-sdk-cli` / `auth-session`). That gives you both a working rclone mount remote and a browserless `proton-drive` CLI session for later `browse`/`sync` commands. The command behaves like `--non-interactive` and fails if required values are missing. Use `--skip-verify` only when `proton-drive` is not installed yet; rclone still performs one real listing so the session tokens can be captured.

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

The session import reads Proton's OS secret-store entry (`ch.proton.drive/drive-sdk-cli` / `auth-session`) and writes an rclone Proton Drive remote containing the SDK session fields rclone needs. It uses macOS Keychain on macOS and `secret-tool`/Secret Service on Linux. The old encrypted credential vault remains available for password-based rclone auto-refresh.

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

Use rclone when you need exact rclone mirror semantics, `--dry-run`, or passthrough flags:

```bash
protondrive --backend rclone sync ~/Documents --remote-path backups --dry-run
protondrive --backend rclone sync ~/Documents --remote-path backups -- --delete-after
```

Watch mode still works with both upload backends by rerunning the selected transfer after filesystem changes settle:

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

On macOS, `auto` mode avoids stale or blocked macFUSE installations by serving the rclone remote over localhost WebDAV and mounting it with macOS `mount_webdav`:

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

Linux can keep Proton Drive mounted across reboots through a `systemd --user` service generated by this CLI. The service runs the rclone mount in foreground mode, restarts it if it crashes, and is enabled for future user sessions.

First make sure the rclone remote exists. The recommended path is to import the official Proton CLI session:

```bash
protondrive --backend proton configure
protondrive --backend rclone configure --from-proton-cli-session
```

Password-based rclone configuration remains available with `protondrive --backend rclone configure --email alice@proton.me --store-credentials`.
For fully browserless servers, use `protondrive configure --headless --email ... --password-stdin`; it writes the official Proton CLI session and the rclone remote in one run.

Install and start a persistent mount:

```bash
protondrive --backend rclone mount ~/ProtonDrive --persist --persist-name main-drive
```

Useful variants:

```bash
# Mount only a subfolder
protondrive --backend rclone mount ~/ProtonBackups --remote-path Backups --persist --persist-name backups

# Allow the user service to start at boot before an interactive login
protondrive --backend rclone mount ~/ProtonDrive --persist --enable-linger

# Pass rclone mount tuning flags
protondrive --backend rclone mount ~/ProtonDrive --persist --rclone-flag --dir-cache-time=10m
```

Inspect the generated Linux service:

```bash
systemctl --user status protondrive-mount-main-drive.service
journalctl --user -u protondrive-mount-main-drive.service -f
```

Remove a persistent mount:

```bash
protondrive unmount ~/ProtonDrive --remove-persist --persist-name main-drive --force
```

Notes:

- `--persist` is Linux-only and requires systemd user services.
- Without `--enable-linger`, the mount starts when the user session starts. With lingering enabled, systemd can start the user service after boot without an active graphical/SSH login.
- `--persist` uses FUSE/fusermount on Linux. Install `fuse3`/`fusermount3` or the equivalent package for your distribution.
- Use `--rclone-flag` for extra rclone mount options. Positional passthrough flags are intentionally rejected for persistent units so the generated service stays deterministic.

## Reusable sync configs

The CLI stores JSON configs under `${XDG_CONFIG_HOME:-~/.config}/protondrive/sync-configs`.

```bash
protondrive configs list
protondrive configs init paperless-ngx-export
protondrive configs show paperless-ngx-export
protondrive sync --config paperless-ngx-export
```

Each JSON file can declare `name`, `description`, `local_path`, `remote_path`, `direction`, `watch`, `watch_debounce`, and `extra_rclone_args`. If `extra_rclone_args` is present, `auto` selects the rclone backend for that run.

## Configuration locations

| Path | Purpose |
| --- | --- |
| `${XDG_CONFIG_HOME:-~/.config}/protondrive` | Wrapper metadata. |
| `*.creds` | Legacy encrypted credential vault for rclone mode. |
| `*.state` | Last auth and mount metadata recorded by this wrapper. |
| `sync-configs/*.json` | User sync config files. |

Proton's official CLI stores cache, app data, logs, and credentials in its own OS-specific locations. See [Using Proton Drive CLI](https://proton.me/support/drive-cli) for the current upstream details.

## Development

```bash
go fmt ./...
go vet ./...
go test ./...
go build ./cmd/protondrive
```

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
- macOS mount fails with macFUSE errors: use the default `--mount-method auto` or explicitly pass `--mount-method webdav`; this avoids macFUSE.
- Linux/FUSE mounts never become ready: rerun with `protondrive --backend rclone mount --foreground` and check FUSE/fusermount.
- Proton upload/download prompts for conflicts: pass one of the conflict strategy flags for non-interactive automation.

## License

GPLv3. See [LICENSE](LICENSE).
