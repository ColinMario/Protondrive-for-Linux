package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ColinMario/Protondrive-for-Linux/internal/safefile"
)

func configureRemote(remote, email, password, mailboxPassword string, quiet bool) error {
	if !quiet {
		fmt.Printf("Configuring rclone remote '%s'...\n", remote)
	}

	obscuredPassword, err := obscureRcloneSecret(password)
	if err != nil {
		return fmt.Errorf("failed to process password: %w", err)
	}

	values := map[string]string{
		"type":     "protondrive",
		"username": strings.TrimSpace(email),
		"password": strings.TrimSpace(obscuredPassword),
	}
	if strings.TrimSpace(mailboxPassword) != "" {
		obscuredMailboxPassword, err := obscureRcloneSecret(mailboxPassword)
		if err != nil {
			return fmt.Errorf("failed to process mailbox password: %w", err)
		}
		values["mailbox_password"] = strings.TrimSpace(obscuredMailboxPassword)
	}
	configPath, err := rcloneConfigFilePath()
	if err != nil {
		return err
	}
	if err := writeRcloneConfigSection(configPath, normalizedRemoteName(remote), values); err != nil {
		return fmt.Errorf("rclone config update failed: %w", err)
	}
	if !quiet {
		fmt.Println("Remote saved successfully.")
	}
	return nil
}

type protonCLISessionSnapshot struct {
	CachePassword   string `json:"cachePassword"`
	UserKeyPassword string `json:"userKeyPassword"`
	Session         struct {
		UID          string `json:"uid"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	} `json:"session"`
}

var errProtonCLISessionNotFound = errors.New("proton CLI session secret not found")

type protonCLISessionStoreSnapshot struct {
	Exists  bool
	Payload []byte
	Mode    fs.FileMode
}

func configureRemoteFromProtonCLISession(remote string, verify bool) error {
	if err := validateProtonSessionBridgeVersion(false); err != nil {
		return err
	}
	sessionSnapshot, err := loadProtonCLISessionSnapshot()
	if err != nil {
		return err
	}
	if strings.TrimSpace(sessionSnapshot.UserKeyPassword) == "" ||
		strings.TrimSpace(sessionSnapshot.Session.UID) == "" ||
		strings.TrimSpace(sessionSnapshot.Session.AccessToken) == "" ||
		strings.TrimSpace(sessionSnapshot.Session.RefreshToken) == "" {
		return errors.New("official Proton Drive CLI session is incomplete; run 'protondrive --backend proton configure' first")
	}

	obscuredPlaceholder, err := obscureRcloneSecret("proton-cli-session-placeholder")
	if err != nil {
		return fmt.Errorf("failed to prepare rclone placeholder password: %w", err)
	}
	configPath, err := rcloneConfigFilePath()
	if err != nil {
		return err
	}
	configSnapshot, err := captureRcloneConfigSnapshot(configPath)
	if err != nil {
		return err
	}
	values := map[string]string{
		"type":                   "protondrive",
		"username":               "proton-cli-session",
		"password":               strings.TrimSpace(obscuredPlaceholder),
		"client_uid":             sessionSnapshot.Session.UID,
		"client_access_token":    sessionSnapshot.Session.AccessToken,
		"client_refresh_token":   sessionSnapshot.Session.RefreshToken,
		"client_salted_key_pass": base64.StdEncoding.EncodeToString([]byte(sessionSnapshot.UserKeyPassword)),
		"enable_caching":         "false",
	}
	if err := writeRcloneConfigSection(configPath, normalizedRemoteName(remote), values); err != nil {
		return err
	}
	fmt.Printf("Imported official Proton Drive CLI session into rclone remote '%s'.\n", normalizedRemoteName(remote))
	fmt.Printf("Updated rclone config: %s\n", configPath)
	if verify {
		fmt.Println("Verifying imported rclone session...")
		if err := verifyRemote(remote); err != nil {
			return rollbackRcloneConfigError(configPath, configSnapshot, fmt.Errorf("imported session could not be verified: %w", err))
		}
		fmt.Println("Imported rclone session verified.")
	}
	return nil
}

func configureProtonCLISessionFromRcloneRemote(remote string, verify bool) error {
	if err := validateProtonSessionBridgeVersion(!verify); err != nil {
		return err
	}
	snapshot, err := protonCLISessionFromRcloneRemote(remote)
	if err != nil {
		return err
	}
	previous, err := captureProtonCLISessionStoreSnapshot()
	if err != nil {
		return fmt.Errorf("unable to snapshot the existing Proton CLI session before export: %w", err)
	}
	target, err := writeProtonCLISessionSnapshot(snapshot)
	if err != nil {
		return rollbackProtonCLISessionError(previous, err)
	}
	fmt.Printf("Wrote official Proton Drive CLI session to %s.\n", target)
	if verify {
		if err := ensureProtonDrive(); err != nil {
			return rollbackProtonCLISessionError(previous, fmt.Errorf("proton CLI session could not be verified: %w", err))
		}
		fmt.Println("Verifying Proton CLI session...")
		if _, err := runProtonDriveCapture("filesystem", "list", "/"); err != nil {
			return rollbackProtonCLISessionError(previous, fmt.Errorf("proton CLI session verification failed: %w", err))
		}
		fmt.Println("Browserless Proton CLI session verified.")
	}
	return nil
}

func validateProtonSessionBridgeVersion(allowUnavailable bool) error {
	if truthyEnv(os.Getenv("PROTONDRIVE_UNSAFE_SESSION_BRIDGE")) {
		fmt.Fprintln(os.Stderr, "Warning: bypassing Proton CLI private-session compatibility check.")
		return nil
	}
	if err := ensureProtonDrive(); err != nil {
		if allowUnavailable {
			fmt.Fprintln(os.Stderr, "Warning: Proton CLI is unavailable; private-session compatibility could not be checked.")
			return nil
		}
		return err
	}
	output, err := runProtonDriveCapture("version")
	if err != nil {
		return fmt.Errorf("unable to verify Proton CLI session compatibility: %w", err)
	}
	return validateProtonSessionBridgeVersionOutput(output)
}

func validateProtonSessionBridgeVersionOutput(output string) error {
	match := regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`).FindStringSubmatch(output)
	if len(match) != 4 {
		return fmt.Errorf("cannot recognize Proton CLI version %q; refusing to manipulate its private session format", strings.TrimSpace(output))
	}
	if match[1] != "0" || (match[2] != "4" && match[2] != "5") {
		return fmt.Errorf("proton CLI %s is outside the validated private-session bridge series 0.4.x and 0.5.x; use official browser authentication or set PROTONDRIVE_UNSAFE_SESSION_BRIDGE=true only after verifying the upstream format", match[0])
	}
	return nil
}

