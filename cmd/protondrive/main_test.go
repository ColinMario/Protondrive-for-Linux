package main

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		"--",
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
