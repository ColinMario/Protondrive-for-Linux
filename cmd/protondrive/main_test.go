package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("unable to determine home directory: %v", err)
	}

	tmp := filepath.Join(os.TempDir(), "protondrive-path")

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "noTilde", input: tmp, want: tmp},
		{name: "tildeOnly", input: "~", want: home},
		{name: "tildeSlash", input: "~/", want: home},
		{name: "tildeNested", input: "~/ProtonDrive/sub", want: filepath.Join(home, "ProtonDrive", "sub")},
		{name: "tildeBackslash", input: "~\\ProtonDrive", want: filepath.Join(home, "ProtonDrive")},
		{name: "tildeUsername", input: "~someone", want: "~someone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expandPath(tt.input); got != tt.want {
				t.Fatalf("expandPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseGlobalArgsBackendAndBins(t *testing.T) {
	t.Setenv(backendEnv, "")
	t.Setenv(protonDriveBinEnv, "")
	t.Setenv(rcloneBinEnv, "")

	options, args, err := parseGlobalArgs([]string{
		"--backend", "proton",
		"--proton-drive-bin", "/tmp/proton-drive",
		"--rclone-bin=/tmp/rclone",
		"--remote", "archive",
		"status",
	})
	if err != nil {
		t.Fatalf("parseGlobalArgs returned error: %v", err)
	}
	if options.Backend != backendProton {
		t.Fatalf("Backend = %q, want %q", options.Backend, backendProton)
	}
	if options.ProtonDriveBin != "/tmp/proton-drive" {
		t.Fatalf("ProtonDriveBin = %q", options.ProtonDriveBin)
	}
	if options.RcloneBin != "/tmp/rclone" {
		t.Fatalf("RcloneBin = %q", options.RcloneBin)
	}
	if options.Remote != "archive" {
		t.Fatalf("Remote = %q", options.Remote)
	}
	if len(args) != 1 || args[0] != "status" {
		t.Fatalf("args = %#v, want [status]", args)
	}
}

func TestConfigsRejectsForceOutsideInit(t *testing.T) {
	for _, args := range [][]string{{"--force", "list"}, {"--force=false", "show", "paperless-ngx-export"}, {"--force"}} {
		if err := runConfigs(remoteDefault, args); err == nil || !strings.Contains(err.Error(), "only valid with 'configs init'") {
			t.Fatalf("runConfigs(%q) error = %v, want force-scope error", args, err)
		}
	}
}

func TestBrowseRejectsIgnoredModeFlags(t *testing.T) {
	previous := currentOptions
	t.Cleanup(func() { currentOptions = previous })

	currentOptions = runtimeOptions{Remote: remoteDefault, Backend: backendRclone, RcloneBin: "/usr/bin/true", ProtonDriveBin: "/usr/bin/true"}
	if err := runBrowse(remoteDefault, []string{"--all"}); err == nil || !strings.Contains(err.Error(), "only supported by the Proton backend") {
		t.Fatalf("rclone browse --all error = %v, want backend-scope error", err)
	}

	currentOptions.Backend = backendProton
	if err := runBrowse(remoteDefault, []string{"--files", "--all"}); err == nil || !strings.Contains(err.Error(), "either --files or --all") {
		t.Fatalf("proton browse --files --all error = %v, want conflicting-mode error", err)
	}
}

func TestParseGlobalArgsValidatesRemoteName(t *testing.T) {
	options, args, err := parseGlobalArgs([]string{"--remote", " archive: ", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Remote != "archive" || len(args) != 1 || args[0] != "status" {
		t.Fatalf("normalized parse = %#v, %#v", options, args)
	}
	for _, invalid := range []string{"foo:bar", "evil]\n[other", "bad\x00name"} {
		if _, _, err := parseGlobalArgs([]string{"--remote", invalid, "status"}); err == nil {
			t.Fatalf("invalid remote name %q was accepted", invalid)
		}
	}
}

func TestNormalizeBackend(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty", input: "", want: backendAuto},
		{name: "auto", input: "auto", want: backendAuto},
		{name: "protonAlias", input: "proton-drive", want: backendProton},
		{name: "officialAlias", input: "official", want: backendProton},
		{name: "rclone", input: "rclone", want: backendRclone},
		{name: "invalid", input: "webdav", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeBackend(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeBackend(%q) returned nil error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeBackend(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeBackend(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConfigureRejectsConflictingSecretSourcesAndModes(t *testing.T) {
	for _, args := range [][]string{
		{"--email", "alice@example.com", "--password", "one", "--password-stdin"},
		{"--from-proton-cli-session", "--from-rclone-session"},
		{"--from-proton-cli-session", "--email", "alice@example.com"},
		{"--vault-passphrase", "secret"},
		{"unexpected-positional"},
	} {
		if err := runRcloneConfigure(remoteDefault, args); err == nil {
			t.Fatalf("conflicting configure args were accepted: %#v", args)
		}
	}
}

func TestHTTPClientAllowsSlowReleaseDownloads(t *testing.T) {
	if got := httpClient().Timeout; got < 30*time.Minute {
		t.Fatalf("http client timeout = %s, want at least 30m for slow release assets", got)
	}
	redirect, err := http.NewRequest(http.MethodGet, "http://example.com/insecure", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpClient().CheckRedirect(redirect, nil); err == nil {
		t.Fatal("HTTP downgrade redirect was accepted")
	}
	if _, err := fetchURLBytes("http://example.com/insecure", 10); err == nil {
		t.Fatal("direct HTTP download was accepted")
	}
}

func TestFlatpakHostSpawnArgsForwardsToolEnvironment(t *testing.T) {
	t.Setenv("RCLONE_CONFIG", "/tmp/rclone.conf")
	t.Setenv("PROTON_DRIVE_CACHE_DIR", "/tmp/proton-cache")

	args := flatpakHostSpawnArgs("/usr/bin/rclone", "lsd", "protondrive:")
	text := strings.Join(args, "\n")
	for _, want := range []string{
		"--host",
		"--env=RCLONE_CONFIG=/tmp/rclone.conf",
		"--env=PROTON_DRIVE_CACHE_DIR=/tmp/proton-cache",
		"/usr/bin/rclone",
		"lsd",
		"protondrive:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("flatpakHostSpawnArgs missing %q: %#v", want, args)
		}
	}
}

func TestExternalCommandsDoNotInheritVaultPassphrase(t *testing.T) {
	t.Setenv(vaultPassphraseEnv, "vault-secret-that-must-not-reach-tools")
	cmd := externalCommandWithEnvironment("/usr/bin/true", map[string]string{"PROTONDRIVE_SAFE_TEST": "ok"})
	environment := strings.Join(cmd.Env, "\n")
	if strings.Contains(environment, vaultPassphraseEnv+"=") || strings.Contains(environment, "vault-secret-that-must-not-reach-tools") {
		t.Fatal("vault passphrase leaked into child environment")
	}
	if !strings.Contains(environment, "PROTONDRIVE_SAFE_TEST=ok") {
		t.Fatal("explicit non-secret environment override was lost")
	}
}

func TestNormalizeMountMethod(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty", input: "", want: mountMethodAuto},
		{name: "auto", input: "auto", want: mountMethodAuto},
		{name: "fuse", input: "fuse", want: mountMethodFuse},
		{name: "rcloneAlias", input: "rclone", want: mountMethodFuse},
		{name: "webdav", input: "webdav", want: mountMethodWebDAV},
		{name: "invalid", input: "nfs", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeMountMethod(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeMountMethod(%q) returned nil error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeMountMethod(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeMountMethod(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestProtonCLIPlatform(t *testing.T) {
	tests := []struct {
		goos    string
		goarch  string
		want    string
		wantErr bool
	}{
		{goos: "linux", goarch: "amd64", want: "linux/x64"},
		{goos: "linux", goarch: "arm64", want: "linux/arm64"},
		{goos: "darwin", goarch: "amd64", want: "macos/x64"},
		{goos: "darwin", goarch: "arm64", want: "macos/arm64"},
		{goos: "freebsd", goarch: "amd64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"-"+tt.goarch, func(t *testing.T) {
			got, err := protonCLIPlatform(tt.goos, tt.goarch)
			if tt.wantErr {
				if err == nil {
					t.Fatal("protonCLIPlatform returned nil error")
				}
				return
			}
			if err != nil {
				t.Fatalf("protonCLIPlatform returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("protonCLIPlatform = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseProtonCLIAssets(t *testing.T) {
	html := `<table><tbody>
	<tr>
		<td>linux/x64</td>
		<td><a href="https://proton.me/download/drive/cli/0.5.0/linux-x64/proton-drive">download</a></td>
		<td><code>d187409932742e6fdc6aae2995998f4c89ea51999283395bc8d0bdc5343a79d31bf5a485d5af9adf3b7909fc92f2d2ef0b133edc4939d5faf1d096eb744425bb</code></td>
	</tr>
	</tbody></table>`
	assets := parseProtonCLIAssets(html)
	asset, ok := assets["linux/x64"]
	if !ok {
		t.Fatal("linux/x64 asset was not parsed")
	}
	if asset.URL != "https://proton.me/download/drive/cli/0.5.0/linux-x64/proton-drive" {
		t.Fatalf("URL = %q", asset.URL)
	}
	if len(asset.SHA512) != 128 {
		t.Fatalf("SHA512 length = %d", len(asset.SHA512))
	}
	if err := validateProtonCLIAssetURL(asset.URL); err != nil {
		t.Fatalf("official asset URL rejected: %v", err)
	}
	for _, invalid := range []string{
		"http://proton.me/download/drive/cli/0.5.0/proton-drive",
		"https://example.com/download/drive/cli/0.5.0/proton-drive",
		"https://proton.me.evil.invalid/download/drive/cli/0.5.0/proton-drive",
	} {
		if err := validateProtonCLIAssetURL(invalid); err == nil {
			t.Fatalf("untrusted Proton asset URL %q was accepted", invalid)
		}
	}
}

func TestValidateProtonSessionBridgeVersionOutput(t *testing.T) {
	accepted := []string{
		"Proton Drive CLI cli-drive@0.4.6\nProton Drive SDK js@0.14.10",
		"Proton Drive CLI cli-drive@0.5.0+73e40d90\nProton Drive SDK js@0.19.1+73e40d90",
	}
	for _, output := range accepted {
		if err := validateProtonSessionBridgeVersionOutput(output); err != nil {
			t.Fatalf("expected %q to be accepted: %v", output, err)
		}
	}
	for _, output := range []string{"Proton Drive CLI 0.6.0", "Proton Drive CLI 1.0.0", "unknown"} {
		if err := validateProtonSessionBridgeVersionOutput(output); err == nil {
			t.Fatalf("expected %q to be rejected", output)
		}
	}
}

func TestRclonePlatform(t *testing.T) {
	goos, goarch, err := rclonePlatform("darwin", "arm64")
	if err != nil {
		t.Fatalf("rclonePlatform returned error: %v", err)
	}
	if goos != "osx" || goarch != "arm64" {
		t.Fatalf("rclonePlatform = %s/%s, want osx/arm64", goos, goarch)
	}
}

func TestFetchRcloneChecksumFromText(t *testing.T) {
	body := strings.Join([]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  rclone-v1.74.3-linux-amd64.zip",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  rclone-v1.74.3-linux-arm64.zip",
	}, "\n")
	got, err := rcloneChecksumFromText(body, "rclone-v1.74.3-linux-arm64.zip")
	if err != nil {
		t.Fatalf("rcloneChecksumFromText returned error: %v", err)
	}
	if got != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("checksum = %q", got)
	}
}

func TestChooseMountMethod(t *testing.T) {
	if chooseMountMethod(mountMethodFuse, false) != mountMethodFuse {
		t.Fatal("explicit fuse mount method was not preserved")
	}
	if chooseMountMethod(mountMethodWebDAV, false) != mountMethodWebDAV {
		t.Fatal("explicit webdav mount method was not preserved")
	}
	got := chooseMountMethod(mountMethodAuto, false)
	if runtime.GOOS == "darwin" {
		if got != mountMethodWebDAV {
			t.Fatalf("darwin auto mount method = %q, want %q", got, mountMethodWebDAV)
		}
	} else if got != mountMethodFuse {
		t.Fatalf("non-darwin auto mount method = %q, want %q", got, mountMethodFuse)
	}
	if chooseMountMethod(mountMethodAuto, true) != mountMethodFuse {
		t.Fatal("foreground auto mount method should use fuse")
	}
}

func TestConfigureRemoteUsesOneTimeTwoFAWithoutPersistingIt(t *testing.T) {
	tmp := t.TempDir()
	fakeRclone := filepath.Join(tmp, "rclone")
	logPath := filepath.Join(tmp, "rclone.log")
	configPath := filepath.Join(tmp, "rclone.conf")
	twoFALogPath := filepath.Join(tmp, "twofa.log")
	script := `#!/bin/sh
if [ "$1" = "obscure" ]; then
  read secret
  printf 'obscure args: %s\n' "$*" >> "$RCLONE_FAKE_LOG"
  printf 'obscured-%s\n' "$secret"
  exit 0
fi
if [ "$1" = "config" ] && [ "$2" = "file" ]; then
  printf 'Configuration file is stored at:\n%s\n' "$RCLONE_CONFIG"
  exit 0
fi
if [ "$1" = "lsd" ]; then
  sed -n '/^2fa = /p' "$RCLONE_CONFIG" >> "$RCLONE_2FA_LOG"
  exit 0
fi
printf '%s\n' "$*" >> "$RCLONE_FAKE_LOG"
`
	if err := os.WriteFile(fakeRclone, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake rclone: %v", err)
	}
	t.Setenv("RCLONE_FAKE_LOG", logPath)
	t.Setenv("RCLONE_2FA_LOG", twoFALogPath)
	t.Setenv("RCLONE_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("[protondrive]\ncustom_option = keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	previous := currentOptions
	currentOptions.RcloneBin = fakeRclone
	t.Cleanup(func() {
		currentOptions = previous
	})

	if err := configureRemote("protondrive", "alice@proton.me", "loginpass", "mailpass", true); err != nil {
		t.Fatalf("configureRemote returned error: %v", err)
	}
	if err := verifyRemoteWithOneTimeCode("protondrive", configPath, "123456"); err != nil {
		t.Fatalf("verifyRemoteWithOneTimeCode returned error: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read fake rclone log: %v", err)
	}
	got := string(logData)
	if strings.Contains(got, "loginpass") || strings.Contains(got, "mailpass") || strings.Contains(got, "123456") {
		t.Fatalf("secret leaked through rclone process arguments: %s", got)
	}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read rclone config: %v", err)
	}
	configText := string(configData)
	for _, want := range []string{
		"[protondrive]",
		"type = protondrive",
		"username = alice@proton.me",
		"password = obscured-loginpass",
		"mailbox_password = obscured-mailpass",
		"custom_option = keep-me",
	} {
		if !strings.Contains(configText, want) {
			t.Fatalf("rclone config did not contain %q: %s", want, configText)
		}
	}
	if strings.Contains(configText, "2fa") || strings.Contains(configText, "123456") {
		t.Fatalf("one-time 2FA code persisted in rclone config: %s", configText)
	}
	twoFAData, err := os.ReadFile(twoFALogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(twoFAData)) != "2fa = 123456" {
		t.Fatalf("transient rclone config did not receive one-time code: %q", string(twoFAData))
	}
	matches, err := filepath.Glob(filepath.Join(tmp, ".protondrive-2fa-*.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("transient 2FA files were not removed: %v", matches)
	}
}

func TestRcloneConfigEditorRefusesEncryptedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rclone.conf")
	original := []byte("RCLONE_ENCRYPT_V0:\nopaque-ciphertext\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeRcloneConfigSection(path, "protondrive", map[string]string{"type": "protondrive"})
	if err == nil || !strings.Contains(err.Error(), "encrypted rclone config") {
		t.Fatalf("error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("encrypted config changed: %q", string(got))
	}
}

func TestProtonDrivePath(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		defaultMyFiles bool
		want           string
	}{
		{name: "defaultMyFiles", input: "", defaultMyFiles: true, want: "/my-files"},
		{name: "defaultRoot", input: "", defaultMyFiles: false, want: "/"},
		{name: "relative", input: "Documents/Reports", defaultMyFiles: true, want: "/my-files/Documents/Reports"},
		{name: "alreadyMyFiles", input: "/my-files/Documents", defaultMyFiles: true, want: "/my-files/Documents"},
		{name: "sharedWithMe", input: "/shared-with-me/abc", defaultMyFiles: true, want: "/shared-with-me/abc"},
		{name: "rootPassthrough", input: "/", defaultMyFiles: true, want: "/"},
		{name: "rootRelative", input: "devices/laptop", defaultMyFiles: false, want: "/devices/laptop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := protonDrivePath(tt.input, tt.defaultMyFiles); got != tt.want {
				t.Fatalf("protonDrivePath(%q, %v) = %q, want %q", tt.input, tt.defaultMyFiles, got, tt.want)
			}
		})
	}
}

func TestBackendHintDetection(t *testing.T) {
	if !configureArgsRequireRclone([]string{"--email", "alice@proton.me"}) {
		t.Fatal("configureArgsRequireRclone did not detect --email")
	}
	if !configureArgsRequireRclone([]string{"--store-credentials"}) {
		t.Fatal("configureArgsRequireRclone did not detect --store-credentials")
	}
	if !configureArgsRequireRclone([]string{"--mailbox-password", "secret"}) {
		t.Fatal("configureArgsRequireRclone did not detect --mailbox-password")
	}
	if !configureArgsRequireRclone([]string{"--mailbox-password-stdin"}) {
		t.Fatal("configureArgsRequireRclone did not detect --mailbox-password-stdin")
	}
	if !configureArgsRequireRclone([]string{"--from-proton-cli-session"}) {
		t.Fatal("configureArgsRequireRclone did not detect --from-proton-cli-session")
	}
	if !configureArgsRequireRclone([]string{"--from-rclone-session"}) {
		t.Fatal("configureArgsRequireRclone did not detect --from-rclone-session")
	}
	if !configureArgsRequireRclone([]string{"--headless"}) {
		t.Fatal("configureArgsRequireRclone did not detect --headless")
	}
	if configureArgsRequireRclone([]string{"--skip-verify"}) {
		t.Fatal("configureArgsRequireRclone treated --skip-verify as rclone-only")
	}
	if !syncArgsRequireRclone([]string{"--dry-run"}) {
		t.Fatal("syncArgsRequireRclone did not detect --dry-run")
	}
	if !syncArgsRequireRclone([]string{"--"}) {
		t.Fatal("syncArgsRequireRclone did not detect passthrough marker")
	}
}

func TestAutoSyncDoesNotSilentlyFallbackToRclone(t *testing.T) {
	previous := currentOptions
	currentOptions = runtimeOptions{
		Remote:         remoteDefault,
		Backend:        backendAuto,
		ProtonDriveBin: filepath.Join(t.TempDir(), "missing-proton-drive"),
		RcloneBin:      filepath.Join(t.TempDir(), "missing-rclone"),
	}
	t.Cleanup(func() {
		currentOptions = previous
	})

	_, err := resolveSyncBackend(nil, nil, false, false, nil)
	if err == nil {
		t.Fatal("resolveSyncBackend returned nil error")
	}
	if !strings.Contains(err.Error(), "does not fall back to rclone") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProtonConfigureHeadlessExplainsRclone(t *testing.T) {
	err := runProtonConfigure("protondrive", []string{"--headless"})
	if err == nil {
		t.Fatal("runProtonConfigure --headless returned nil error")
	}
	message := err.Error()
	if !strings.Contains(message, "uses rclone") || !strings.Contains(message, "--backend rclone") {
		t.Fatalf("unexpected headless error: %s", message)
	}
}

func TestNormalizeInterspersedFlags(t *testing.T) {
	got := normalizeInterspersedFlags([]string{
		"~/Documents",
		"--remote-path", "/my-files/backups",
		"--conflict-strategy=merge",
		"--watch",
		"--",
		"--delete-after",
	}, map[string]bool{
		"remote-path":       true,
		"conflict-strategy": true,
		"watch":             false,
	})
	want := []string{
		"--remote-path", "/my-files/backups",
		"--conflict-strategy=merge",
		"--watch",
		"~/Documents",
		"--delete-after",
	}
	if len(got) != len(want) {
		t.Fatalf("len(normalizeInterspersedFlags) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeInterspersedFlags[%d] = %q, want %q; full result %#v", i, got[i], want[i], got)
		}
	}
}

func TestNormalizeInterspersedMountFlags(t *testing.T) {
	got := normalizeInterspersedFlags([]string{
		"~/ProtonDrive",
		"--remote-path", "Backups",
		"--read-only",
		"--ready-timeout=15s",
	}, map[string]bool{
		"remote-path":   true,
		"read-only":     false,
		"ready-timeout": true,
	})
	want := []string{
		"--remote-path", "Backups",
		"--read-only",
		"--ready-timeout=15s",
		"~/ProtonDrive",
	}
	if len(got) != len(want) {
		t.Fatalf("len(normalizeInterspersedFlags) = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeInterspersedFlags[%d] = %q, want %q; full result %#v", i, got[i], want[i], got)
		}
	}
}

func TestMountErrorWithPlatformHints(t *testing.T) {
	err := mountErrorWithHints("protondrive:", "/tmp/mount", 10*time.Second, errors.New("mount_macfuse: the file system is not available (2)"), false)
	message := err.Error()
	if runtime.GOOS == "darwin" {
		if !strings.Contains(message, "macFUSE") {
			t.Fatalf("darwin mount error did not mention macFUSE: %s", message)
		}
		return
	}
	if !strings.Contains(message, "fusermount") {
		t.Fatalf("non-darwin mount error did not keep generic FUSE hint: %s", message)
	}
}

func TestDarwinForceUnmountCommand(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-specific unmount syntax")
	}
	commands := unmountCommands("/tmp/example", true)
	if len(commands) == 0 {
		t.Fatal("no unmount commands returned")
	}
	want := []string{"diskutil", "unmount", "force", "/tmp/example"}
	if len(commands[0]) != len(want) {
		t.Fatalf("first unmount command = %#v, want %#v", commands[0], want)
	}
	for i := range want {
		if commands[0][i] != want[i] {
			t.Fatalf("first unmount command = %#v, want %#v", commands[0], want)
		}
	}
}

func TestCanonicalMountPathResolvesExistingSymlinkPrefix(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalMountPath(link); got != want {
		t.Fatalf("canonicalMountPath(%q) = %q, want %q", link, got, want)
	}
}

func TestPersistentMountServiceName(t *testing.T) {
	got := persistentMountServiceName("protondrive", "/home/alice/Proton Drive", "")
	want := "protondrive-mount-protondrive-proton-drive.service"
	if got != want {
		t.Fatalf("persistentMountServiceName = %q, want %q", got, want)
	}

	got = persistentMountServiceName("protondrive", "/ignored", "Work Backup")
	want = "protondrive-mount-work-backup.service"
	if got != want {
		t.Fatalf("persistentMountServiceName with override = %q, want %q", got, want)
	}

	got = persistentMountOpenRCServiceName("protondrive", "/home/alice/Proton Drive", "")
	want = "protondrive-mount-protondrive-proton-drive"
	if got != want {
		t.Fatalf("persistentMountOpenRCServiceName = %q, want %q", got, want)
	}
}

func TestSystemdQuoteEscapesSpecifiersAndExpansions(t *testing.T) {
	input := "/home/alice/100%/$drive/with\\slash/and\"quote\nnext"
	want := `"/home/alice/100%%/$$drive/with\\slash/and\"quote\nnext"`
	if got := systemdQuote(input); got != want {
		t.Fatalf("systemdQuote(%q) = %q, want %q", input, got, want)
	}
}

func TestNormalizePersistentMountManager(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"", persistentMountManagerAuto},
		{"AUTO", persistentMountManagerAuto},
		{" systemd ", persistentMountManagerSystemd},
		{"openrc", persistentMountManagerOpenRC},
	} {
		got, err := normalizePersistentMountManager(tc.input)
		if err != nil {
			t.Fatalf("normalizePersistentMountManager(%q) returned error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("normalizePersistentMountManager(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	if _, err := normalizePersistentMountManager("launchd"); err == nil {
		t.Fatal("normalizePersistentMountManager accepted unsupported manager")
	}
}

func TestPersistentMountStartArgs(t *testing.T) {
	args := persistentMountStartArgs("/usr/local/bin/protondrive", "/usr/bin/rclone", persistentMountOptions{
		Remote:       "archive",
		MountPoint:   "/home/alice/ProtonDrive",
		RemotePath:   "Backups",
		CacheMode:    "full",
		CacheMaxAge:  "2h",
		BufferSize:   "32M",
		ReadOnly:     true,
		AllowOther:   true,
		ReadyTimeout: 20 * time.Second,
		RcloneFlags:  []string{"--dir-cache-time=10m"},
	})
	wantContains := []string{
		"/usr/local/bin/protondrive",
		"--backend", backendRclone,
		"--remote", "archive",
		"--rclone-bin", "/usr/bin/rclone",
		"mount", "/home/alice/ProtonDrive",
		"--foreground",
		"--mount-method", mountMethodFuse,
		"--remote-path", "Backups",
		"--vfs-cache-max-age", "2h",
		"--buffer-size", "32M",
		"--read-only",
		"--allow-other",
		"--rclone-flag", "--dir-cache-time=10m",
	}
	for _, want := range wantContains {
		found := false
		for _, got := range args {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("persistent args missing %q: %#v", want, args)
		}
	}
}

func TestPersistentMountRecordsStateAndRemovePersistIsIdempotent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("persistent mount services are linux-specific")
	}

	tmp := t.TempDir()
	fakeRclone := filepath.Join(tmp, "rclone")
	fakeSystemctl := filepath.Join(tmp, "systemctl")
	rcloneScript := `#!/bin/sh
if [ "$1" = "lsd" ]; then
  exit 0
fi
exit 0
`
	systemctlScript := `#!/bin/sh
case "$2" in
  enable) touch "$SYSTEMCTL_FAKE_STATE" ;;
  disable) rm -f "$SYSTEMCTL_FAKE_STATE" ;;
  is-active|is-enabled) test -f "$SYSTEMCTL_FAKE_STATE"; exit $? ;;
esac
exit 0
`
	if err := os.WriteFile(fakeRclone, []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("failed to write fake rclone: %v", err)
	}
	if err := os.WriteFile(fakeSystemctl, []byte(systemctlScript), 0o755); err != nil {
		t.Fatalf("failed to write fake systemctl: %v", err)
	}

	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("SYSTEMCTL_FAKE_STATE", filepath.Join(tmp, "systemctl-active"))
	previousMountCheck := mountedPathCheck
	mountedPathCheck = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() { mountedPathCheck = previousMountCheck })

	previous := currentOptions
	currentOptions = runtimeOptions{
		Remote:         remoteDefault,
		Backend:        backendRclone,
		ProtonDriveBin: filepath.Join(tmp, "proton-drive"),
		RcloneBin:      fakeRclone,
	}
	t.Cleanup(func() {
		currentOptions = previous
	})

	mountPoint := filepath.Join(tmp, "mnt")
	if err := runMount(remoteDefault, []string{
		mountPoint,
		"--persist",
		"--persist-manager", persistentMountManagerSystemd,
		"--persist-name", "audit-main",
		"--remote-path", "Backups",
	}); err != nil {
		t.Fatalf("runMount --persist returned error: %v", err)
	}

	state, err := loadRemoteState(remoteDefault)
	if err != nil {
		t.Fatalf("failed to load remote state: %v", err)
	}
	if len(state.Mounts) != 1 || !state.Mounts[0].Attached {
		t.Fatalf("persistent mount was not recorded as attached: %#v", state.Mounts)
	}
	if state.Mounts[0].RemotePath != "protondrive:Backups" {
		t.Fatalf("remote path = %q", state.Mounts[0].RemotePath)
	}

	if err := runUnmount(remoteDefault, []string{
		mountPoint,
		"--remove-persist",
		"--persist-manager", persistentMountManagerSystemd,
		"--persist-name", "audit-main",
		"--force",
	}); err != nil {
		t.Fatalf("runUnmount --remove-persist returned error for unmounted path: %v", err)
	}

	state, err = loadRemoteState(remoteDefault)
	if err != nil {
		t.Fatalf("failed to reload remote state: %v", err)
	}
	if len(state.Mounts) != 1 || state.Mounts[0].Attached {
		t.Fatalf("persistent mount was not detached after remove-persist: %#v", state.Mounts)
	}
}

func TestSystemdPersistentMountRollbackRestoresExistingService(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("SYSTEMCTL_ACTIVE", filepath.Join(tmp, "active"))
	t.Setenv("SYSTEMCTL_ENABLED", filepath.Join(tmp, "enabled"))
	t.Setenv("SYSTEMCTL_FAIL_RESTART", "1")
	fakeSystemctl := filepath.Join(tmp, "systemctl")
	script := `#!/bin/sh
case "$2" in
  is-active) test -f "$SYSTEMCTL_ACTIVE"; exit $? ;;
  is-enabled) test -f "$SYSTEMCTL_ENABLED"; exit $? ;;
  enable) touch "$SYSTEMCTL_ENABLED" ;;
  disable) rm -f "$SYSTEMCTL_ACTIVE" "$SYSTEMCTL_ENABLED" ;;
  restart)
    if [ "$SYSTEMCTL_FAIL_RESTART" = "1" ]; then
      printf 'injected restart failure\n' >&2
      exit 1
    fi
    touch "$SYSTEMCTL_ACTIVE"
    ;;
  start) touch "$SYSTEMCTL_ACTIVE" ;;
esac
exit 0
`
	if err := os.WriteFile(fakeSystemctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(os.Getenv("SYSTEMCTL_ACTIVE"), []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("SYSTEMCTL_ENABLED"), []byte("enabled"), 0o600); err != nil {
		t.Fatal(err)
	}

	options := persistentMountOptions{
		Remote:       remoteDefault,
		MountPoint:   filepath.Join(tmp, "mount"),
		CacheMode:    "full",
		ReadyTimeout: time.Second,
		PersistName:  "rollback-test",
		RcloneBin:    "/usr/bin/true",
	}
	serviceName := persistentMountServiceName(options.Remote, options.MountPoint, options.PersistName)
	unitDir, scriptDir, err := systemdPersistentMountDirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scriptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	baseName := strings.TrimSuffix(serviceName, ".service")
	want := map[string]string{
		filepath.Join(unitDir, serviceName):           "previous unit\n",
		filepath.Join(scriptDir, baseName+".sh"):      "previous start\n",
		filepath.Join(scriptDir, baseName+"-stop.sh"): "previous stop\n",
	}
	for path, contents := range want {
		if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := installSystemdPersistentMount(options); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("installSystemdPersistentMount error = %v, want injected restart failure", err)
	}
	for path, contents := range want {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read restored %s: %v", path, err)
		}
		if string(data) != contents {
			t.Fatalf("restored %s = %q, want %q", path, data, contents)
		}
	}
	for _, statePath := range []string{os.Getenv("SYSTEMCTL_ACTIVE"), os.Getenv("SYSTEMCTL_ENABLED")} {
		if _, err := os.Stat(statePath); err != nil {
			t.Fatalf("previous systemd state was not restored at %s: %v", statePath, err)
		}
	}
}

func TestOpenRCMountService(t *testing.T) {
	service := openRCMountService("/sbin/openrc-run", "protondrive-mount-main-drive", "/home/alice/.local/share/protondrive/openrc/start.sh", "/home/alice/.local/share/protondrive/openrc/stop.sh")
	for _, want := range []string{
		"#!/sbin/openrc-run",
		"supervisor=supervise-daemon",
		"respawn_delay=10",
		"respawn_max=3",
		"respawn_period=30",
		"command='/home/alice/.local/share/protondrive/openrc/start.sh'",
		"'/home/alice/.local/share/protondrive/openrc/stop.sh' || true",
		"XDG_RUNTIME_DIR is required for OpenRC user services",
	} {
		if !strings.Contains(service, want) {
			t.Fatalf("OpenRC service missing %q:\n%s", want, service)
		}
	}
}

func TestWriteRcloneConfigSectionReplacesTarget(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	initial := `
[keep]
type = local

[protondrive]
type = protondrive
username = old@example.com
client_access_token = old-token

[other]
type = alias
remote = keep:
`
	if err := os.WriteFile(configPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("failed to seed config: %v", err)
	}

	values := map[string]string{
		"type":                   "protondrive",
		"username":               "proton-cli-session",
		"password":               "obscured-placeholder",
		"mailbox_password":       "obscured-mailbox",
		"2fa":                    "123456",
		"client_uid":             "uid-123",
		"client_access_token":    "access-token",
		"client_refresh_token":   "refresh-token",
		"client_salted_key_pass": "base64-user-key-password",
		"enable_caching":         "false",
	}
	if err := writeRcloneConfigSection(configPath, "protondrive", values); err != nil {
		t.Fatalf("writeRcloneConfigSection returned error: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"[keep]\ntype = local",
		"[other]\ntype = alias\nremote = keep:",
		"[protondrive]",
		"type = protondrive",
		"username = proton-cli-session",
		"password = obscured-placeholder",
		"mailbox_password = obscured-mailbox",
		"2fa = 123456",
		"client_uid = uid-123",
		"client_access_token = access-token",
		"client_refresh_token = refresh-token",
		"client_salted_key_pass = base64-user-key-password",
		"enable_caching = false",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"old@example.com", "old-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("config kept replaced value %q:\n%s", forbidden, text)
		}
	}
	backup, err := os.ReadFile(configPath + ".protondrive.bak")
	if err != nil {
		t.Fatalf("failed to read config backup: %v", err)
	}
	if !strings.Contains(string(backup), "client_access_token = old-token") {
		t.Fatalf("backup did not preserve prior config: %s", backup)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("failed to stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %v, want 0600", got)
	}
}

func TestReadRcloneConfigSection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	initial := `
[keep]
type = local

[protondrive]
type = protondrive
client_uid = uid-123
client_access_token = access-token
client_refresh_token = refresh-token
client_salted_key_pass = salted
`
	if err := os.WriteFile(configPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("failed to seed config: %v", err)
	}
	values, err := readRcloneConfigSection(configPath, "protondrive")
	if err != nil {
		t.Fatalf("readRcloneConfigSection returned error: %v", err)
	}
	if values["client_uid"] != "uid-123" || values["client_access_token"] != "access-token" {
		t.Fatalf("unexpected section values: %#v", values)
	}
}

func TestRcloneConfigRollbackRestoresVerifiedPreviousFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "rclone.conf")
	original := []byte("[protondrive]\ntype = protondrive\nclient_access_token = working\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureRcloneConfigSnapshot(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRcloneConfigSection(configPath, "protondrive", map[string]string{"type": "protondrive", "client_access_token": "broken"}); err != nil {
		t.Fatal(err)
	}
	err = rollbackRcloneConfigError(configPath, snapshot, errors.New("verification failed"))
	if err == nil || !strings.Contains(err.Error(), "previous rclone config restored") {
		t.Fatalf("rollback error = %v", err)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(original) {
		t.Fatalf("restored config = %q, want %q", got, original)
	}
	if _, statErr := os.Stat(configPath + ".protondrive.bak"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("transient backup still exists after rollback: %v", statErr)
	}
}

func TestProtonCLISessionFromRcloneConfigValues(t *testing.T) {
	userKeyPassword := "user-key-password"
	values := map[string]string{
		"client_uid":             "uid-123",
		"client_access_token":    "access-token",
		"client_refresh_token":   "refresh-token",
		"client_salted_key_pass": base64.StdEncoding.EncodeToString([]byte(userKeyPassword)),
	}
	snapshot, err := protonCLISessionFromRcloneConfigValues(values)
	if err != nil {
		t.Fatalf("protonCLISessionFromRcloneConfigValues returned error: %v", err)
	}
	if snapshot.UserKeyPassword != userKeyPassword {
		t.Fatalf("UserKeyPassword = %q, want %q", snapshot.UserKeyPassword, userKeyPassword)
	}
	if snapshot.Session.UID != "uid-123" || snapshot.Session.AccessToken != "access-token" || snapshot.Session.RefreshToken != "refresh-token" {
		t.Fatalf("unexpected session snapshot: %#v", snapshot.Session)
	}
	if strings.TrimSpace(snapshot.CachePassword) == "" {
		t.Fatal("CachePassword was empty")
	}
}

func TestDecodeRcloneSaltedKeyPassKeepsRawFallback(t *testing.T) {
	raw := "FpQm185uM2GWkxDhIyhf8OOT8wYHq/6"
	if got := decodeRcloneSaltedKeyPass(raw); got != raw {
		t.Fatalf("decodeRcloneSaltedKeyPass raw fallback = %q, want %q", got, raw)
	}
}

func TestWriteProtonCLISessionSnapshotUnsafeFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROTON_DRIVE_UNSAFE_SECRETS", "true")
	t.Setenv("PROTON_DRIVE_CACHE_DIR", dir)

	var snapshot protonCLISessionSnapshot
	snapshot.CachePassword = "cache-password"
	snapshot.UserKeyPassword = "user-key-password"
	snapshot.Session.UID = "uid-123"
	snapshot.Session.AccessToken = "access-token"
	snapshot.Session.RefreshToken = "refresh-token"

	target, err := writeProtonCLISessionSnapshot(snapshot)
	if err != nil {
		t.Fatalf("writeProtonCLISessionSnapshot returned error: %v", err)
	}
	want := filepath.Join(dir, "auth-session.json")
	if target != want {
		t.Fatalf("target = %q, want %q", target, want)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("failed to stat session file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("session file mode = %v, want 0600", got)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("failed to read session file: %v", err)
	}
	if !strings.Contains(string(data), `"userKeyPassword":"user-key-password"`) {
		t.Fatalf("session file did not contain expected JSON: %s", string(data))
	}
}

func TestConfigureProtonCLISessionRollbackRestoresUnsafeFile(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	configPath := filepath.Join(dir, "rclone.conf")
	fakeProton := filepath.Join(dir, "proton-drive")
	t.Setenv("PROTON_DRIVE_UNSAFE_SECRETS", "true")
	t.Setenv("PROTON_DRIVE_CACHE_DIR", cacheDir)
	t.Setenv("RCLONE_CONFIG", configPath)

	config := "[protondrive]\n" +
		"type = protondrive\n" +
		"client_uid = uid-123\n" +
		"client_access_token = access-token\n" +
		"client_refresh_token = refresh-token\n" +
		"client_salted_key_pass = " + base64.StdEncoding.EncodeToString([]byte("user-key-password")) + "\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then\n  echo 'Proton Drive CLI cli-drive@0.5.0'\n  exit 0\nfi\necho 'forced verification failure' >&2\nexit 1\n"
	if err := os.WriteFile(fakeProton, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	previousPayload := []byte("{\"previous\":true}\n")
	sessionPath := filepath.Join(cacheDir, "auth-session.json")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, previousPayload, 0o640); err != nil {
		t.Fatal(err)
	}

	previousOptions := currentOptions
	currentOptions = runtimeOptions{Remote: remoteDefault, Backend: backendRclone, ProtonDriveBin: fakeProton, RcloneBin: "rclone"}
	t.Cleanup(func() { currentOptions = previousOptions })

	err := configureProtonCLISessionFromRcloneRemote(remoteDefault, true)
	if err == nil || !strings.Contains(err.Error(), "previous Proton CLI session state restored") {
		t.Fatalf("configureProtonCLISessionFromRcloneRemote error = %v, want restored-state failure", err)
	}
	got, readErr := os.ReadFile(sessionPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(previousPayload) {
		t.Fatalf("restored payload = %q, want %q", got, previousPayload)
	}
	info, statErr := os.Stat(sessionPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o640 {
		t.Fatalf("restored mode = %v, want 0640", gotMode)
	}
}

func TestConfigureProtonCLISessionRollbackRemovesNewUnsafeFile(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	configPath := filepath.Join(dir, "rclone.conf")
	fakeProton := filepath.Join(dir, "proton-drive")
	t.Setenv("PROTON_DRIVE_UNSAFE_SECRETS", "true")
	t.Setenv("PROTON_DRIVE_CACHE_DIR", cacheDir)
	t.Setenv("RCLONE_CONFIG", configPath)

	config := "[protondrive]\n" +
		"client_uid = uid-123\n" +
		"client_access_token = access-token\n" +
		"client_refresh_token = refresh-token\n" +
		"client_salted_key_pass = " + base64.StdEncoding.EncodeToString([]byte("user-key-password")) + "\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then\n  echo 'Proton Drive CLI cli-drive@0.5.0'\n  exit 0\nfi\nexit 1\n"
	if err := os.WriteFile(fakeProton, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	previousOptions := currentOptions
	currentOptions = runtimeOptions{Remote: remoteDefault, Backend: backendRclone, ProtonDriveBin: fakeProton, RcloneBin: "rclone"}
	t.Cleanup(func() { currentOptions = previousOptions })

	err := configureProtonCLISessionFromRcloneRemote(remoteDefault, true)
	if err == nil || !strings.Contains(err.Error(), "previous Proton CLI session state restored") {
		t.Fatalf("configureProtonCLISessionFromRcloneRemote error = %v, want restored-state failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "auth-session.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("new session file still exists after rollback: %v", statErr)
	}
}

func TestSanitizeConfigValue(t *testing.T) {
	got := sanitizeConfigValue("  token\r\nwith\nbreaks  ")
	want := "tokenwithbreaks"
	if got != want {
		t.Fatalf("sanitizeConfigValue = %q, want %q", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	got := shellQuote("a'b c")
	want := `'a'"'"'b c'`
	if got != want {
		t.Fatalf("shellQuote = %q, want %q", got, want)
	}
}

func TestMirrorSafetyDefaultsAndSourceValidation(t *testing.T) {
	source := t.TempDir()
	if err := validateMirrorSource(source, true, "", false); err == nil || !strings.Contains(err.Error(), "empty source") {
		t.Fatalf("empty source error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, ".protondrive-source"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMirrorSource(source, true, ".protondrive-source", false); err == nil || !strings.Contains(err.Error(), "empty source") {
		t.Fatalf("sentinel-only source error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "document.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateMirrorSource(source, true, ".protondrive-source", false); err != nil {
		t.Fatalf("valid guarded source with content: %v", err)
	}
	if err := validateMirrorSource(source, true, "missing", false); err == nil {
		t.Fatal("missing sentinel was accepted")
	}
	if err := validateMirrorRoots("upload", source, ""); err == nil {
		t.Fatal("remote-root mirror was accepted")
	}
	backup := defaultMirrorBackupDir("protondrive", "Backups/Documents", "upload", source, time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC))
	if backup != "protondrive:.protondrive-backups/Backups/Documents/20260714T123000Z" {
		t.Fatalf("backup dir = %q", backup)
	}
}

func TestMirrorSourceRejectsSymlinkSentinelAndRootAlias(t *testing.T) {
	source := t.TempDir()
	realSentinel := filepath.Join(source, "real-sentinel")
	if err := os.WriteFile(realSentinel, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realSentinel, filepath.Join(source, ".protondrive-source")); err != nil {
		t.Fatal(err)
	}
	if err := validateMirrorSource(source, true, ".protondrive-source", true); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink sentinel error = %v", err)
	}
	rootAlias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(string(os.PathSeparator), rootAlias); err != nil {
		t.Fatal(err)
	}
	if !dangerousLocalRoot(rootAlias) {
		t.Fatal("symlink alias to filesystem root bypassed mirror root guard")
	}
}

func TestMirrorSourceRejectsEscapingRemoteSentinel(t *testing.T) {
	if err := validateMirrorSource("protondrive:source", false, "../outside", true); err == nil {
		t.Fatal("expected escaping remote sentinel to be rejected")
	}
}

func TestRunWithRetryRecovers(t *testing.T) {
	attempts := 0
	err := runWithRetry(func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary")
		}
		return nil
	}, 5, 0)
	if err != nil || attempts != 3 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestAuthClassificationSeparatesTransportFailures(t *testing.T) {
	for _, message := range []string{
		"context deadline exceeded",
		"TLS handshake timeout",
		"connection reset by peer",
		"503 Service Unavailable",
	} {
		if isAuthError(errors.New(message)) {
			t.Errorf("transport failure classified as auth: %q", message)
		}
	}
	for _, message := range []string{
		"401 Unauthorized",
		"invalid_grant",
		"refresh token expired",
	} {
		if !isAuthError(errors.New(message)) {
			t.Errorf("auth failure not classified: %q", message)
		}
	}
}

func TestRemoteStorageKeyAvoidsSanitizationCollisions(t *testing.T) {
	a := remoteStorageKey("team/a")
	b := remoteStorageKey("team_a")
	if a == b {
		t.Fatalf("storage keys collided: %q", a)
	}
	if !strings.HasPrefix(a, "team_a-") || !strings.HasPrefix(b, "team_a-") {
		t.Fatalf("unexpected keys: %q %q", a, b)
	}
	if suffix := strings.TrimPrefix(a, "team_a-"); len(suffix) != 16 {
		t.Fatalf("storage hash suffix = %q, want 64-bit hex", suffix)
	}
}

func TestLegacyCredentialAndStateFilesAreMigratedAndRemoved(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	dir, err := ensureCredentialDir()
	if err != nil {
		t.Fatal(err)
	}
	remote := "team/a"
	legacyCreds := filepath.Join(dir, sanitizedRemoteName(remote)+".creds")
	creds := storedCredentials{Email: "alice@example.com", Password: "secret", SavedAt: time.Now()}
	payload, err := encryptCredentials("vault-pass", creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyCreds, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadEncryptedCredentials(remote, "vault-pass")
	if err != nil || loaded.Email != creds.Email {
		t.Fatalf("load legacy credentials = %#v, %v", loaded, err)
	}
	newCreds, err := saveEncryptedCredentials(remote, loaded, "vault-pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newCreds); err != nil {
		t.Fatalf("new credential vault missing: %v", err)
	}
	if _, err := os.Stat(legacyCreds); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy credential vault still exists: %v", err)
	}

	legacyState := filepath.Join(dir, sanitizedRemoteName(remote)+".state")
	if err := os.WriteFile(legacyState, []byte(`{"remote":"team/a","last_auth_method":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updateRemoteState(remote, func(state *remoteState) { state.LastAuthMethod = "migrated" }); err != nil {
		t.Fatal(err)
	}
	newState, err := remoteStateFilePath(remote)
	if err != nil {
		t.Fatal(err)
	}
	if newState == legacyState {
		t.Fatal("state path did not migrate to hashed filename")
	}
	if _, err := os.Stat(legacyState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state still exists: %v", err)
	}
}

func TestRedactCommandArgs(t *testing.T) {
	args := redactCommandArgs([]string{"config", "--password", "secret", "--access-token=token", "--other", "safe"})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, " secret") || strings.Contains(joined, "=token") {
		t.Fatalf("secrets were not redacted: %s", joined)
	}
	if !strings.Contains(joined, "--other safe") {
		t.Fatalf("safe arguments changed: %s", joined)
	}
}

func TestWaitForWebDAVUsesBasicAuth(t *testing.T) {
	authenticated := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "protondrive" || pass != "random-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	serverDone := make(chan error)
	if err := waitForWebDAV(server.URL, "protondrive", "random-secret", time.Second, serverDone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-authenticated:
	default:
		t.Fatal("readiness probe did not send the expected credentials")
	}
}

func TestWriteWebDAVAuthFileUsesPrivateBcryptEntry(t *testing.T) {
	path, err := writeWebDAVAuthFile(t.TempDir(), "protondrive", "runtime-secret")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	username, hash, ok := strings.Cut(line, ":")
	if !ok || username != "protondrive" {
		t.Fatalf("unexpected htpasswd entry: %q", line)
	}
	if strings.Contains(line, "runtime-secret") {
		t.Fatal("WebDAV password was written in plaintext")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("runtime-secret")); err != nil {
		t.Fatalf("htpasswd entry did not contain the expected bcrypt hash: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRecordedMountCleanupOnlyRemovesGeneratedAuthFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	credentialDir, err := ensureCredentialDir()
	if err != nil {
		t.Fatal(err)
	}
	logsDir := filepath.Join(credentialDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(logsDir, "webdav-auth-1234.htpasswd")
	if err := os.WriteFile(authPath, []byte("hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeRecordedMountAuthFile(mountState{AuthFile: authPath}); err != nil {
		t.Fatalf("generated auth file cleanup failed: %v", err)
	}
	if _, err := os.Stat(authPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated auth file still exists: %v", err)
	}

	logPath := filepath.Join(logsDir, "webdav-protondrive.log")
	if err := os.WriteFile(logPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{logPath, filepath.Join(logsDir, ".."), filepath.Join(logsDir, "subdir", "webdav-auth-1234.htpasswd")} {
		if err := removeRecordedMountAuthFile(mountState{AuthFile: unsafe}); err == nil {
			t.Fatalf("unsafe recorded auth path %q was accepted", unsafe)
		}
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("unrelated log file was removed: %v", err)
	}
}

func TestWebDAVMountPasswordIsOnlySentOnStdin(t *testing.T) {
	const password = "runtime-secret-never-in-argv-or-env"
	cmd := webDAVMountCommand("http://127.0.0.1:54321/", "/tmp/Proton Drive", "protondrive", password, 10*time.Second)
	if got := strings.Join(cmd.Args, "\n"); strings.Contains(got, password) {
		t.Fatalf("WebDAV password leaked through process arguments: %s", got)
	}
	if got := strings.Join(cmd.Env, "\n"); strings.Contains(got, password) {
		t.Fatal("WebDAV password leaked through process environment")
	}
	stdin, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdin), password) {
		t.Fatal("WebDAV password was not staged on stdin")
	}
}

func TestRejectWebDAVSecurityOverrides(t *testing.T) {
	for _, args := range [][]string{{"--user", "other"}, {"--pass=secret"}, {"--htpasswd", "/tmp/file"}, {"--addr", ":8080"}} {
		if err := rejectWebDAVSecurityOverrides(args); err == nil {
			t.Fatalf("expected %v to be rejected", args)
		}
	}
	if err := rejectWebDAVSecurityOverrides([]string{"--transfers", "8", "--read-only"}); err != nil {
		t.Fatalf("safe WebDAV options were rejected: %v", err)
	}
}

func TestRejectMountControlOverrides(t *testing.T) {
	for _, args := range [][]string{
		{"--daemon"}, {"--config=/tmp/other.conf"}, {"--read-only=false"},
		{"--vfs-cache-mode=off"}, {"--rc"}, {"--rc-no-auth=true"},
	} {
		if err := rejectMountControlOverrides(args); err == nil {
			t.Fatalf("expected %v to be rejected", args)
		}
	}
	if err := rejectMountControlOverrides([]string{"--dir-cache-time=10m", "--vfs-read-ahead=64M"}); err != nil {
		t.Fatalf("safe mount tuning flags were rejected: %v", err)
	}
}

func TestRcloneWebDAVHtpasswdIntegration(t *testing.T) {
	rcloneBin := strings.TrimSpace(os.Getenv("PROTONDRIVE_RCLONE_INTEGRATION"))
	if rcloneBin == "" {
		t.Skip("set PROTONDRIVE_RCLONE_INTEGRATION to run the real rclone WebDAV test")
	}
	addr, err := reserveLocalTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	authPath, err := writeWebDAVAuthFile(t.TempDir(), "protondrive", "runtime-secret")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rcloneBin, "serve", "webdav", t.TempDir(), "--addr", addr, "--htpasswd", authPath) // #nosec G204 -- opt-in test executes the explicitly supplied rclone binary
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}()
	if err := waitForWebDAV("http://"+addr+"/", "protondrive", "runtime-secret", 10*time.Second, done); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveWebDAVMountIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("mount_webdav is macOS-specific")
	}
	url := strings.TrimSpace(os.Getenv("PROTONDRIVE_WEBDAV_MOUNT_TEST_URL"))
	password := os.Getenv("PROTONDRIVE_WEBDAV_MOUNT_TEST_PASSWORD")
	if url == "" || password == "" {
		t.Skip("set PROTONDRIVE_WEBDAV_MOUNT_TEST_URL and PROTONDRIVE_WEBDAV_MOUNT_TEST_PASSWORD to run the real mount test")
	}
	username := strings.TrimSpace(os.Getenv("PROTONDRIVE_WEBDAV_MOUNT_TEST_USERNAME"))
	if username == "" {
		username = "protondrive"
	}
	mountPoint := t.TempDir()
	t.Cleanup(func() {
		_ = exec.Command("diskutil", "unmount", mountPoint).Run()
	})
	if err := runInteractiveWebDAVMount(url, mountPoint, username, password, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	mounted, err := isPathMounted(mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	if !mounted {
		t.Fatalf("mount_webdav returned success but %s is not mounted", mountPoint)
	}
}

func TestRunSyncUsesCopyByDefaultAndGuardsMirror(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "document.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tmp, "rclone.log")
	fakeRclone := filepath.Join(tmp, "rclone")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$RCLONE_FAKE_LOG"
exit 0
`
	if err := os.WriteFile(fakeRclone, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RCLONE_FAKE_LOG", logPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	previous := currentOptions
	currentOptions = runtimeOptions{Remote: remoteDefault, Backend: backendRclone, ProtonDriveBin: filepath.Join(tmp, "proton-drive"), RcloneBin: fakeRclone}
	t.Cleanup(func() { currentOptions = previous })

	if err := runSync(remoteDefault, []string{source, "--remote-path", "Backups", "--no-progress"}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := runSync(remoteDefault, []string{source, "--remote-path", "Backups", "--operation", "mirror", "--no-progress"}); err == nil || !strings.Contains(err.Error(), "--confirm-mirror") {
		t.Fatalf("unconfirmed mirror error = %v", err)
	}
	if err := runSync(remoteDefault, []string{source, "--remote-path", "Backups", "--operation", "mirror", "--dry-run", "--no-progress", "--", "--checksum"}); err != nil {
		t.Fatalf("mirror dry-run: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "copy "+source+" protondrive:Backups") {
		t.Fatalf("copy command missing:\n%s", logText)
	}
	if !strings.Contains(logText, "sync "+source+" protondrive:Backups") || !strings.Contains(logText, "--max-delete 25") || !strings.Contains(logText, "--backup-dir protondrive:.protondrive-backups/Backups/") {
		t.Fatalf("guarded mirror command missing:\n%s", logText)
	}
	if strings.Contains(logText, " -- --checksum") {
		t.Fatalf("wrapper delimiter leaked to rclone:\n%s", logText)
	}
	for _, protected := range []string{"--dry-run=false", "--max-delete=-1", "--backup-dir=", "--ignore-errors", "--inplace", "--delete-excluded"} {
		err := runSync(remoteDefault, []string{source, "--remote-path", "Backups", "--operation", "mirror", "--dry-run", "--no-progress", "--", protected})
		if err == nil {
			t.Fatalf("protected rclone passthrough %q was accepted", protected)
		}
	}
}

func TestMirrorBackupDirectoryMustBeOutsideDestination(t *testing.T) {
	for _, tc := range []struct {
		destination string
		backup      string
		local       bool
	}{
		{destination: "protondrive:Backups", backup: "protondrive:Backups/.history", local: false},
		{destination: "protondrive:", backup: "protondrive:.protondrive-backups/root", local: false},
		{destination: "/tmp/restore", backup: "/tmp/restore/.history", local: true},
	} {
		if err := validateMirrorBackupDestination(tc.destination, tc.backup, tc.local); err == nil {
			t.Fatalf("unsafe backup %#v was accepted", tc)
		}
	}
	for _, tc := range []struct {
		destination string
		backup      string
		local       bool
	}{
		{destination: "protondrive:Backups", backup: "protondrive:.protondrive-backups/Backups", local: false},
		{destination: "protondrive:", backup: "archive:root-backup", local: false},
		{destination: "/tmp/restore", backup: "/tmp/.protondrive-backups/restore", local: true},
	} {
		if err := validateMirrorBackupDestination(tc.destination, tc.backup, tc.local); err != nil {
			t.Fatalf("safe backup %#v was rejected: %v", tc, err)
		}
	}
}

func TestStatusMissingRemoteHasStableExitCodeAndJSON(t *testing.T) {
	tmp := t.TempDir()
	fakeRclone := filepath.Join(tmp, "rclone")
	if err := os.WriteFile(fakeRclone, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousOptions := currentOptions
	currentOptions = runtimeOptions{Remote: remoteDefault, Backend: backendRclone, RcloneBin: fakeRclone}
	t.Cleanup(func() { currentOptions = previousOptions })

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStdout := os.Stdout
	os.Stdout = write
	err = runStatus(remoteDefault, []string{"--json"})
	_ = write.Close()
	os.Stdout = previousStdout
	output, errRead := io.ReadAll(read)
	_ = read.Close()
	if errRead != nil {
		t.Fatal(errRead)
	}
	var report statusReport
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("JSON %q: %v", output, err)
	}
	coded, ok := err.(statusExitError)
	if !ok || coded.ExitCode() != statusExitNotConfigured {
		t.Fatalf("status error = %#v", err)
	}
	if report.Healthy || report.Configured {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestStatusJSONAuthFailureIsNonInteractiveAndValid(t *testing.T) {
	tmp := t.TempDir()
	fakeRclone := filepath.Join(tmp, "rclone")
	script := `#!/bin/sh
if [ "$1" = "listremotes" ]; then
  printf 'protondrive:\n'
  exit 0
fi
if [ "$1" = "lsd" ]; then
  printf 'username and password are required\n' >&2
  exit 1
fi
exit 0
`
	if err := os.WriteFile(fakeRclone, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv(vaultPassphraseEnv, "")
	previousOptions := currentOptions
	currentOptions = runtimeOptions{Remote: remoteDefault, Backend: backendRclone, RcloneBin: fakeRclone}
	t.Cleanup(func() { currentOptions = previousOptions })

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStdout := os.Stdout
	os.Stdout = write
	statusErr := runStatus(remoteDefault, []string{"--json"})
	_ = write.Close()
	os.Stdout = previousStdout
	output, readErr := io.ReadAll(read)
	_ = read.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	var report statusReport
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("status output is not one valid JSON document: %q: %v", output, err)
	}
	coded, ok := statusErr.(statusExitError)
	if !ok || coded.ExitCode() != statusExitAuthFailed {
		t.Fatalf("status error = %#v, want auth exit %d", statusErr, statusExitAuthFailed)
	}
	if report.Healthy || report.Authenticated || !report.Configured {
		t.Fatalf("unexpected auth-failure report: %#v", report)
	}
}

func TestIsProtonCLIAuthErrorRejectsTransportFailures(t *testing.T) {
	for _, message := range []string{
		"You need to login first",
		"authentication required",
		"session expired",
	} {
		if !isProtonCLIAuthError(errors.New(message)) {
			t.Fatalf("expected auth classification for %q", message)
		}
	}
	for _, message := range []string{
		"TLS handshake timeout while contacting API",
		"connection reset by peer",
		"provider returned an unexpected response",
	} {
		if isProtonCLIAuthError(errors.New(message)) {
			t.Fatalf("unexpected auth classification for %q", message)
		}
	}
}