func protonCLISessionFromRcloneRemote(remote string) (protonCLISessionSnapshot, error) {
	configPath, err := rcloneConfigFilePath()
	if err != nil {
		return protonCLISessionSnapshot{}, err
	}
	values, err := readRcloneConfigSection(configPath, normalizedRemoteName(remote))
	if err != nil {
		return protonCLISessionSnapshot{}, err
	}
	return protonCLISessionFromRcloneConfigValues(values)
}

func protonCLISessionFromRcloneConfigValues(values map[string]string) (protonCLISessionSnapshot, error) {
	required := []string{"client_uid", "client_access_token", "client_refresh_token", "client_salted_key_pass"}
	for _, key := range required {
		if strings.TrimSpace(values[key]) == "" {
			return protonCLISessionSnapshot{}, fmt.Errorf("rclone Proton Drive remote is missing %s; run browserless configure without --skip-verify so rclone can initialize cached tokens", key)
		}
	}
	userKeyPassword := decodeRcloneSaltedKeyPass(values["client_salted_key_pass"])
	cachePassword, err := randomBase64(32)
	if err != nil {
		return protonCLISessionSnapshot{}, err
	}

	var snapshot protonCLISessionSnapshot
	snapshot.CachePassword = cachePassword
	snapshot.UserKeyPassword = userKeyPassword
	snapshot.Session.UID = strings.TrimSpace(values["client_uid"])
	snapshot.Session.AccessToken = strings.TrimSpace(values["client_access_token"])
	snapshot.Session.RefreshToken = strings.TrimSpace(values["client_refresh_token"])
	return snapshot, nil
}

func decodeRcloneSaltedKeyPass(value string) string {
	value = strings.TrimSpace(value)
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 {
		return value
	}
	text := string(decoded)
	if !isPrintableSecret(text) {
		return value
	}
	return text
}

