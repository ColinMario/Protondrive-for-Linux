# Protondrive-for-Linux

![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8.svg) ![rclone](https://img.shields.io/badge/rclone-ProtonDrive-5d2fbe.svg) ![License](https://img.shields.io/badge/License-GPLv3-green.svg)

> ⚠️ This is an **unofficial** community project and is **not affiliated** with Proton AG or Proton Drive.

Go-based CLI that wraps the rclone Proton Drive backend so you can configure, sync, browse, and mount Proton Drive from any Linux shell (and other POSIX systems) without Python or desktop dependencies.

## Table of contents
- [Why Protondrive-for-Linux](#why-protondrive-for-linux)
- [Architecture at a glance](#architecture-at-a-glance)
- [Requirements](#requirements)
- [Installation](#installation)
- [Quick start](#quick-start)
- [CLI usage](#cli-usage)
  - [Configure the remote](#configure-the-remote)
  - [Inspect Proton Drive status](#inspect-proton-drive-status)
  - [Sync workflows](#sync-workflows)
  - [Mounting Proton Drive](#mounting-proton-drive)
  - [Managing reusable sync configs](#managing-reusable-sync-configs)
  - [Credential vault & automation](#credential-vault--automation)
- [Configuration & data locations](#configuration--data-locations)
- [Environment variables](#environment-variables)
- [Built-in sync templates](#built-in-sync-templates)
- [Development](#development)
- [Testing & QA](#testing--qa)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)
- [License](#license)

## Why Protondrive-for-Linux
- Single static binary handles configure/sync/mount flows for Proton Drive through rclone.
- Works anywhere rclone runs: Linux servers, containers, headless SBCs, or automated jobs.
- Ships with reusable JSON sync templates and optional filesystem watching powered by `fsnotify`.
- Offers an encrypted credential vault (AES-256-GCM + scrypt) for zero-touch re-authentication when Proton expires sessions.
- Records mount metadata so you can see when and where remotes are attached.

## Architecture at a glance
- `cmd/protondrive`: main CLI entrypoint built with the Go standard library and `fsnotify`.
- `internal/customconfigs`: embedded JSON templates that you can copy/edit via `protondrive configs`.
- External dependency: [`rclone`](https://rclone.org/) with the Proton Drive backend enabled (no direct Proton API calls).
- Credential vault + remote state live under `~/.config/protondrive` (or the OS-specific config dir).
- All heavy lifting (syncing, mounting) is delegated to `rclone`; this project adds ergonomic workflows around it.

## Requirements
- Linux or another POSIX environment supported by rclone (mount helpers require FUSE/fusermount on Unix, `mountvol` on Windows WSL).
- [Go 1.21+](https://go.dev/dl/) to build from source.
- [rclone](https://rclone.org/install/) compiled with the Proton Drive backend (`rclone version` shows `protondrive`).
- `fusermount`/`fusermount3` (Linux) or `diskutil` (macOS) when using the `mount` command.

## Installation
```bash
# clone the repository
git clone https://github.com/ColinMario/Protondrive-for-Linux.git
cd Protondrive-for-Linux

# build the CLI (outputs ./protondrive)
go build ./cmd/protondrive

# optional: install to /usr/local/bin (or another $PATH location)
sudo install -m 0755 protondrive /usr/local/bin/protondrive
```
To upgrade, pull the latest changes and rebuild:
```bash
git pull --ff-only
go build ./cmd/protondrive && sudo install -m 0755 protondrive /usr/local/bin/protondrive
```

## Quick start
1. Install rclone and ensure `rclone config` can create Proton Drive remotes.
2. Build/install `protondrive` as shown above.
3. Configure the remote and populate the credential vault:
   ```bash
   protondrive configure --email alice@proton.me --store-credentials
   ```
4. Inspect the connection:
   ```bash
   protondrive status --details
   ```
5. Sync or mount as needed:
   ```bash
   protondrive sync ~/Docs --remote-path backups
   protondrive mount ~/ProtonDrive
   ```

## CLI usage
```
protondrive [--remote name] <command> [options]
```
Commands summary:

| Command | Purpose |
| --- | --- |
| `configure` | Create/update the Proton Drive rclone remote with credentials and optional vault entry. |
| `status` | Show whether the remote exists; `--details` prints auth history, vault status, mount overview, and top-level folders. |
| `browse` | List remote folders (`lsd`) or files (`--files` runs `ls`). |
| `sync` | Upload/download folders, optionally with watch mode or JSON configs. |
| `mount` | Mount Proton Drive via `rclone mount`, foreground or daemonized. |
| `unmount` | Detach mount points (`--force` tries OS-specific force options). |
| `configs` | List, show, or copy reusable sync config templates. |

### Configure the remote
`protondrive configure` wraps `rclone config create` for the Proton Drive backend. Key options:
- `--email`, `--password`, `--twofa`: supply credentials directly.
- `--password-stdin`, `--twofa-stdin`, `--vault-passphrase-stdin`: read sensitive values from STDIN for automation.
- `--non-interactive`: fail instead of prompting when a value is missing.
- `--skip-verify`: avoid the post-config `lsd` sanity check.
- `--store-credentials`: encrypt the credentials locally for auto-refresh. Pair with `--vault-passphrase` or the `PROTONDRIVE_VAULT_PASSPHRASE` env var.

Example:
```bash
PROTONDRIVE_VAULT_PASSPHRASE="s3cret" \
protondrive configure \
  --email alice@proton.me \
  --password-stdin <<<"$(pass protondrive/password)" \
  --twofa-stdin    <<<"$(pass protondrive/totp)" \
  --store-credentials --non-interactive
```

### Inspect Proton Drive status
- `protondrive status --details` prints
  - last successful auth and method (configure, status, auto-refresh),
  - the vault state (enabled/missing/disabled),
  - recorded mounts with OS verification,
  - and a list of top-level folders (via `rclone lsd`).
- `protondrive browse --remote-path Shares/Photos --files` displays directory contents without syncing.

If authentication fails, the CLI attempts to auto-refresh from the vault and records the outcome in `~/.config/protondrive/<remote>.state`.

### Sync workflows
`protondrive sync [local_path]` orchestrates `rclone sync` for uploads (default) or downloads.

Important options:
- `--direction upload|download`
- `--remote-path dir/in/protondrive`
- `--config <name-or-json>`: load a config from `~/.config/protondrive/sync-configs` or from the embedded templates.
- `--watch` (upload only) + `--watch-debounce 30s`: keep watching for filesystem events and rerun `rclone` after changes settle.
- `--robust` (default) keeps rerunning failed syncs with exponential backoff; tune it with `--max-attempts`, `--backoff-initial`, and `--backoff-max`.
- `--dry-run`, `--no-progress`: pass through to rclone.
- Extra rclone flags go after `--`, e.g. `protondrive sync ~/Docs -- --delete-after`.

Robust mode is enabled automatically so long uploads/downloads can survive transient errors. Repeated attempts are idempotent because `rclone sync` skips already-mirrored files. Use `--robust=false` or `--max-attempts 1` to restore the one-shot behavior if your automation needs fast failures.

Examples:
```bash
# Upload a project folder into Proton Drive/Projects
dirs=Projects/MyApp
protondrive sync ~/Projects/MyApp --remote-path "$dirs"

# Download to an offline mirror
protondrive sync ~/ProtonMirror --remote-path Backups/server --direction download

# Use a preset config and keep it in sync when files change
protondrive sync --config paperless-ngx-export --watch --watch-debounce 45s
```

### Mounting Proton Drive
`protondrive mount ~/ProtonDrive` wraps `rclone mount` and can run either
- foreground (`--foreground`) so you can keep the process attached, or
- background (default) with `--daemon` until rclone reports readiness.

Flags of note:
- `--remote-path`: mount only a subfolder.
- `--cache-mode`, `--vfs-cache-max-age`, `--buffer-size`: pass VFS tuning parameters.
- `--read-only`, `--allow-other`, `--allow-root`: tweak permissions.
- `--ready-timeout`: how long to wait for the daemon to report success.
- `--robust` (default) + `--max-attempts`, `--backoff-initial`, `--backoff-max`: keep retrying `rclone mount` if Proton Drive is temporarily unavailable.
- `--rclone-flag=<flag>`: repeat to forward arbitrary options to `rclone mount`.

Successful mounts are recorded in the state file so `status --details` can show history. Use `protondrive unmount ~/ProtonDrive` to detach (with `--force` for stuck mounts). Platform-specific helpers are invoked automatically (`fusermount`, `umount`, `diskutil`, `mountvol`).

### Managing reusable sync configs
The CLI stores JSON configs under `${XDG_CONFIG_HOME:-~/.config}/protondrive/sync-configs`.

```
protondrive configs list   # built-ins + user configs
protondrive configs init paperless-ngx-export  # copy a template
protondrive configs show paperless-ngx-export  # inspect JSON
protondrive sync --config paperless-ngx-export # run it
```
Each JSON file can declare `name`, `description`, `local_path`, `remote_path`, `direction`, `watch`, `watch_debounce`, `extra_rclone_args`, `robust`, `max_attempts`, `backoff_initial`, and `backoff_max`. Templates are embedded at build time via `internal/customconfigs`.

### Credential vault & automation
Running `protondrive configure --store-credentials`
1. Prompts for a vault passphrase (or reads `PROTONDRIVE_VAULT_PASSPHRASE`).
2. Encrypts `{email, password, twofa}` with AES-256-GCM; the key is derived via scrypt.
3. Saves `<remote>.creds` to `~/.config/protondrive` and records metadata in `<remote>.state`.

Whenever `status`, `browse`, `sync`, or `mount` detects an auth failure, the CLI automatically decrypts the vault, reruns `configure` quietly, and retries the original command. Supply the passphrase non-interactively through the environment for cron/systemd usage:
```bash
export PROTONDRIVE_VAULT_PASSPHRASE="s3cret"
protondrive sync --config nightly-backup --dry-run
```
Delete `<remote>.creds` if you want to disable auto-refresh.

## Configuration & data locations
| Path | Purpose |
| --- | --- |
| `${XDG_CONFIG_HOME:-~/.config}/protondrive` | Base directory for all Protondrive CLI metadata. |
| `*.creds` | Encrypted credential vault per remote. |
| `*.state` | JSON with last auth info, vault state, and mount history. |
| `sync-configs/` | Folder that holds user-defined sync JSON files. |
| `sync-configs/*.json` | Individual configs referenced by `--config`. |

## Environment variables
- `PROTONDRIVE_VAULT_PASSPHRASE`: Provides the vault passphrase so commands can run non-interactively. Leave unset to be prompted each time.
- `XDG_CONFIG_HOME`: Standard override for the config directory if you prefer a different location than `~/.config`.

## Built-in sync templates
| Template | Description | Notes |
| --- | --- | --- |
| `paperless-ngx-export` | Mirrors the `./export` directory from the Paperless-ngx stack to `Backups/Paperless/export`. | Ships with `watch: true` (30s debounce) for near-real-time exports. |
| `photo-drop-upload` | Watches `~/Pictures/ToProton` and uploads new files into `Shares/Photos`. | Convenient "drop to share" workflow. |
| `shared-media-downloader` | Downloads `Shared/Family-Photos` into `~/ProtonShares/FamilyPhotos`. | One-shot mirror suitable for cron jobs. |

Copy any template via `protondrive configs init <name>` and edit the resulting JSON to match your paths.

## Development
- Requires Go 1.21+ and a working rclone install for manual testing.
- Source layout:
  - `cmd/protondrive/main.go`: CLI and command implementations.
  - `internal/customconfigs`: embedded sync templates.
- Typical workflow:
  ```bash
  go fmt ./...
  go vet ./...
  go build ./cmd/protondrive
  ```
- Keep [`CHANGELOG.md`](CHANGELOG.md) updated when adding features.

## Testing & QA
Minimal unit tests cover helpers such as path expansion:
```bash
go test ./...
```
Integration coverage relies on your local rclone installation. For CI or scripted checks, stub out rclone or run against a disposable Proton test account.

## Troubleshooting
- **Blocked behind Proton VPN ("too many connection attempts")**: Proton's anti-abuse layer counts logins per exit IP, so Proton VPN's shared addresses often trigger 422/2028 errors or the Proton Drive warning banner. Split-tunnel Proton domains outside the VPN or forward rclone's `--bind <LAN_IP>` flag through this CLI (for example, `protondrive sync ~/Docs --remote-path backups -- --bind 192.168.1.25`) so Proton Drive connections leave via your regular interface instead of the congested VPN endpoint [speich.net](https://www.speich.net/articles/en/2024/12/14/use-proton-drive-on-linux/). Proton staff also asked rclone users not to cron sync every few minutes because it re-triggers these protections, so space out automations to longer intervals [Reddit](https://www.reddit.com/r/rclone/comments/1d2pm6p/comment/l67lpl2/).
- **`rclone` not found**: ensure `rclone` is on `$PATH`; the CLI checks via `exec.LookPath` before running commands.
- **Mounts never become ready**: rerun with `protondrive mount --foreground` to see rclone logs, or increase `--ready-timeout`. Confirm `fusermount3` is installed.
- **Auto-refresh keeps failing**: verify `PROTONDRIVE_VAULT_PASSPHRASE` matches the passphrase used when storing credentials. Delete `<remote>.creds` and rerun `configure --store-credentials` if unsure.
- **Watch mode stops syncing**: watch is upload-only; downloads cannot use `--watch`. Ensure the local path exists before enabling watches.
- **Permission denied**: mounts inherit directory permissions. Use `--allow-other` (with proper FUSE config) or mount inside your home directory.

## Contributing
Issues and PRs are welcome! Please:
1. Open an issue describing the bug/feature when possible.
2. Include reproduction steps or CLI logs (redacted) for bugs.
3. Run `go fmt`, `go vet`, and `go test ./...` before submitting.
4. Update the README/CHANGELOG when changing user-facing behavior.

## License
GPLv3 – see [`LICENSE`](LICENSE) for the full text.
