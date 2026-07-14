package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ColinMario/Protondrive-for-Linux/internal/safefile"
	"golang.org/x/crypto/scrypt"
)

type remoteState struct {
	Remote          string       `json:"remote"`
	LastAuthSuccess time.Time    `json:"last_auth_success"`
	LastAuthMethod  string       `json:"last_auth_method"`
	LastAuthAttempt time.Time    `json:"last_auth_attempt"`
	LastAuthError   string       `json:"last_auth_error"`
	VaultConfigured bool         `json:"vault_configured"`
	VaultUpdated    time.Time    `json:"vault_updated"`
	Mounts          []mountState `json:"mounts"`
}

type mountState struct {
	MountPoint        string    `json:"mount_point"`
	RemotePath        string    `json:"remote_path"`
	Method            string    `json:"method,omitempty"`
	ProcessID         int       `json:"process_id,omitempty"`
	ProcessExecutable string    `json:"process_executable,omitempty"`
	ProcessStartToken string    `json:"process_start_token,omitempty"`
	URL               string    `json:"url,omitempty"`
	AuthFile          string    `json:"auth_file,omitempty"`
	Attached          bool      `json:"attached"`
	LastUpdated       time.Time `json:"last_updated"`
}

func remoteStateFilePath(remote string) (string, error) {
	dir, err := credentialDirPath()
	if err != nil {
		return "", err
	}
	current := filepath.Join(dir, remoteStateFilename(remote))
	if _, err := os.Stat(current); err == nil || !errors.Is(err, os.ErrNotExist) {
		return current, nil
	}
	legacy := filepath.Join(dir, sanitizedRemoteName(remote)+".state")
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	return current, nil
}

func loadRemoteState(remote string) (remoteState, error) {
	path, err := remoteStateFilePath(remote)
	if err != nil {
		return remoteState{}, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- state path is generated under the app config directory
	if errors.Is(err, os.ErrNotExist) {
		return remoteState{Remote: normalizedRemoteName(remote)}, nil
	}
	if err != nil {
		return remoteState{}, err
	}
	var state remoteState
	if err := json.Unmarshal(data, &state); err != nil {
		return remoteState{}, err
	}
	if strings.TrimSpace(state.Remote) == "" {
		state.Remote = normalizedRemoteName(remote)
	}
	return state, nil
}

func updateRemoteState(remote string, mutator func(*remoteState)) error {
	dir, err := ensureCredentialDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, remoteStateFilename(remote))
	legacyState, legacyErr := loadRemoteState(remote)
	if legacyErr != nil {
		return legacyErr
	}
	if err := safefile.Update(path, 0o600, func(current []byte) ([]byte, fs.FileMode, error) {
		state := legacyState
		if len(bytes.TrimSpace(current)) > 0 {
			if err := json.Unmarshal(current, &state); err != nil {
				return nil, 0, err
			}
		}
		state.Remote = normalizedRemoteName(remote)
		mutator(&state)
		payload, err := json.MarshalIndent(state, "", "  ")
		return payload, 0o600, err
	}); err != nil {
		return err
	}
	legacyPath := filepath.Join(dir, sanitizedRemoteName(remote)+".state")
	if legacyPath != path {
		if err := safefile.Remove(legacyPath); err != nil {
			return fmt.Errorf("new state was saved but legacy state cleanup failed: %w", err)
		}
	}
	return nil
}

func normalizedRemoteName(remote string) string {
	name := normalizeRemote(remote)
	if strings.TrimSpace(name) == "" {
		name = remoteDefault
	}
	return name
}

func recordAuthEvent(remote, method string, success bool, message string) {
	err := updateRemoteState(remote, func(state *remoteState) {
		now := time.Now()
		state.LastAuthAttempt = now
		if success {
			state.LastAuthSuccess = now
			state.LastAuthMethod = method
			state.LastAuthError = ""
		} else {
			state.LastAuthError = message
		}
	})
	if err != nil {
		logStateWarning(err)
	}
}