func isPrintableSecret(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func randomBase64(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func loadProtonCLISessionSnapshot() (protonCLISessionSnapshot, error) {
	raw, err := loadProtonCLISessionRaw()
	if err != nil {
		return protonCLISessionSnapshot{}, fmt.Errorf("unable to read official Proton Drive CLI session; run 'protondrive --backend proton configure' first: %w", err)
	}
	var snapshot protonCLISessionSnapshot
	if err := json.Unmarshal(bytes.TrimSpace(raw), &snapshot); err != nil {
		return protonCLISessionSnapshot{}, fmt.Errorf("official Proton Drive CLI session has unexpected format: %w", err)
	}
	return snapshot, nil
}

func loadProtonCLISessionRaw() ([]byte, error) {
	if truthyEnv(os.Getenv("PROTON_DRIVE_UNSAFE_SECRETS")) {
		path, err := protonCLIUnsafeSessionFilePath()
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- explicitly configured Proton CLI compatibility path
		if errors.Is(err, os.ErrNotExist) {
			return nil, errProtonCLISessionNotFound
		}
		return raw, err
	}
	switch runtime.GOOS {
	case "darwin":
		output, err := safeExecCommand("security", "find-generic-password", "-s", protonCLISecretService, "-a", protonCLISecretName, "-w").CombinedOutput()
		if err != nil {
			if protonCLISessionSecretMissing(output) {
				return nil, errProtonCLISessionNotFound
			}
			return nil, fmt.Errorf("macOS Keychain lookup failed: %w", err)
		}
		if len(bytes.TrimSpace(output)) == 0 {
			return nil, errProtonCLISessionNotFound
		}
		return output, nil
	case "linux":
		return loadProtonCLISessionFromSecretTool()
	default:
		return nil, fmt.Errorf("importing the official Proton Drive CLI session is not implemented on %s", runtime.GOOS)
	}
}

func loadProtonCLISessionFromSecretTool() ([]byte, error) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return nil, errors.New("secret-tool not found; install libsecret-tools or configure rclone with username/password")
	}
	candidates := [][]string{
		{"lookup", "service", protonCLISecretService, "name", protonCLISecretName},
		{"lookup", "service", protonCLISecretService, "account", protonCLISecretName},
		{"lookup", "application", protonCLISecretService, "name", protonCLISecretName},
	}
	var lastDiagnostic string
	for _, args := range candidates {
		out, err := safeExecCommand("secret-tool", args...).CombinedOutput() // #nosec G204 -- fixed secret-tool command with fixed attribute candidates
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return out, nil
		}
		if diagnostic := strings.TrimSpace(string(out)); diagnostic != "" {
			lastDiagnostic = diagnostic
		}
	}
	if lastDiagnostic != "" {
		return nil, fmt.Errorf("system Secret Service lookup failed: %s", lastDiagnostic)
	}
	return nil, errProtonCLISessionNotFound
}

func writeProtonCLISessionSnapshot(snapshot protonCLISessionSnapshot) (string, error) {
	if strings.TrimSpace(snapshot.CachePassword) == "" ||
		strings.TrimSpace(snapshot.UserKeyPassword) == "" ||
		strings.TrimSpace(snapshot.Session.UID) == "" ||
		strings.TrimSpace(snapshot.Session.AccessToken) == "" ||
		strings.TrimSpace(snapshot.Session.RefreshToken) == "" {
		return "", errors.New("cannot write incomplete Proton CLI session")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	return writeProtonCLISessionPayload(payload)
}

func writeProtonCLISessionPayload(payload []byte) (string, error) {
	if truthyEnv(os.Getenv("PROTON_DRIVE_UNSAFE_SECRETS")) {
		path, err := writeProtonCLIUnsafeSessionFile(payload)
		if err != nil {
			return "", err
		}
		return path, nil
	}
	switch runtime.GOOS {
	case "darwin":
		if err := writeProtonCLISessionWithBun(payload); err == nil {
			return fmt.Sprintf("macOS Keychain via Bun.secrets (%s/%s)", protonCLISecretService, protonCLISecretName), nil
		} else {
			return "", fmt.Errorf("safe macOS Keychain writer unavailable: %w; install Bun so the session is read from a protected temporary file, or authenticate with the official Proton CLI", err)
		}
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return "", errors.New("secret-tool not found; install libsecret-tools to write the Proton CLI session, or set PROTON_DRIVE_UNSAFE_SECRETS=true with PROTON_DRIVE_CACHE_DIR for a plaintext file session")
		}
		cmd := safeExecCommand("secret-tool", "store", "--label", "Proton Drive CLI session", "service", protonCLISecretService, "name", protonCLISecretName)
		cmd.Stdin = bytes.NewReader(payload)
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("unable to write Proton CLI session to Secret Service: %s", strings.TrimSpace(string(output)))
		}
		return fmt.Sprintf("Secret Service (%s/%s)", protonCLISecretService, protonCLISecretName), nil
	default:
		return "", fmt.Errorf("writing the Proton CLI session is not implemented on %s", runtime.GOOS)
	}
}

