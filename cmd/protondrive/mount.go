package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ColinMario/Protondrive-for-Linux/internal/safefile"
	"golang.org/x/crypto/bcrypt"
)

func runMount(remote string, args []string) error {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	remotePathFlag := fs.String("remote-path", "", "Remote folder to mount (defaults to root)")
	cacheMode := fs.String("cache-mode", "full", "Value for --vfs-cache-mode")
	cacheMaxAge := fs.String("vfs-cache-max-age", "", "Value passed to --vfs-cache-max-age")
	bufferSize := fs.String("buffer-size", "", "Value passed to --buffer-size")
	readOnly := fs.Bool("read-only", false, "Mount in read-only mode")
	allowOther := fs.Bool("allow-other", false, "Add --allow-other (requires FUSE permissions)")
	allowRoot := fs.Bool("allow-root", false, "Add --allow-root")
	foreground := fs.Bool("foreground", false, "Run rclone mount in the foreground (Ctrl+C to stop)")
	mountMethodFlag := fs.String("mount-method", mountMethodAuto, "Mount method: auto, fuse, or webdav (macOS auto uses webdav to avoid macFUSE)")
	persist := fs.Bool("persist", false, "Install and start a persistent Linux user mount service")
	persistName := fs.String("persist-name", "", "Name suffix for the persistent user mount service")
	persistManager := fs.String("persist-manager", persistentMountManagerAuto, "Persistent service manager: auto, systemd, or openrc")
	enableLinger := fs.Bool("enable-linger", false, "With --persist, attempt to enable systemd user lingering for boot-time mounts")
	readyTimeout := fs.Duration("ready-timeout", 30*time.Second, "Max wait for mounts to become ready when backgrounding")
	var customFlags repeatableFlag
	fs.Var(&customFlags, "rclone-flag", "Additional flag passed through to rclone mount (repeatable)")
	parseArgs := normalizeInterspersedFlags(args, map[string]bool{
		"remote-path":       true,
		"cache-mode":        true,
		"vfs-cache-max-age": true,
		"buffer-size":       true,
		"mount-method":      true,
		"persist-name":      true,
		"persist-manager":   true,
		"ready-timeout":     true,
		"rclone-flag":       true,
		"persist":           false,
		"enable-linger":     false,
		"read-only":         false,
		"allow-other":       false,
		"allow-root":        false,
		"foreground":        false,
	})
	if err := parseCommandFlags(fs, parseArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if _, err := resolveBackend("mount", args); err != nil {
		return err
	}
	if err := ensureRemoteAuth(remote); err != nil {
		return err
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return errors.New("mount requires a mount point argument")
	}
	mountPoint, err := filepath.Abs(expandPath(remaining[0]))
	if err != nil {
		return fmt.Errorf("unable to resolve mount point: %w", err)
	}
	extra := remaining[1:]

	if err := os.MkdirAll(mountPoint, 0o755); err != nil { // #nosec G301 -- mount points are user-facing directories
		return fmt.Errorf("unable to create mount point '%s': %w", mountPoint, err)
	}

	mountMethod, err := normalizeMountMethod(*mountMethodFlag)
	if err != nil {
		return err
	}
	mountMethod = chooseMountMethod(mountMethod, *foreground)
	allRcloneFlags := append(append([]string{}, customFlags...), extra...)
	if err := rejectMountControlOverrides(allRcloneFlags); err != nil {
		return err
	}
	if *persist {
		if *foreground {
			return errors.New("--persist manages the mount as a foreground service; do not combine it with --foreground")
		}
		if mountMethod != mountMethodFuse {
			return fmt.Errorf("--persist currently supports Linux FUSE mounts only; got mount method %s", mountMethod)
		}
		if len(extra) > 0 {
			return errors.New("--persist does not support positional rclone mount extras; use --rclone-flag for repeatable rclone flags")
		}
		options := persistentMountOptions{
			Remote:       remote,
			MountPoint:   mountPoint,
			RemotePath:   *remotePathFlag,
			CacheMode:    *cacheMode,
			CacheMaxAge:  *cacheMaxAge,
			BufferSize:   *bufferSize,
			ReadOnly:     *readOnly,
			AllowOther:   *allowOther,
			AllowRoot:    *allowRoot,
			ReadyTimeout: *readyTimeout,
			RcloneFlags:  append([]string(nil), customFlags...),
			PersistName:  *persistName,
			Manager:      *persistManager,
			EnableLinger: *enableLinger,
			RcloneBin:    currentOptions.RcloneBin,
		}
		if err := installPersistentMount(options); err != nil {
			return err
		}
		if err := recordMountAttach(remote, mountPoint, remotePath(remote, *remotePathFlag), mountMethodFuse, 0, "", ""); err != nil {
			cleanupErr := removePersistentMount(remote, mountPoint, *persistName, *persistManager)
			return errors.Join(fmt.Errorf("persistent mount was started but its state could not be recorded: %w", err), cleanupErr)
		}
		return nil
	}
	if mountMethod == mountMethodWebDAV {
		if *foreground {
			return errors.New("--foreground is not supported with --mount-method webdav; use --mount-method fuse for foreground rclone logs")
		}
		return runWebDAVMount(remote, mountPoint, *remotePathFlag, *cacheMode, *cacheMaxAge, *bufferSize, *readOnly, *allowOther, *allowRoot, *readyTimeout, customFlags, extra)
	}

	cmd := []string{
		"mount",
		remotePath(remote, *remotePathFlag),
		mountPoint,
		"--vfs-cache-mode", *cacheMode,
	}
	if strings.TrimSpace(*cacheMaxAge) != "" {
		cmd = append(cmd, "--vfs-cache-max-age", strings.TrimSpace(*cacheMaxAge))
	}
	if strings.TrimSpace(*bufferSize) != "" {
		cmd = append(cmd, "--buffer-size", strings.TrimSpace(*bufferSize))
	}
	if *readOnly {
		cmd = append(cmd, "--read-only")
	}
	if *allowOther {
		cmd = append(cmd, "--allow-other")
	}
	if *allowRoot {
		cmd = append(cmd, "--allow-root")
	}
	if !*foreground {
		cmd = append(cmd, "--daemon", fmt.Sprintf("--daemon-timeout=%s", (*readyTimeout).String()))
	}
	if len(customFlags) > 0 {
		cmd = append(cmd, customFlags...)
	}
	cmd = append(cmd, extra...)

	target := remotePath(remote, *remotePathFlag)
	if *foreground {
		fmt.Printf("Mounting %s at %s. Press Ctrl+C to stop.\n", target, mountPoint)
		if err := streamRclone(cmd...); err != nil {
			return mountErrorWithHints(target, mountPoint, *readyTimeout, err, false)
		}
		return nil
	}

	fmt.Printf("Mounting %s at %s. This returns once the mount is ready.\n", target, mountPoint)
	if err := streamRclone(cmd...); err != nil {
		return mountErrorWithHints(target, mountPoint, *readyTimeout, err, true)
	}
	if err := recordMountAttach(remote, mountPoint, target, mountMethodFuse, 0, "", ""); err != nil {
		cleanupErr := unmountPathForRollback(mountPoint)
		return errors.Join(fmt.Errorf("mount was started but its state could not be recorded: %w", err), cleanupErr)
	}
	fmt.Printf("Mount ready at %s. Use 'protondrive unmount %s' to detach.\n", mountPoint, mountPoint)
	return nil
}

func runWebDAVMount(remote, mountPoint, remotePathFlag, cacheMode, cacheMaxAge, bufferSize string, readOnly, allowOther, allowRoot bool, readyTimeout time.Duration, customFlags repeatableFlag, extra []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("--mount-method %s is currently implemented for macOS only; use --mount-method %s on %s", mountMethodWebDAV, mountMethodFuse, runtime.GOOS)
	}
	if allowOther || allowRoot {
		return fmt.Errorf("--allow-other and --allow-root require FUSE; use --mount-method %s", mountMethodFuse)
	}
	if !externalCommandAvailable("/sbin/mount_webdav") {
		return errors.New("mount_webdav not found; macOS WebDAV mounting is unavailable")
	}
	if !externalCommandAvailable("/usr/bin/expect") {
		return errors.New("expect not found; refusing to put WebDAV credentials in mount_webdav process arguments")
	}
	if readyTimeout <= 0 {
		readyTimeout = 30 * time.Second
	}

	addr, err := reserveLocalTCPAddr()
	if err != nil {
		return err
	}
	url := "http://" + addr + "/"
	target := remotePath(remote, remotePathFlag)
	webDAVUser := "protondrive"
	webDAVPassword, err := randomBase64(32)
	if err != nil {
		return fmt.Errorf("unable to create WebDAV credentials: %w", err)
	}
	if err := rejectWebDAVSecurityOverrides(append(append([]string{}, customFlags...), extra...)); err != nil {
		return err
	}

	cmdArgs := []string{
		"serve", "webdav", target,
		"--addr", addr,
		"--vfs-cache-mode", cacheMode,
	}
	if strings.TrimSpace(cacheMaxAge) != "" {
		cmdArgs = append(cmdArgs, "--vfs-cache-max-age", strings.TrimSpace(cacheMaxAge))
	}
	if strings.TrimSpace(bufferSize) != "" {
		cmdArgs = append(cmdArgs, "--buffer-size", strings.TrimSpace(bufferSize))
	}
	if readOnly {
		cmdArgs = append(cmdArgs, "--read-only")
	}
	cmdArgs = append(cmdArgs, customFlags...)
	cmdArgs = append(cmdArgs, extra...)

	logDir, err := ensureCredentialDir()
	if err != nil {
		return err
	}
	logDir = filepath.Join(logDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	authPath, err := writeWebDAVAuthFile(logDir, webDAVUser, webDAVPassword)
	if err != nil {
		return fmt.Errorf("unable to create WebDAV authentication file: %w", err)
	}
	removeAuthFile := func() {
		if removeErr := safefile.Remove(authPath); removeErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: unable to remove WebDAV authentication file %s: %v\n", authPath, removeErr)
		}
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("webdav-%s.log", remoteStorageKey(remote)))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- generated application log path
	if err != nil {
		removeAuthFile()
		return fmt.Errorf("unable to open WebDAV log: %w", err)
	}

	fmt.Printf("Serving %s over authenticated local WebDAV at %s for macOS mount. Log: %s\n", target, url, logPath)
	cmdArgs = append(cmdArgs, "--htpasswd", authPath)
	cmd := externalCommand(currentOptions.RcloneBin, cmdArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		removeAuthFile()
		return fmt.Errorf("failed to start rclone WebDAV server: %w", err)
	}
	_ = logFile.Close()
	serverPID := cmd.Process.Pid
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- cmd.Wait()
	}()

	cleanupServer := func() {
		_ = stopProcess(serverPID)
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
		}
		removeAuthFile()
	}

	if err := waitForWebDAV(url, webDAVUser, webDAVPassword, readyTimeout, serverDone); err != nil {
		cleanupServer()
		return err
	}

	if err := runInteractiveWebDAVMount(url, mountPoint, webDAVUser, webDAVPassword, readyTimeout); err != nil {
		cleanupServer()
		return mountErrorWithHints(target, mountPoint, readyTimeout, err, true)
	}
	if err := waitForMountReady(mountPoint, readyTimeout); err != nil {
		cleanupServer()
		return mountErrorWithHints(target, mountPoint, readyTimeout, err, true)
	}

	if err := recordMountAttach(remote, mountPoint, target, mountMethodWebDAV, serverPID, url, authPath); err != nil {
		unmountErr := unmountPathForRollback(mountPoint)
		cleanupServer()
		return errors.Join(fmt.Errorf("WebDAV mount was started but its process identity could not be recorded: %w", err), unmountErr)
	}
	fmt.Printf("Mount ready at %s via local WebDAV. Use 'protondrive unmount %s' to detach.\n", mountPoint, mountPoint)
	return nil
}