func recordMountAttach(remote, mountPoint, remotePath, method string, processID int, url, authFile string) error {
	abs := filepath.Clean(mountPoint)
	now := time.Now()
	processExecutable := ""
	processStart := ""
	if processID > 0 {
		processExecutable = filepath.Base(currentOptions.RcloneBin)
		var err error
		processStart, err = processStartToken(processID)
		if err != nil {
			return fmt.Errorf("unable to record mount process identity: %w", err)
		}
	}
	err := updateRemoteState(remote, func(state *remoteState) {
		for i := range state.Mounts {
			if sameMountPoint(state.Mounts[i].MountPoint, abs) {
				state.Mounts[i].MountPoint = abs
				state.Mounts[i].RemotePath = remotePath
				state.Mounts[i].Method = method
				state.Mounts[i].ProcessID = processID
				state.Mounts[i].ProcessExecutable = processExecutable
				state.Mounts[i].ProcessStartToken = processStart
				state.Mounts[i].URL = url
				state.Mounts[i].AuthFile = authFile
				state.Mounts[i].Attached = true
				state.Mounts[i].LastUpdated = now
				return
			}
		}
		state.Mounts = append(state.Mounts, mountState{
			MountPoint:        abs,
			RemotePath:        remotePath,
			Method:            method,
			ProcessID:         processID,
			ProcessExecutable: processExecutable,
			ProcessStartToken: processStart,
			URL:               url,
			AuthFile:          authFile,
			Attached:          true,
			LastUpdated:       now,
		})
	})
	return err
}

func recordVaultUpdate(remote string, timestamp time.Time) {
	err := updateRemoteState(remote, func(state *remoteState) {
		state.VaultConfigured = true
		state.VaultUpdated = timestamp
	})
	if err != nil {
		logStateWarning(err)
	}
}

func recordMountDetach(remote, mountPoint string) {
	abs := filepath.Clean(mountPoint)
	now := time.Now()
	err := updateRemoteState(remote, func(state *remoteState) {
		for i := range state.Mounts {
			if sameMountPoint(state.Mounts[i].MountPoint, abs) {
				state.Mounts[i].Attached = false
				state.Mounts[i].LastUpdated = now
				return
			}
		}
	})
	if err != nil {
		logStateWarning(err)
	}
}

func sameMountPoint(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func logStateWarning(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "Warning: unable to update ProtonDrive metadata: %v\n", err)
}

func isPathMounted(mountPoint string) (bool, error) {
	target := canonicalMountPath(mountPoint)
	switch runtime.GOOS {
	case "linux", "android":
		return checkProcMounts(target)
	case "darwin":
		return checkDarwinMounts(target)
	default:
		return false, fmt.Errorf("mount detection is not implemented on %s", runtime.GOOS)
	}
}

func canonicalMountPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	candidate := filepath.Clean(abs)
	var missing []string
	for {
		if resolved, resolveErr := filepath.EvalSymlinks(candidate); resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return filepath.Clean(abs)
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}

func checkProcMounts(target string) (bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		mountPath := canonicalMountPath(decodeProcMountField(fields[1]))
		if mountPath == target {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func checkDarwinMounts(target string) (bool, error) {
	out, err := safeExecCommand("mount").Output()
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, " on ", 2)
		if len(parts) != 2 {
			continue
		}
		right := parts[1]
		idx := strings.Index(right, " (")
		if idx == -1 {
			continue
		}
		mountPath := canonicalMountPath(strings.TrimSpace(right[:idx]))
		if mountPath == target {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func decodeProcMountField(field string) string {
	return procMountReplacer.Replace(field)
}

func encryptCredentials(passphrase string, creds storedCredentials) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := scrypt.Key([]byte(passphrase), salt, 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	raw, err := json.Marshal(creds) // #nosec G117 -- plaintext is marshaled only to be immediately encrypted with AES-GCM
	if err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, raw, nil)
	payload := encryptedCredentialBlob{
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}
	return json.Marshal(payload)
}

func decryptCredentials(passphrase string, payload []byte) (storedCredentials, error) {
	var blob encryptedCredentialBlob
	if err := json.Unmarshal(payload, &blob); err != nil {
		return storedCredentials{}, err
	}
	salt, err := base64.StdEncoding.DecodeString(blob.Salt)
	if err != nil {
		return storedCredentials{}, err
	}
	nonce, err := base64.StdEncoding.DecodeString(blob.Nonce)
	if err != nil {
		return storedCredentials{}, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(blob.Ciphertext)
	if err != nil {
		return storedCredentials{}, err
	}
	key, err := scrypt.Key([]byte(passphrase), salt, 1<<15, 8, 1, 32)
	if err != nil {
		return storedCredentials{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return storedCredentials{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return storedCredentials{}, err
	}
	raw, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return storedCredentials{}, errors.New("invalid passphrase or corrupted credential file")
	}
	var creds storedCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return storedCredentials{}, err
	}
	return creds, nil
}