func captureProtonCLISessionStoreSnapshot() (protonCLISessionStoreSnapshot, error) {
	raw, err := loadProtonCLISessionRaw()
	if errors.Is(err, errProtonCLISessionNotFound) {
		return protonCLISessionStoreSnapshot{}, nil
	}
	if err != nil {
		return protonCLISessionStoreSnapshot{}, err
	}
	snapshot := protonCLISessionStoreSnapshot{Exists: true, Payload: append([]byte(nil), raw...), Mode: 0o600}
	if truthyEnv(os.Getenv("PROTON_DRIVE_UNSAFE_SECRETS")) {
		path, pathErr := protonCLIUnsafeSessionFilePath()
		if pathErr != nil {
			return protonCLISessionStoreSnapshot{}, pathErr
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return protonCLISessionStoreSnapshot{}, statErr
		}
		snapshot.Mode = info.Mode().Perm()
	}
	return snapshot, nil
}

func restoreProtonCLISessionStoreSnapshot(snapshot protonCLISessionStoreSnapshot) error {
	if !snapshot.Exists {
		return removeProtonCLISessionPayload()
	}
	if _, err := writeProtonCLISessionPayload(snapshot.Payload); err != nil {
		return err
	}
	if truthyEnv(os.Getenv("PROTON_DRIVE_UNSAFE_SECRETS")) {
		path, err := protonCLIUnsafeSessionFilePath()
		if err != nil {
			return err
		}
		if err := os.Chmod(path, snapshot.Mode); err != nil {
			return err
		}
	}
	return nil
}

func rollbackProtonCLISessionError(snapshot protonCLISessionStoreSnapshot, cause error) error {
	if err := restoreProtonCLISessionStoreSnapshot(snapshot); err != nil {
		return errors.Join(cause, fmt.Errorf("failed to restore previous Proton CLI session: %w", err))
	}
	return fmt.Errorf("%w (previous Proton CLI session state restored)", cause)
}

func removeProtonCLISessionPayload() error {
	if truthyEnv(os.Getenv("PROTON_DRIVE_UNSAFE_SECRETS")) {
		path, err := protonCLIUnsafeSessionFilePath()
		if err != nil {
			return err
		}
		return safefile.Remove(path)
	}
	switch runtime.GOOS {
	case "darwin":
		output, err := safeExecCommand("security", "delete-generic-password", "-s", protonCLISecretService, "-a", protonCLISecretName).CombinedOutput()
		if err == nil || protonCLISessionSecretMissing(output) {
			return nil
		}
		return fmt.Errorf("macOS Keychain delete failed: %w", err)
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return errors.New("secret-tool not found while rolling back Proton CLI session")
		}
		output, err := safeExecCommand("secret-tool", "clear", "service", protonCLISecretService, "name", protonCLISecretName).CombinedOutput()
		if err == nil || len(bytes.TrimSpace(output)) == 0 {
			return nil
		}
		return fmt.Errorf("system Secret Service delete failed: %s", strings.TrimSpace(string(output)))
	default:
		return fmt.Errorf("removing the Proton CLI session is not implemented on %s", runtime.GOOS)
	}
}