const macOSWebDAVExpectScript = `
set timeout $env(PROTONDRIVE_WEBDAV_EXPECT_TIMEOUT)
if {[gets stdin username] < 0 || [gets stdin password] < 0} {
  exit 2
}
log_user 0
spawn /sbin/mount_webdav -i $env(PROTONDRIVE_WEBDAV_URL) $env(PROTONDRIVE_WEBDAV_MOUNT_POINT)
expect {
  "Username:" {}
  timeout { exit 124 }
  eof { set result [wait]; exit [lindex $result 3] }
}
send -- "$username\r"
expect {
  "Password:" {}
  timeout { exit 124 }
  eof { set result [wait]; exit [lindex $result 3] }
}
send -- "$password\r"
expect {
  eof {}
  timeout { exit 124 }
}
set result [wait]
exit [lindex $result 3]
`

func webDAVMountCommand(url, mountPoint, username, password string, timeout time.Duration) *exec.Cmd {
	seconds := int((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 30
	}
	cmd := externalCommandWithEnvironment("/usr/bin/expect", map[string]string{
		"PROTONDRIVE_WEBDAV_URL":            url,
		"PROTONDRIVE_WEBDAV_MOUNT_POINT":    mountPoint,
		"PROTONDRIVE_WEBDAV_EXPECT_TIMEOUT": strconv.Itoa(seconds),
	}, "-c", macOSWebDAVExpectScript)
	cmd.Stdin = strings.NewReader(username + "\n" + password + "\n")
	return cmd
}

func runInteractiveWebDAVMount(url, mountPoint, username, password string, timeout time.Duration) error {
	cmd := webDAVMountCommand(url, mountPoint, username, password, timeout)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if password != "" {
		detail = strings.ReplaceAll(detail, password, "[REDACTED]")
	}
	if detail == "" {
		return fmt.Errorf("authenticated mount_webdav failed: %w", err)
	}
	return fmt.Errorf("authenticated mount_webdav failed: %s", detail)
}

func rejectWebDAVSecurityOverrides(args []string) error {
	blocked := map[string]bool{
		"addr": true, "auth-proxy": true, "baseurl": true, "cert": true,
		"client-ca": true, "htpasswd": true, "key": true, "pass": true,
		"realm": true, "salt": true, "user": true, "user-from-header": true,
	}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if before, _, found := strings.Cut(name, "="); found {
			name = before
		}
		if blocked[name] {
			return fmt.Errorf("rclone option --%s is managed internally for the authenticated localhost WebDAV mount", name)
		}
	}
	return nil
}