func protonCLISessionSecretMissing(output []byte) bool {
	lower := strings.ToLower(string(output))
	for _, marker := range []string{"could not be found", "item not found", "not found", "no such item"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func writeProtonCLISessionWithBun(payload []byte) error {
	bunPath, err := exec.LookPath("bun")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "protondrive-bun-secret-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	payloadPath := filepath.Join(dir, "session.json")
	scriptPath := filepath.Join(dir, "write-session.js")
	if err := safefile.Write(payloadPath, payload, 0o600); err != nil {
		return err
	}
	script := fmt.Sprintf(`const value = await Bun.file(process.argv[2]).text();
await Bun.secrets.set({ service: %q, name: %q, value });
`, protonCLISecretService, protonCLISecretName)
	if err := safefile.Write(scriptPath, []byte(script), 0o600); err != nil {
		return err
	}
	cmd := safeExecCommand(bunPath, scriptPath, payloadPath) // #nosec G204 -- bun path is resolved via exec.LookPath and arguments are temp files
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bun secrets writer failed: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func writeProtonCLIUnsafeSessionFile(payload []byte) (string, error) {
	path, err := protonCLIUnsafeSessionFilePath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := safefile.Write(path, payload, 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func protonCLIUnsafeSessionFilePath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("PROTON_DRIVE_CACHE_DIR")); override != "" {
		return filepath.Join(expandPath(override), "auth-session.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "proton-drive-cli", "auth-session.json"), nil
	case "linux":
		dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
		if dataHome == "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
		return filepath.Join(expandPath(dataHome), "proton-drive-cli", "auth-session.json"), nil
	case "windows":
		localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(localAppData, "proton-drive-cli", "Data", "auth-session.json"), nil
	default:
		configDir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(configDir, "proton-drive-cli", "auth-session.json"), nil
	}
}

func truthyEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func rcloneConfigFilePath() (string, error) {
	if config := strings.TrimSpace(os.Getenv("RCLONE_CONFIG")); config != "" {
		return expandPath(config), nil
	}
	output, err := runRcloneCapture("config", "file")
	if err != nil {
		return "", err
	}
	var candidate string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") || strings.HasPrefix(line, "~") || filepath.IsAbs(line) {
			candidate = line
		}
	}
	if candidate == "" {
		return "", fmt.Errorf("unable to parse rclone config path from: %s", strings.TrimSpace(output))
	}
	return expandPath(candidate), nil
}

func obscureRcloneSecret(secret string) (string, error) {
	cmd := externalCommand(currentOptions.RcloneBin, "obscure", "-")
	cmd.Stdin = strings.NewReader(secret + "\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s obscure failed: %s", currentOptions.RcloneBin, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func readRcloneConfigSection(configPath, section string) (map[string]string, error) {
	validatedSection, err := validateRemoteName(section)
	if err != nil {
		return nil, err
	}
	section = validatedSection
	data, err := os.ReadFile(configPath) // #nosec G304 -- rclone config path is resolved from RCLONE_CONFIG or rclone itself
	if err != nil {
		return nil, err
	}
	if rcloneConfigEncrypted(data) {
		return nil, errors.New("encrypted rclone configs are not supported by the wrapper's transactional editor; use a dedicated plaintext RCLONE_CONFIG with mode 0600")
	}
	header := "[" + strings.TrimSpace(section) + "]"
	values := make(map[string]string)
	inTarget := false
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inTarget = trimmed == header
			continue
		}
		if !inTarget {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("rclone remote %q not found in %s", section, configPath)
	}
	return values, nil
}

func writeRcloneConfigSection(configPath, section string, values map[string]string) error {
	return updateRcloneConfigSection(configPath, section, values, true)
}

func writeRcloneConfigSectionWithoutBackup(configPath, section string, values map[string]string) error {
	return updateRcloneConfigSection(configPath, section, values, false)
}

func updateRcloneConfigSection(configPath, section string, values map[string]string, backup bool) error {
	validatedSection, err := validateRemoteName(section)
	if err != nil {
		return err
	}
	section = validatedSection
	if strings.TrimSpace(os.Getenv("RCLONE_CONFIG_PASS")) != "" {
		return errors.New("RCLONE_CONFIG_PASS is set, but the wrapper cannot safely rewrite encrypted rclone configs; use a dedicated plaintext RCLONE_CONFIG with mode 0600")
	}
	update := func(current []byte) ([]byte, fs.FileMode, error) {
		if rcloneConfigEncrypted(current) {
			return nil, 0, errors.New("refusing to overwrite an encrypted rclone config; use a dedicated plaintext RCLONE_CONFIG with mode 0600")
		}
		return renderRcloneConfigSection(current, section, values), 0o600, nil
	}
	if backup {
		return safefile.UpdateWithBackup(configPath, 0o600, ".protondrive.bak", update)
	}
	return safefile.Update(configPath, 0o600, update)
}

func rcloneConfigEncrypted(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(data), []byte("RCLONE_ENCRYPT_V0:"))
}

func renderRcloneConfigSection(current []byte, section string, values map[string]string) []byte {
	lines := strings.Split(strings.ReplaceAll(string(current), "\r\n", "\n"), "\n")
	header := "[" + section + "]"
	var out []string
	var preservedTarget []string
	inTarget := false
	managed := make(map[string]bool)
	for _, key := range managedRcloneConfigKeys() {
		managed[key] = true
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inTarget = trimmed == header
			if inTarget {
				continue
			}
		}
		if inTarget {
			key, _, hasValue := strings.Cut(trimmed, "=")
			if !hasValue || !managed[strings.TrimSpace(key)] {
				preservedTarget = append(preservedTarget, line)
			}
			continue
		}
		if len(out) == 0 && strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	out = append(out, header)
	for _, key := range managedRcloneConfigKeys() {
		value, ok := values[key]
		if ok {
			out = append(out, fmt.Sprintf("%s = %s", key, sanitizeConfigValue(value)))
		}
	}
	for len(preservedTarget) > 0 && strings.TrimSpace(preservedTarget[0]) == "" {
		preservedTarget = preservedTarget[1:]
	}
	for len(preservedTarget) > 0 && strings.TrimSpace(preservedTarget[len(preservedTarget)-1]) == "" {
		preservedTarget = preservedTarget[:len(preservedTarget)-1]
	}
	out = append(out, preservedTarget...)
	return []byte(strings.Join(out, "\n") + "\n")
}

func managedRcloneConfigKeys() []string {
	return []string{
		"type", "username", "password", "mailbox_password", "2fa",
		"client_uid", "client_access_token", "client_refresh_token",
		"client_salted_key_pass", "enable_caching",
	}
}

type rcloneConfigSnapshot struct {
	Exists bool
	Data   []byte
	Mode   fs.FileMode
}

func captureRcloneConfigSnapshot(configPath string) (rcloneConfigSnapshot, error) {
	info, err := os.Stat(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return rcloneConfigSnapshot{Mode: 0o600}, nil
	}
	if err != nil {
		return rcloneConfigSnapshot{}, err
	}
	data, err := os.ReadFile(configPath) // #nosec G304 -- caller supplies the resolved rclone config path
	if err != nil {
		return rcloneConfigSnapshot{}, err
	}
	return rcloneConfigSnapshot{Exists: true, Data: data, Mode: info.Mode().Perm()}, nil
}

func rollbackRcloneConfigError(configPath string, snapshot rcloneConfigSnapshot, cause error) error {
	var rollbackErr error
	if snapshot.Exists {
		rollbackErr = safefile.Write(configPath, snapshot.Data, snapshot.Mode)
	} else {
		rollbackErr = safefile.Remove(configPath)
	}
	if err := safefile.Remove(configPath + ".protondrive.bak"); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove transient config backup: %w", err))
	}
	if rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("failed to restore previous rclone config: %w", rollbackErr))
	}
	return fmt.Errorf("%w (previous rclone config restored)", cause)
}

func sanitizeConfigValue(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return strings.TrimSpace(value)
}

func verifyRemote(remote string) error {
	_, err := runRcloneCapture("lsd", remotePath(remote, ""))
	return err
}

func verifyRemoteWithOneTimeCode(remote, configPath, code string) (returnErr error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return verifyRemote(remote)
	}
	current, err := os.ReadFile(configPath) // #nosec G304 -- caller supplies the already-resolved rclone config path
	if err != nil {
		return err
	}
	values, err := readRcloneConfigSection(configPath, normalizedRemoteName(remote))
	if err != nil {
		return err
	}
	values["2fa"] = code
	temp, err := os.CreateTemp(filepath.Dir(configPath), ".protondrive-2fa-*.conf")
	if err != nil {
		return fmt.Errorf("unable to stage one-time 2FA config: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	defer func() {
		if err := safefile.Remove(tempPath); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("unable to remove one-time 2FA config: %w", err))
		}
	}()
	if err := safefile.Write(tempPath, renderRcloneConfigSection(current, normalizedRemoteName(remote), values), 0o600); err != nil {
		return err
	}
	if _, err := runRcloneCaptureWithConfig(tempPath, "lsd", remotePath(remote, "")); err != nil {
		return err
	}
	refreshed, err := readRcloneConfigSection(tempPath, normalizedRemoteName(remote))
	if err != nil {
		return fmt.Errorf("unable to read refreshed rclone session: %w", err)
	}
	delete(refreshed, "2fa")
	if err := writeRcloneConfigSectionWithoutBackup(configPath, normalizedRemoteName(remote), refreshed); err != nil {
		return fmt.Errorf("unable to persist refreshed rclone session without the one-time 2FA code: %w", err)
	}
	return nil
}