func rejectMountControlOverrides(args []string) error {
	blocked := map[string]bool{
		"daemon": true, "daemon-timeout": true, "config": true,
		"vfs-cache-mode": true, "vfs-cache-max-age": true, "buffer-size": true,
		"read-only": true, "allow-other": true, "allow-root": true,
		"rc": true, "rc-addr": true, "rc-no-auth": true, "rc-user": true,
		"rc-pass": true, "rc-web-gui": true, "rc-web-gui-no-open-browser": true,
	}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if before, _, found := strings.Cut(name, "="); found {
			name = before
		}
		if blocked[name] {
			return fmt.Errorf("rclone option --%s is controlled by protondrive; use the corresponding mount option instead", name)
		}
	}
	return nil
}

func writeWebDAVAuthFile(dir, username, password string) (string, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return "", errors.New("WebDAV username and password must not be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(username + "\x00" + password))
	path := filepath.Join(dir, fmt.Sprintf("webdav-auth-%x.htpasswd", key[:8]))
	if err := safefile.Write(path, []byte(username+":"+string(hash)+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func reserveLocalTCPAddr() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("unable to reserve localhost port for WebDAV mount: %w", err)
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}

func waitForWebDAV(url, username, password string, timeout time.Duration, serverDone <-chan error) error {
	deadline := time.Now().Add(timeout)
	client := http.Client{Timeout: 500 * time.Millisecond}
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-serverDone:
			if err == nil {
				err = errors.New("rclone WebDAV server exited before becoming ready")
			}
			return err
		default:
		}
		unauthenticated, requestErr := http.NewRequest(http.MethodGet, url, nil)
		if requestErr != nil {
			return requestErr
		}
		probe, probeErr := client.Do(unauthenticated)
		if probeErr == nil {
			_ = probe.Body.Close()
			if probe.StatusCode != http.StatusUnauthorized && probe.StatusCode != http.StatusForbidden {
				return fmt.Errorf("local WebDAV endpoint became ready without requiring authentication (HTTP %d)", probe.StatusCode)
			}
		}
		req, requestErr := http.NewRequest(http.MethodGet, url, nil)
		if requestErr != nil {
			return requestErr
		}
		req.SetBasicAuth(username, password)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
			lastErr = fmt.Errorf("authenticated WebDAV readiness probe returned HTTP %d", resp.StatusCode)
			continue
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("rclone WebDAV server did not become ready within %s: %w", timeout, lastErr)
	}
	return fmt.Errorf("rclone WebDAV server did not become ready within %s", timeout)
}

var mountedPathCheck = isPathMounted

func waitForMountReady(mountPoint string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		mounted, err := mountedPathCheck(mountPoint)
		if err == nil && mounted {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("mount did not become ready: %w", lastErr)
	}
	return fmt.Errorf("mount did not become ready within %s", timeout)
}

func waitForWebDAVClosed(rawURL string, timeout time.Duration) error {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", parsed.Host, 250*time.Millisecond)
		if dialErr != nil {
			return nil
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("local WebDAV endpoint %s is still accepting connections", parsed.Host)
}

func stopProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	if !processExists(pid) {
		return nil
	}
	if err := interruptProcess(pid); err != nil && processExists(pid) {
		return err
	}
	if waitForProcessExit(pid, 5*time.Second) {
		return nil
	}
	if err := killProcess(pid); err != nil && processExists(pid) {
		return err
	}
	if !waitForProcessExit(pid, 2*time.Second) {
		return fmt.Errorf("process %d did not exit after interrupt and kill", pid)
	}
	return nil
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processExists(pid)
}

type persistentMountOptions struct {
	Remote       string
	MountPoint   string
	RemotePath   string
	CacheMode    string
	CacheMaxAge  string
	BufferSize   string
	ReadOnly     bool
	AllowOther   bool
	AllowRoot    bool
	ReadyTimeout time.Duration
	RcloneFlags  []string
	PersistName  string
	Manager      string
	EnableLinger bool
	RcloneBin    string
}

type mountArtifactSnapshot struct {
	Path       string
	Exists     bool
	Mode       os.FileMode
	Data       []byte
	LinkTarget string
}

func captureMountArtifacts(paths []string) ([]mountArtifactSnapshot, error) {
	snapshots := make([]mountArtifactSnapshot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, mountArtifactSnapshot{Path: path})
			continue
		}
		if err != nil {
			return nil, err
		}
		snapshot := mountArtifactSnapshot{Path: path, Exists: true, Mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			snapshot.LinkTarget, err = os.Readlink(path)
		case info.Mode().IsRegular():
			snapshot.Data, err = os.ReadFile(path) // #nosec G304 -- path is an internally generated service artifact
		default:
			err = fmt.Errorf("refusing to replace non-file service artifact %s", path)
		}
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func restoreMountArtifacts(snapshots []mountArtifactSnapshot) error {
	var errs []error
	for _, snapshot := range snapshots {
		if !snapshot.Exists {
			errs = append(errs, removeFileIfExists(snapshot.Path))
			continue
		}
		if snapshot.Mode&os.ModeSymlink != 0 {
			if err := removeFileIfExists(snapshot.Path); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := os.Symlink(snapshot.LinkTarget, snapshot.Path); err != nil {
				errs = append(errs, fmt.Errorf("restore symlink %s: %w", snapshot.Path, err))
			}
			continue
		}
		if err := safefile.Write(snapshot.Path, snapshot.Data, snapshot.Mode.Perm()); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", snapshot.Path, err))
		}
	}
	return errors.Join(errs...)
}