func ensureRemoteAuth(remote string) error {
	return ensureRemoteAuthMode(remote, true)
}

func ensureRemoteAuthForStatus(remote string) error {
	return ensureRemoteAuthMode(remote, false)
}

func ensureRemoteAuthMode(remote string, allowPrompt bool) error {
	if authErr := verifyRemote(remote); authErr != nil {
		recordAuthEvent(remote, "verify", false, strings.TrimSpace(authErr.Error()))
		if !isAuthError(authErr) {
			return authErr
		}
		if !hasStoredCredentials(remote) {
			return fmt.Errorf("%w; re-run 'protondrive configure --store-credentials' to enable auto-refresh", authErr)
		}
		if !allowPrompt && strings.TrimSpace(os.Getenv(vaultPassphraseEnv)) == "" {
			return fmt.Errorf("%w; automatic refresh requires %s in non-interactive status checks", authErr, vaultPassphraseEnv)
		}
		if allowPrompt {
			fmt.Println("Remote authentication failed. Attempting to refresh credentials...")
		}
		if refreshErr := tryAutoRefresh(remote, allowPrompt); refreshErr != nil {
			recordAuthEvent(remote, "auto-refresh", false, strings.TrimSpace(refreshErr.Error()))
			return errors.Join(authErr, fmt.Errorf("automatic refresh failed: %w", refreshErr))
		}
		if err := verifyRemote(remote); err != nil {
			recordAuthEvent(remote, "auto-refresh", false, strings.TrimSpace(err.Error()))
			return err
		}
		recordAuthEvent(remote, "auto-refresh", true, "")
		return nil
	}

	recordAuthEvent(remote, "verify", true, "")
	return nil
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if isTransientTransportMessage(msg) {
		return false
	}
	if strings.Contains(msg, "username and password are required") {
		return true
	}
	if strings.Contains(msg, "couldn't initialize a new proton drive instance") {
		return true
	}
	if strings.Contains(msg, "401") && strings.Contains(msg, "unauthorized") {
		return true
	}
	if strings.Contains(msg, "invalid_grant") {
		return true
	}
	if strings.Contains(msg, "token") && strings.Contains(msg, "expired") {
		return true
	}
	if strings.Contains(msg, "session") && strings.Contains(msg, "expired") {
		return true
	}
	if strings.Contains(msg, "refresh token") && (strings.Contains(msg, "invalid") || strings.Contains(msg, "expired")) {
		return true
	}
	return false
}

func isTransientTransportMessage(msg string) bool {
	for _, marker := range []string{
		"context deadline exceeded", "i/o timeout", "connection timed out",
		"connection reset by peer", "connection refused", "no such host",
		"temporary failure in name resolution", "tls handshake timeout",
		"temporarily unavailable", "service unavailable", "bad gateway",
		"broken pipe", "use of closed network connection", "network is unreachable",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func tryAutoRefresh(remote string, allowPrompt bool) error {
	passphrase := strings.TrimSpace(os.Getenv(vaultPassphraseEnv))
	var err error
	if passphrase == "" {
		if !allowPrompt {
			return fmt.Errorf("%s is required for non-interactive credential refresh", vaultPassphraseEnv)
		}
		passphrase, err = promptPassword("Credential vault passphrase: ")
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(passphrase) == "" {
		return errors.New("credential vault passphrase cannot be empty")
	}

	creds, err := loadEncryptedCredentials(remote, passphrase)
	if err != nil {
		return err
	}
	// Re-encrypt legacy vaults with the current schema so one-time 2FA values
	// written by <=0.2.5 are removed as soon as the vault is unlocked.
	if _, err := saveEncryptedCredentials(remote, creds, passphrase); err != nil {
		return fmt.Errorf("unable to migrate credential vault: %w", err)
	}
	configPath, err := rcloneConfigFilePath()
	if err != nil {
		return err
	}
	snapshot, err := captureRcloneConfigSnapshot(configPath)
	if err != nil {
		return err
	}
	if err := configureRemote(remote, creds.Email, creds.Password, creds.MailboxPassword, true); err != nil {
		return err
	}
	if err := verifyRemote(remote); err != nil {
		return rollbackRcloneConfigError(configPath, snapshot, fmt.Errorf("refreshed credentials could not be verified: %w", err))
	}
	if allowPrompt {
		fmt.Println("Credentials refreshed from the local vault.")
	}
	return nil
}

func resolveVaultPassphrase(reader *bufio.Reader, provided string, fromStdin bool, nonInteractive bool) (string, error) {
	if strings.TrimSpace(provided) != "" {
		return provided, nil
	}
	if fromStdin {
		text, err := readLine(reader)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(text) == "" {
			return "", errors.New("vault passphrase cannot be empty")
		}
		return text, nil
	}
	if nonInteractive {
		return "", errors.New("vault passphrase must be provided via --vault-passphrase when running non-interactively")
	}
	first, err := promptPassword("Credential vault passphrase: ")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(first) == "" {
		return "", errors.New("passphrase cannot be empty")
	}
	second, err := promptPassword("Confirm passphrase: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("passphrases did not match")
	}
	return first, nil
}

type storedCredentials struct {
	Email           string    `json:"email"`
	Password        string    `json:"password"`
	MailboxPassword string    `json:"mailbox_password,omitempty"`
	SavedAt         time.Time `json:"saved_at"`
}

type encryptedCredentialBlob struct {
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

func saveEncryptedCredentials(remote string, creds storedCredentials, passphrase string) (string, error) {
	payload, err := encryptCredentials(passphrase, creds)
	if err != nil {
		return "", err
	}
	dir, err := ensureCredentialDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, credentialFilename(remote))
	if err := safefile.Write(path, payload, 0o600); err != nil {
		return "", err
	}
	legacy := filepath.Join(dir, sanitizedRemoteName(remote)+".creds")
	if legacy != path {
		if err := safefile.Remove(legacy); err != nil {
			return path, fmt.Errorf("new credential vault was saved but legacy vault cleanup failed: %w", err)
		}
	}
	return path, nil
}

func loadEncryptedCredentials(remote, passphrase string) (storedCredentials, error) {
	path, err := credentialFilePath(remote)
	if err != nil {
		return storedCredentials{}, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- credential path is generated under the app config directory
	if err != nil {
		return storedCredentials{}, err
	}
	return decryptCredentials(passphrase, data)
}

func hasStoredCredentials(remote string) bool {
	path, err := credentialFilePath(remote)
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

func credentialFilename(remote string) string {
	return remoteStorageKey(remote) + ".creds"
}

func remoteStateFilename(remote string) string {
	return remoteStorageKey(remote) + ".state"
}

func sanitizedRemoteName(remote string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	name := normalizeRemote(remote)
	if strings.TrimSpace(name) == "" {
		name = remoteDefault
	}
	return replacer.Replace(name)
}

func remoteStorageKey(remote string) string {
	normalized := normalizedRemoteName(remote)
	base := strings.Trim(sanitizedRemoteName(normalized), "._-")
	if base == "" {
		base = "remote"
	}
	digest := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%s-%s", base, hex.EncodeToString(digest[:8]))
}

func credentialDirPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "protondrive"), nil
}

func ensureCredentialDir() (string, error) {
	dir, err := credentialDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func credentialFilePath(remote string) (string, error) {
	dir, err := credentialDirPath()
	if err != nil {
		return "", err
	}
	current := filepath.Join(dir, credentialFilename(remote))
	if _, err := os.Stat(current); err == nil || !errors.Is(err, os.ErrNotExist) {
		return current, nil
	}
	legacy := filepath.Join(dir, sanitizedRemoteName(remote)+".creds")
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	return current, nil
}