func installPersistentMount(options persistentMountOptions) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("--persist currently installs Linux user services only (current OS: %s)", runtime.GOOS)
	}
	manager, err := resolvePersistentMountManager(options.Manager)
	if err != nil {
		return err
	}
	var installErr error
	switch manager {
	case persistentMountManagerSystemd:
		installErr = installSystemdPersistentMount(options)
	case persistentMountManagerOpenRC:
		installErr = installOpenRCPersistentMount(options)
	default:
		return fmt.Errorf("unsupported persistent service manager %q", manager)
	}
	if installErr != nil {
		return installErr
	}
	if err := waitForMountReady(options.MountPoint, options.ReadyTimeout); err != nil {
		cleanupErr := removePersistentMount(options.Remote, options.MountPoint, options.PersistName, manager)
		return errors.Join(fmt.Errorf("persistent service started but mount never became ready: %w", err), cleanupErr)
	}
	fmt.Printf("Persistent mount service is active and mount is ready: %s\n", persistentMountBaseName(options.Remote, options.MountPoint, options.PersistName))
	return nil
}

func installSystemdPersistentMount(options persistentMountOptions) error {
	systemctlPath, err := exec.LookPath("systemctl")
	if err != nil {
		return errors.New("systemctl not found; persistent mounts require systemd --user")
	}

	serviceName := persistentMountServiceName(options.Remote, options.MountPoint, options.PersistName)
	unitDir, scriptDir, err := systemdPersistentMountDirs()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil { // #nosec G301 -- systemd user unit directory follows systemd conventions
		return err
	}
	if err := os.MkdirAll(scriptDir, 0o700); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	rcloneBin := options.RcloneBin
	if found, err := exec.LookPath(rcloneBin); err == nil {
		rcloneBin = found
	}

	baseName := strings.TrimSuffix(serviceName, ".service")
	startScript := filepath.Join(scriptDir, baseName+".sh")
	stopScript := filepath.Join(scriptDir, baseName+"-stop.sh")
	unitPath := filepath.Join(unitDir, serviceName)
	artifacts, err := captureMountArtifacts([]string{unitPath, startScript, stopScript})
	if err != nil {
		return err
	}
	wasActive := safeExecCommand(systemctlPath, "--user", "is-active", "--quiet", serviceName).Run() == nil   // #nosec G204 -- resolved systemctl command
	wasEnabled := safeExecCommand(systemctlPath, "--user", "is-enabled", "--quiet", serviceName).Run() == nil // #nosec G204 -- resolved systemctl command
	rollback := func(cause error) error {
		var rollbackErrs []error
		if err := runCommandIdempotent(systemctlPath, "--user", "disable", "--now", serviceName); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		rollbackErrs = append(rollbackErrs, restoreMountArtifacts(artifacts))
		if err := runCommand(systemctlPath, "--user", "daemon-reload"); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		if wasEnabled {
			if err := runCommand(systemctlPath, "--user", "enable", serviceName); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		if wasActive {
			if err := runCommand(systemctlPath, "--user", "start", serviceName); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		return errors.Join(cause, errors.Join(rollbackErrs...))
	}

	startArgs := persistentMountStartArgs(exe, rcloneBin, options)
	if err := safefile.Write(startScript, []byte(shellScript(startArgs)), 0o700); err != nil {
		return rollback(err)
	}
	if err := safefile.Write(stopScript, []byte(unmountShellScript(options.MountPoint)), 0o700); err != nil {
		return rollback(err)
	}
	if err := safefile.Write(unitPath, []byte(systemdMountUnit(serviceName, startScript, stopScript, options.MountPoint)), 0o644); err != nil {
		return rollback(err)
	}

	if err := runCommand(systemctlPath, "--user", "daemon-reload"); err != nil {
		return rollback(err)
	}
	if err := runCommand(systemctlPath, "--user", "enable", serviceName); err != nil {
		return rollback(err)
	}
	if err := runCommand(systemctlPath, "--user", "restart", serviceName); err != nil {
		return rollback(err)
	}
	if err := safeExecCommand(systemctlPath, "--user", "is-active", "--quiet", serviceName).Run(); err != nil { // #nosec G204 -- resolved systemctl command
		return rollback(fmt.Errorf("systemd service %s did not become active: %w", serviceName, err))
	}
	if options.EnableLinger {
		if err := enableUserLinger(); err != nil {
			return rollback(err)
		}
	}

	fmt.Printf("Persistent mount service installed and started; checking mount readiness: %s\n", serviceName)
	fmt.Printf("Unit: %s\n", unitPath)
	fmt.Printf("Mount point: %s\n", options.MountPoint)
	if !options.EnableLinger {
		fmt.Println("For boot-time mounts before login, run again with --enable-linger or enable lingering with loginctl.")
	}
	return nil
}

func installOpenRCPersistentMount(options persistentMountOptions) error {
	if options.EnableLinger {
		return errors.New("--enable-linger is only supported with systemd user services; OpenRC user services depend on the user's OpenRC session setup")
	}
	rcService, err := findOpenRCBinary("rc-service")
	if err != nil {
		return err
	}
	rcUpdate, err := findOpenRCBinary("rc-update")
	if err != nil {
		return err
	}
	openRCRun, err := findOpenRCBinary("openrc-run")
	if err != nil {
		return err
	}
	if strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")) == "" {
		return errors.New("openrc user services require XDG_RUNTIME_DIR; run from an OpenRC user session before using --persist-manager openrc")
	}

	serviceName := persistentMountOpenRCServiceName(options.Remote, options.MountPoint, options.PersistName)
	initDir, runlevelDir, scriptDir, err := openRCPersistentMountDirs()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(initDir, 0o755); err != nil { // #nosec G301 -- OpenRC user init directory must be traversable by OpenRC
		return err
	}
	if err := os.MkdirAll(runlevelDir, 0o755); err != nil { // #nosec G301 -- OpenRC user runlevel directory must be traversable by OpenRC
		return err
	}
	if err := os.MkdirAll(scriptDir, 0o700); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	rcloneBin := options.RcloneBin
	if found, err := exec.LookPath(rcloneBin); err == nil {
		rcloneBin = found
	}

	startScript := filepath.Join(scriptDir, serviceName+".sh")
	stopScript := filepath.Join(scriptDir, serviceName+"-stop.sh")
	servicePath := filepath.Join(initDir, serviceName)
	runlevelPath := filepath.Join(runlevelDir, serviceName)
	artifacts, err := captureMountArtifacts([]string{servicePath, startScript, stopScript})
	if err != nil {
		return err
	}
	wasActive := safeExecCommand(rcService, "--user", serviceName, "status").Run() == nil // #nosec G204 -- resolved OpenRC command
	_, enabledErr := os.Lstat(runlevelPath)
	wasEnabled := enabledErr == nil
	if enabledErr != nil && !errors.Is(enabledErr, os.ErrNotExist) {
		return enabledErr
	}
	rollback := func(cause error) error {
		var rollbackErrs []error
		if err := runCommandIdempotent(rcService, "--user", serviceName, "stop"); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		if err := runCommandIdempotent(rcUpdate, "--user", "del", serviceName, "default"); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		rollbackErrs = append(rollbackErrs, restoreMountArtifacts(artifacts))
		if wasEnabled {
			if err := runCommand(rcUpdate, "--user", "add", serviceName, "default"); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		if wasActive {
			if err := runCommand(rcService, "--user", serviceName, "start"); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		return errors.Join(cause, errors.Join(rollbackErrs...))
	}

	startArgs := persistentMountStartArgs(exe, rcloneBin, options)
	if err := safefile.Write(startScript, []byte(shellScript(startArgs)), 0o700); err != nil {
		return rollback(err)
	}
	if err := safefile.Write(stopScript, []byte(unmountShellScript(options.MountPoint)), 0o700); err != nil {
		return rollback(err)
	}
	if err := safefile.Write(servicePath, []byte(openRCMountService(openRCRun, serviceName, startScript, stopScript)), 0o755); err != nil {
		return rollback(err)
	}

	if err := runCommand(rcUpdate, "--user", "add", serviceName, "default"); err != nil {
		return rollback(err)
	}
	startAction := "start"
	if wasActive {
		startAction = "restart"
	}
	if err := runCommand(rcService, "--user", serviceName, startAction); err != nil {
		return rollback(err)
	}

	fmt.Printf("Persistent OpenRC user service installed and started; checking mount readiness: %s\n", serviceName)
	fmt.Printf("Service: %s\n", servicePath)
	fmt.Printf("Mount point: %s\n", options.MountPoint)
	fmt.Println("OpenRC user services start with the user's OpenRC session; configure your distribution's OpenRC user-session support for boot-time startup.")
	return nil
}

func removePersistentMount(remote, mountPoint, persistName, managerName string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("--remove-persist currently manages Linux user services only (current OS: %s)", runtime.GOOS)
	}
	manager, err := normalizePersistentMountManager(managerName)
	if err != nil {
		return err
	}
	if manager == persistentMountManagerAuto {
		if err := errors.Join(
			removeSystemdPersistentMount(remote, mountPoint, persistName),
			removeOpenRCPersistentMount(remote, mountPoint, persistName),
		); err != nil {
			return err
		}
		fmt.Printf("Persistent mount service removed: %s\n", persistentMountBaseName(remote, mountPoint, persistName))
		return nil
	}
	switch manager {
	case persistentMountManagerSystemd:
		err = removeSystemdPersistentMount(remote, mountPoint, persistName)
	case persistentMountManagerOpenRC:
		err = removeOpenRCPersistentMount(remote, mountPoint, persistName)
	default:
		return fmt.Errorf("unsupported persistent service manager %q", manager)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Persistent mount service removed: %s\n", persistentMountBaseName(remote, mountPoint, persistName))
	return nil
}

func removeSystemdPersistentMount(remote, mountPoint, persistName string) error {
	serviceName := persistentMountServiceName(remote, mountPoint, persistName)
	unitDir, scriptDir, err := systemdPersistentMountDirs()
	if err != nil {
		return err
	}
	baseName := strings.TrimSuffix(serviceName, ".service")
	paths := []string{
		filepath.Join(unitDir, serviceName),
		filepath.Join(scriptDir, baseName+".sh"),
		filepath.Join(scriptDir, baseName+"-stop.sh"),
	}
	hadArtifacts := anyPathExists(paths)
	var errs []error
	systemctlPath, systemctlErr := exec.LookPath("systemctl")
	activeOrEnabled := false
	if systemctlErr == nil {
		activeOrEnabled = safeExecCommand(systemctlPath, "--user", "is-active", "--quiet", serviceName).Run() == nil || // #nosec G204 -- resolved systemctl command
			safeExecCommand(systemctlPath, "--user", "is-enabled", "--quiet", serviceName).Run() == nil // #nosec G204 -- resolved systemctl command
	}
	if hadArtifacts || activeOrEnabled {
		if systemctlErr != nil {
			errs = append(errs, fmt.Errorf("cannot disable %s: systemctl not found", serviceName))
		} else if commandErr := runCommand(systemctlPath, "--user", "disable", "--now", serviceName); commandErr != nil {
			errs = append(errs, commandErr)
		}
	}
	for _, target := range paths {
		if removeErr := removeFileIfExists(target); removeErr != nil {
			errs = append(errs, removeErr)
		}
	}
	if hadArtifacts || activeOrEnabled {
		if systemctlErr == nil {
			if reloadErr := runCommand(systemctlPath, "--user", "daemon-reload"); reloadErr != nil {
				errs = append(errs, reloadErr)
			}
			if activeErr := safeExecCommand(systemctlPath, "--user", "is-active", "--quiet", serviceName).Run(); activeErr == nil { // #nosec G204 -- resolved systemctl invocation
				errs = append(errs, fmt.Errorf("systemd service %s is still active after removal", serviceName))
			}
			if enabledErr := safeExecCommand(systemctlPath, "--user", "is-enabled", "--quiet", serviceName).Run(); enabledErr == nil { // #nosec G204 -- resolved systemctl invocation
				errs = append(errs, fmt.Errorf("systemd service %s is still enabled after removal", serviceName))
			}
		}
	}
	return errors.Join(errs...)
}

func removeOpenRCPersistentMount(remote, mountPoint, persistName string) error {
	serviceName := persistentMountOpenRCServiceName(remote, mountPoint, persistName)
	initDir, runlevelDir, scriptDir, err := openRCPersistentMountDirs()
	if err != nil {
		return err
	}
	paths := []string{
		filepath.Join(initDir, serviceName),
		filepath.Join(runlevelDir, serviceName),
		filepath.Join(scriptDir, serviceName+".sh"),
		filepath.Join(scriptDir, serviceName+"-stop.sh"),
	}
	hadArtifacts := anyPathExists(paths)
	var errs []error
	if hadArtifacts {
		if rcService, findErr := findOpenRCBinary("rc-service"); findErr == nil {
			if stopErr := runCommandIdempotent(rcService, "--user", serviceName, "stop"); stopErr != nil {
				errs = append(errs, stopErr)
			}
		} else {
			errs = append(errs, findErr)
		}
		if rcUpdate, findErr := findOpenRCBinary("rc-update"); findErr == nil {
			if updateErr := runCommandIdempotent(rcUpdate, "--user", "del", serviceName, "default"); updateErr != nil {
				errs = append(errs, updateErr)
			}
		} else {
			errs = append(errs, findErr)
		}
	}
	for _, target := range paths {
		if removeErr := removeFileIfExists(target); removeErr != nil {
			errs = append(errs, removeErr)
		}
	}
	return errors.Join(errs...)
}

func anyPathExists(paths []string) bool {
	for _, target := range paths {
		if _, err := os.Lstat(target); err == nil {
			return true
		}
	}
	return false
}

func removeFileIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove %s: %w", path, err)
}

func runCommandIdempotent(name string, args ...string) error {
	cmd := safeExecCommand(name, args...) // #nosec G204 -- caller resolves fixed service-manager commands
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	lower := strings.ToLower(string(output))
	for _, marker := range []string{
		"not running", "already stopped", "does not exist", "not found",
		"not in runlevel", "not loaded", "not enabled", "unit file does not exist",
	} {
		if strings.Contains(lower, marker) {
			return nil
		}
	}
	diagnostic := strings.TrimSpace(string(output))
	if diagnostic == "" {
		return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, diagnostic)
}

func persistentMountStartArgs(exe, rcloneBin string, options persistentMountOptions) []string {
	args := []string{
		exe,
		"--backend", backendRclone,
		"--remote", normalizedRemoteName(options.Remote),
		"--rclone-bin", rcloneBin,
		"mount",
		options.MountPoint,
		"--foreground",
		"--mount-method", mountMethodFuse,
		"--cache-mode", options.CacheMode,
	}
	if strings.TrimSpace(options.RemotePath) != "" {
		args = append(args, "--remote-path", strings.TrimSpace(options.RemotePath))
	}
	if strings.TrimSpace(options.CacheMaxAge) != "" {
		args = append(args, "--vfs-cache-max-age", strings.TrimSpace(options.CacheMaxAge))
	}
	if strings.TrimSpace(options.BufferSize) != "" {
		args = append(args, "--buffer-size", strings.TrimSpace(options.BufferSize))
	}
	if options.ReadOnly {
		args = append(args, "--read-only")
	}
	if options.AllowOther {
		args = append(args, "--allow-other")
	}
	if options.AllowRoot {
		args = append(args, "--allow-root")
	}
	for _, flag := range options.RcloneFlags {
		args = append(args, "--rclone-flag", flag)
	}
	return args
}

func persistentMountServiceName(remote, mountPoint, persistName string) string {
	return persistentMountBaseName(remote, mountPoint, persistName) + ".service"
}

func persistentMountOpenRCServiceName(remote, mountPoint, persistName string) string {
	return persistentMountBaseName(remote, mountPoint, persistName)
}

func persistentMountBaseName(remote, mountPoint, persistName string) string {
	name := strings.TrimSpace(persistName)
	if name == "" {
		name = normalizedRemoteName(remote) + "-" + filepath.Base(filepath.Clean(mountPoint))
	}
	return "protondrive-mount-" + slugifyConfigName(name)
}

func systemdPersistentMountDirs() (unitDir string, scriptDir string, err error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	appDir, err := credentialDirPath()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(configDir, "systemd", "user"), filepath.Join(appDir, "systemd"), nil
}

func openRCPersistentMountDirs() (initDir string, runlevelDir string, scriptDir string, err error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", "", "", err
	}
	appDir, err := credentialDirPath()
	if err != nil {
		return "", "", "", err
	}
	return filepath.Join(configDir, "rc", "init.d"),
		filepath.Join(configDir, "rc", "runlevels", "default"),
		filepath.Join(appDir, "openrc"),
		nil
}

func normalizePersistentMountManager(value string) (string, error) {
	manager := strings.ToLower(strings.TrimSpace(value))
	if manager == "" {
		manager = persistentMountManagerAuto
	}
	switch manager {
	case persistentMountManagerAuto, persistentMountManagerSystemd, persistentMountManagerOpenRC:
		return manager, nil
	default:
		return "", errors.New("--persist-manager must be one of auto, systemd, or openrc")
	}
}

func resolvePersistentMountManager(value string) (string, error) {
	manager, err := normalizePersistentMountManager(value)
	if err != nil {
		return "", err
	}
	if manager != persistentMountManagerAuto {
		return manager, nil
	}
	if isOpenRCRuntime() && openRCServiceManagerAvailable() {
		return persistentMountManagerOpenRC, nil
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		return persistentMountManagerSystemd, nil
	}
	if openRCServiceManagerAvailable() {
		return persistentMountManagerOpenRC, nil
	}
	return "", errors.New("no supported persistent service manager found; install systemd user services or OpenRC user services, or pass --persist-manager systemd/openrc")
}

func isOpenRCRuntime() bool {
	if _, err := os.Stat("/run/openrc"); err == nil {
		return true
	}
	return false
}

func openRCServiceManagerAvailable() bool {
	for _, name := range []string{"rc-service", "rc-update", "openrc-run"} {
		if _, err := findOpenRCBinary(name); err != nil {
			return false
		}
	}
	return true
}

func findOpenRCBinary(name string) (string, error) {
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	for _, dir := range []string{"/sbin", "/usr/sbin", "/bin", "/usr/bin"} {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found; persistent OpenRC mounts require OpenRC user service tools", name)
}

func shellScript(args []string) string {
	var builder strings.Builder
	builder.WriteString("#!/bin/sh\nset -eu\n")
	if rcloneConfig := strings.TrimSpace(os.Getenv("RCLONE_CONFIG")); rcloneConfig != "" {
		if absolute, err := filepath.Abs(expandPath(rcloneConfig)); err == nil {
			rcloneConfig = absolute
		}
		builder.WriteString("export RCLONE_CONFIG=")
		builder.WriteString(shellQuote(rcloneConfig))
		builder.WriteString("\n")
	}
	builder.WriteString("exec")
	for _, arg := range args {
		builder.WriteByte(' ')
		builder.WriteString(shellQuote(arg))
	}
	builder.WriteByte('\n')
	return builder.String()
}

func unmountShellScript(mountPoint string) string {
	quoted := shellQuote(mountPoint)
	return "#!/bin/sh\n" +
		"fusermount3 -u " + quoted + " 2>/dev/null || " +
		"fusermount -u " + quoted + " 2>/dev/null || " +
		"umount " + quoted + " 2>/dev/null || true\n"
}

func systemdMountUnit(serviceName, startScript, stopScript, mountPoint string) string {
	return fmt.Sprintf(`[Unit]
Description=Persistent Proton Drive mount %s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s
ExecStop=%s
Restart=always
RestartSec=10
TimeoutStopSec=20

[Install]
WantedBy=default.target
`, serviceName, systemdQuote(startScript), systemdQuote(stopScript))
}

func openRCMountService(openRCRun, serviceName, startScript, stopScript string) string {
	return fmt.Sprintf(`#!%s
description=%s
supervisor=supervise-daemon
command=%s
respawn_delay=10
respawn_max=3
respawn_period=30
retry="TERM/20/KILL/5"
output_log="${XDG_RUNTIME_DIR}/${RC_SVCNAME}.log"
error_log="${XDG_RUNTIME_DIR}/${RC_SVCNAME}.log"

depend() {
	use net dns
	after net
}

start_pre() {
	if [ -z "${XDG_RUNTIME_DIR:-}" ]; then
		eerror "XDG_RUNTIME_DIR is required for OpenRC user services"
		return 1
	fi
}

stop_post() {
	%s || true
}
`, openRCRun, shellQuote("Persistent Proton Drive mount "+serviceName), shellQuote(startScript), shellQuote(stopScript))
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func systemdQuote(value string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`$`, `$$`,
		`%`, `%%`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(value)
	return `"` + escaped + `"`
}

func runCommand(name string, args ...string) error {
	cmd := safeExecCommand(name, args...) // #nosec G204,G702 -- internal helper for fixed service-manager commands
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s failed: %w", filepath.Base(name), strings.Join(args, " "), err)
	}
	return nil
}

func enableUserLinger() error {
	userName := strings.TrimSpace(os.Getenv("USER"))
	if userName == "" {
		return errors.New("unable to enable lingering because USER is empty")
	}
	if _, err := exec.LookPath("loginctl"); err != nil {
		return errors.New("loginctl not found; cannot enable lingering automatically")
	}
	if err := runCommand("loginctl", "enable-linger", userName); err != nil {
		return fmt.Errorf("unable to enable lingering: %w", err)
	}
	fmt.Printf("Enabled systemd lingering for user %s.\n", userName)
	return nil
}

func mountErrorWithHints(target, mountPoint string, timeout time.Duration, mountErr error, background bool) error {
	if mountErr == nil {
		return errors.New("mount failed without an error description")
	}
	message := fmt.Sprintf("Failed to mount %s at %s: %v", target, mountPoint, mountErr)
	lower := strings.ToLower(mountErr.Error())
	switch {
	case strings.Contains(lower, "context deadline exceeded"), strings.Contains(lower, "timed out"), strings.Contains(lower, "did not become ready"):
		message += fmt.Sprintf(" (the mount did not become ready within %s)", timeout.String())
	case strings.Contains(lower, "fusermount"):
		message += " (rclone could not communicate with fusermount/fusermount3)"
	case strings.Contains(lower, "macfuse"), strings.Contains(lower, "osxfuse"), strings.Contains(lower, "file system is not available"):
		message += " (macFUSE is installed incorrectly, not loaded, or blocked by macOS system extension policy)"
	}

	hints := []string{
		"Ensure the mount point exists and is empty.",
		"Rerun with --foreground to inspect rclone's log output.",
	}
	if runtime.GOOS == "darwin" {
		hints = append(hints, "Verify that macFUSE is installed, approved in macOS Privacy & Security, and loaded.")
	} else {
		hints = append(hints, "Verify that fusermount/fusermount3 is installed and accessible.")
	}
	if background {
		hints = append(hints, fmt.Sprintf("Increase --ready-timeout if Proton Drive needs longer than %s to initialize.", timeout.String()))
	}
	if strings.Contains(lower, "permission denied") {
		hints = append(hints, "Check filesystem permissions or try mounting with sudo if necessary.")
	}

	return fmt.Errorf("%s\nTroubleshooting tips:\n  - %s", message, strings.Join(hints, "\n  - "))
}

func runUnmount(remote string, args []string) error {
	fs := flag.NewFlagSet("unmount", flag.ContinueOnError)
	force := fs.Bool("force", false, "Force unmount (try to detach a stuck mount)")
	removePersist := fs.Bool("remove-persist", false, "Disable and remove the persistent Linux user mount service")
	persistName := fs.String("persist-name", "", "Name suffix for the persistent user mount service")
	persistManager := fs.String("persist-manager", persistentMountManagerAuto, "Persistent service manager: auto, systemd, or openrc")
	parseArgs := normalizeInterspersedFlags(args, map[string]bool{
		"force":           false,
		"remove-persist":  false,
		"persist-name":    true,
		"persist-manager": true,
	})
	if err := parseCommandFlags(fs, parseArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return errors.New("unmount requires a mount point argument")
	}
	if len(remaining) != 1 {
		return errors.New("unmount accepts exactly one mount point argument")
	}
	mountPoint, err := filepath.Abs(expandPath(remaining[0]))
	if err != nil {
		return fmt.Errorf("unable to resolve mount point: %w", err)
	}

	if *removePersist {
		if err := removePersistentMount(remote, mountPoint, *persistName, *persistManager); err != nil {
			return err
		}
		if mounted, err := isPathMounted(mountPoint); err == nil && !mounted {
			recordMountDetach(remote, mountPoint)
			fmt.Printf("%s is not mounted; persistent service was removed.\n", mountPoint)
			return nil
		}
	}
	if mounted, err := isPathMounted(mountPoint); err == nil && !mounted {
		if err := stopRecordedMountProcess(remote, mountPoint); err != nil {
			return fmt.Errorf("%s is not mounted, but its recorded helper could not be stopped safely: %w", mountPoint, err)
		}
		recordMountDetach(remote, mountPoint)
		fmt.Printf("%s is not mounted; any recorded helper and private authentication file were cleaned up.\n", mountPoint)
		return nil
	}

	candidates := unmountCommands(mountPoint, *force)
	if len(candidates) == 0 {
		return fmt.Errorf("unmount is not supported automatically on %s; please use system tools", runtime.GOOS)
	}

	var tried []string
	var lastErr error
	for _, candidate := range candidates {
		if len(candidate) == 0 {
			continue
		}
		if !externalCommandAvailable(candidate[0]) {
			continue
		}
		tried = append(tried, strings.Join(candidate, " "))
		cmd := externalCommand(candidate[0], candidate[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			if err := waitForUnmounted(mountPoint, 5*time.Second); err != nil {
				return err
			}
			if err := stopRecordedMountProcess(remote, mountPoint); err != nil {
				return fmt.Errorf("mount detached but recorded helper could not be stopped safely: %w", err)
			}
			recordMountDetach(remote, mountPoint)
			fmt.Printf("Unmounted %s.\n", mountPoint)
			return nil
		} else {
			lastErr = err
		}
	}

	if len(tried) == 0 {
		return fmt.Errorf("no supported unmount commands were found on %s", runtime.GOOS)
	}
	if lastErr != nil {
		return fmt.Errorf("failed to unmount %s (tried %s): %w", mountPoint, strings.Join(tried, ", "), lastErr)
	}
	return errors.New("failed to unmount for an unknown reason")
}

func waitForUnmounted(mountPoint string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mounted, err := mountedPathCheck(mountPoint)
		if err == nil && !mounted {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("unmount command returned successfully but %s is still mounted", mountPoint)
}

func unmountPathForRollback(mountPoint string) error {
	var errs []error
	for _, candidate := range unmountCommands(mountPoint, false) {
		if len(candidate) == 0 || !externalCommandAvailable(candidate[0]) {
			continue
		}
		cmd := externalCommand(candidate[0], candidate[1:]...)
		if output, err := cmd.CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("%s failed: %s", strings.Join(candidate, " "), strings.TrimSpace(string(output))))
			continue
		}
		if err := waitForUnmounted(mountPoint, 5*time.Second); err != nil {
			return err
		}
		return nil
	}
	if len(errs) == 0 {
		return fmt.Errorf("mount rollback failed: no supported unmount helper found for %s", runtime.GOOS)
	}
	return errors.Join(errs...)
}

func stopRecordedMountProcess(remote, mountPoint string) error {
	state, err := loadRemoteState(remote)
	if err != nil {
		return err
	}
	abs := filepath.Clean(mountPoint)
	for _, entry := range state.Mounts {
		if sameMountPoint(entry.MountPoint, abs) && entry.ProcessID > 0 {
			return stopRecordedProcess(entry)
		}
	}
	return nil
}

func stopRecordedProcess(entry mountState) error {
	if !processExists(entry.ProcessID) {
		return removeRecordedMountAuthFile(entry)
	}
	command, err := processCommandLine(entry.ProcessID)
	if err != nil {
		return fmt.Errorf("unable to verify recorded mount process %d: %w", entry.ProcessID, err)
	}
	if strings.TrimSpace(entry.ProcessExecutable) == "" || strings.TrimSpace(entry.ProcessStartToken) == "" {
		return fmt.Errorf("refusing to signal legacy PID %d without recorded executable and start identity; stop the verified rclone WebDAV process manually", entry.ProcessID)
	}
	if !strings.Contains(command, filepath.Base(entry.ProcessExecutable)) || !strings.Contains(command, "serve webdav") {
		return fmt.Errorf("refusing to signal PID %d because it is no longer the recorded rclone WebDAV helper", entry.ProcessID)
	}
	currentStart, err := processStartToken(entry.ProcessID)
	if err != nil {
		return fmt.Errorf("unable to verify start identity for PID %d: %w", entry.ProcessID, err)
	}
	if currentStart != entry.ProcessStartToken {
		return fmt.Errorf("refusing to signal reused PID %d (process start identity changed)", entry.ProcessID)
	}
	if err := stopProcess(entry.ProcessID); err != nil {
		return err
	}
	if strings.TrimSpace(entry.URL) != "" {
		if err := waitForWebDAVClosed(entry.URL, 3*time.Second); err != nil {
			return err
		}
	}
	return removeRecordedMountAuthFile(entry)
}

func removeRecordedMountAuthFile(entry mountState) error {
	if strings.TrimSpace(entry.AuthFile) == "" {
		return nil
	}
	credentialDir, err := credentialDirPath()
	if err != nil {
		return err
	}
	clean := filepath.Clean(entry.AuthFile)
	logsDir := filepath.Join(filepath.Clean(credentialDir), "logs")
	base := filepath.Base(clean)
	if !sameMountPoint(filepath.Dir(clean), logsDir) || !strings.HasPrefix(base, "webdav-auth-") || !strings.HasSuffix(base, ".htpasswd") {
		return fmt.Errorf("refusing to remove WebDAV authentication file outside %s", logsDir)
	}
	return safefile.Remove(clean)
}

func processStartToken(pid int) (string, error) {
	output, err := safeExecCommand("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output() // #nosec G204 -- PID is an integer generated by the OS
	if err != nil {
		return "", err
	}
	token := strings.Join(strings.Fields(string(output)), " ")
	if token == "" {
		return "", errors.New("process start time is empty")
	}
	return token, nil
}

func processCommandLine(pid int) (string, error) {
	output, err := safeExecCommand("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output() // #nosec G204 -- PID is an integer generated by the OS
	if err != nil {
		return "", err
	}
	command := strings.TrimSpace(string(output))
	if command == "" {
		return "", errors.New("process command is empty")
	}
	return command, nil
}

func unmountCommands(mountPoint string, force bool) [][]string {
	switch runtime.GOOS {
	case "linux":
		flag := "-u"
		if force {
			flag = "-uz"
		}
		umountCmd := []string{"umount"}
		if force {
			umountCmd = append(umountCmd, "-f")
		}
		umountCmd = append(umountCmd, mountPoint)
		return [][]string{
			{"fusermount", flag, mountPoint},
			{"fusermount3", flag, mountPoint},
			umountCmd,
		}
	case "darwin":
		if force {
			return [][]string{
				{"diskutil", "unmount", "force", mountPoint},
				{"umount", "-f", mountPoint},
			}
		}
		return [][]string{
			{"diskutil", "unmount", mountPoint},
			{"umount", mountPoint},
		}
	case "windows":
		return [][]string{
			{"mountvol", mountPoint, "/D"},
		}
	default:
		return nil
	}
}
