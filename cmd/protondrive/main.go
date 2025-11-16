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
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ColinMario/Protondrive-for-Linux/internal/customconfigs"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/crypto/scrypt"
	"golang.org/x/term"
)

const (
	remoteDefault         = "protondrive"
	vaultPassphraseEnv    = "PROTONDRIVE_VAULT_PASSPHRASE"
	defaultRobust         = true
	defaultMaxAttempts    = 5
	defaultBackoffInitial = 15 * time.Second
	defaultBackoffMax     = 2 * time.Minute
)

var procMountReplacer = strings.NewReplacer(
	"\\040", " ",
	"\\011", "\t",
	"\\012", "\n",
	"\\134", "\\",
)

type repeatableFlag []string

func (f *repeatableFlag) String() string {
	return strings.Join(*f, ", ")
}

func (f *repeatableFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type optionalBoolFlag struct {
	value bool
	set   bool
}

func (f *optionalBoolFlag) String() string {
	return strconv.FormatBool(f.value)
}

func (f *optionalBoolFlag) Set(value string) error {
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	f.value = parsed
	f.set = true
	return nil
}

func (f *optionalBoolFlag) IsBoolFlag() bool {
	return true
}

func (f *optionalBoolFlag) Value(defaultVal bool) bool {
	if f.set {
		return f.value
	}
	return defaultVal
}

type optionalDurationFlag struct {
	value time.Duration
	set   bool
}

func (f *optionalDurationFlag) String() string {
	return f.value.String()
}

func (f *optionalDurationFlag) Set(value string) error {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return err
	}
	f.value = parsed
	f.set = true
	return nil
}

func (f *optionalDurationFlag) Value(defaultVal time.Duration) time.Duration {
	if f.set {
		return f.value
	}
	return defaultVal
}

type optionalIntFlag struct {
	value int
	set   bool
}

func (f *optionalIntFlag) String() string {
	return strconv.Itoa(f.value)
}

func (f *optionalIntFlag) Set(value string) error {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	f.value = parsed
	f.set = true
	return nil
}

func (f *optionalIntFlag) Value(defaultVal int) int {
	if f.set {
		return f.value
	}
	return defaultVal
}

type syncConfig struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	LocalPath       string   `json:"local_path"`
	RemotePath      string   `json:"remote_path"`
	Direction       string   `json:"direction"`
	Watch           bool     `json:"watch"`
	WatchDebounce   string   `json:"watch_debounce"`
	ExtraRcloneArgs []string `json:"extra_rclone_args"`
	Robust          *bool    `json:"robust"`
	MaxAttempts     int      `json:"max_attempts"`
	BackoffInitial  string   `json:"backoff_initial"`
	BackoffMax      string   `json:"backoff_max"`
}

type loadedSyncConfig struct {
	Config      syncConfig
	Source      string
	DisplayName string
}

type syncConfigSummary struct {
	Name        string
	Description string
	File        string
}

func main() {
	remote, args, err := parseGlobalArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		printUsage()
		os.Exit(2)
	}

	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	if err := ensureRclone(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	cmd := args[0]
	switch cmd {
	case "configure":
		err = runConfigure(remote, args[1:])
	case "status":
		err = runStatus(remote, args[1:])
	case "browse":
		err = runBrowse(remote, args[1:])
	case "sync":
		err = runSync(remote, args[1:])
	case "mount":
		err = runMount(remote, args[1:])
	case "unmount":
		err = runUnmount(remote, args[1:])
	case "configs":
		err = runConfigs(remote, args[1:])
	case "help", "-h", "--help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func parseGlobalArgs(args []string) (string, []string, error) {
	remote := remoteDefault
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}

		switch {
		case arg == "-h" || arg == "--help":
			printUsage()
			os.Exit(0)
		case arg == "--remote":
			if i+1 >= len(args) {
				return "", nil, errors.New("missing value for --remote")
			}
			remote = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--remote="):
			remote = strings.TrimPrefix(arg, "--remote=")
			i++
		default:
			return "", nil, fmt.Errorf("unknown global flag: %s", arg)
		}
	}
	return remote, args[i:], nil
}

func runConfigure(remote string, args []string) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	email := fs.String("email", "", "ProtonMail email address")
	password := fs.String("password", "", "ProtonMail password (use with caution)")
	passwordStdin := fs.Bool("password-stdin", false, "Read password from stdin")
	twofa := fs.String("twofa", "", "Optional 2FA code")
	twofaStdin := fs.Bool("twofa-stdin", false, "Read 2FA code from stdin")
	nonInteractive := fs.Bool("non-interactive", false, "Fail instead of prompting")
	skipVerify := fs.Bool("skip-verify", false, "Skip connection test after configuring")
	storeCreds := fs.Bool("store-credentials", false, "Encrypt credentials locally for automatic reauth")
	vaultPass := fs.String("vault-passphrase", "", "Passphrase used for --store-credentials (use with caution)")
	vaultPassStdin := fs.Bool("vault-passphrase-stdin", false, "Read vault passphrase from stdin")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	reader := bufio.NewReader(os.Stdin)

	if *email == "" && !*nonInteractive {
		value, err := promptLine(reader, "ProtonMail email: ")
		if err != nil {
			return err
		}
		*email = value
	}
	if strings.TrimSpace(*email) == "" {
		return errors.New("email is required for configuration")
	}

	passValue := strings.TrimSpace(*password)
	var err error
	if *passwordStdin {
		passValue, err = readLine(reader)
	} else if passValue == "" && !*nonInteractive {
		passValue, err = promptPassword("ProtonMail password: ")
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(passValue) == "" {
		return errors.New("password is required for configuration")
	}

	twofaValue := strings.TrimSpace(*twofa)
	if *twofaStdin {
		twofaValue, err = readLine(reader)
		if err != nil {
			return err
		}
	} else if twofaValue == "" && !*nonInteractive {
		value, err := promptLine(reader, "2FA code (leave empty if unused): ")
		if err != nil {
			return err
		}
		twofaValue = value
	}

	if err := configureRemote(remote, *email, passValue, twofaValue, false); err != nil {
		return err
	}

	if !*skipVerify {
		fmt.Println("Verifying connection...")
		if err := verifyRemote(remote); err != nil {
			recordAuthEvent(remote, "configure", false, strings.TrimSpace(err.Error()))
			return fmt.Errorf("verification failed: %w", err)
		}
		recordAuthEvent(remote, "configure", true, "")
		fmt.Println("ProtonDrive connection verified.")
	}

	if *storeCreds {
		passphrase, err := resolveVaultPassphrase(reader, *vaultPass, *vaultPassStdin, *nonInteractive)
		if err != nil {
			return fmt.Errorf("unable to store credentials: %w", err)
		}
		record := storedCredentials{
			Email:    *email,
			Password: passValue,
			TwoFA:    twofaValue,
			SavedAt:  time.Now(),
		}
		path, err := saveEncryptedCredentials(remote, record, passphrase)
		if err != nil {
			return fmt.Errorf("unable to store credentials: %w", err)
		}
		recordVaultUpdate(remote, record.SavedAt)
		fmt.Printf("Encrypted credentials saved for remote '%s' at %s.\n", remote, path)
	}

	return nil
}

func runStatus(remote string, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	details := fs.Bool("details", false, "List ProtonDrive folders if configured")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	output, err := runRcloneCapture("listremotes")
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s:", normalizeRemote(remote))
	found := false
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == target {
			found = true
			break
		}
	}
	if !found {
		fmt.Printf("Remote '%s' is not configured.\n", remote)
		return nil
	}
	fmt.Printf("Remote '%s' is configured.\n", remote)

	if *details {
		hasVault := hasStoredCredentials(remote)
		state, err := loadRemoteState(remote)
		if err != nil {
			logStateWarning(err)
			state = remoteState{Remote: normalizedRemoteName(remote)}
		}
		if err := ensureRemoteAuth(remote); err != nil {
			fmt.Println(strings.TrimSpace(err.Error()))
			printStatusDetails(remote, state, hasVault)
			return nil
		}
		state, err = loadRemoteState(remote)
		if err != nil {
			logStateWarning(err)
			state = remoteState{Remote: normalizedRemoteName(remote)}
		}
		printStatusDetails(remote, state, hasVault)

		fmt.Println("Listing top-level folders:")
		data, err := runRcloneCapture("lsd", remotePath(remote, ""))
		if err != nil {
			fmt.Println(strings.TrimSpace(err.Error()))
			return nil
		}
		if strings.TrimSpace(data) == "" {
			fmt.Println("(empty)")
		} else {
			fmt.Println(strings.TrimSpace(data))
		}
	}
	return nil
}

func printStatusDetails(remote string, state remoteState, vaultPresent bool) {
	fmt.Println("Connection details:")
	if state.LastAuthSuccess.IsZero() {
		fmt.Println("  Last authentication: (no successful checks recorded yet)")
	} else {
		fmt.Printf("  Last authentication: %s via %s\n", formatTimestamp(state.LastAuthSuccess), describeAuthMethod(state.LastAuthMethod))
	}
	if state.LastAuthError != "" && state.LastAuthAttempt.After(state.LastAuthSuccess) {
		fmt.Printf("  Last failure: %s at %s\n", state.LastAuthError, formatTimestamp(state.LastAuthAttempt))
	}
	fmt.Printf("  Auto-refresh vault: %s\n", describeVaultStatus(state, vaultPresent))
	printMountSummary(state)
}

func describeAuthMethod(method string) string {
	switch method {
	case "configure":
		return "manual configure"
	case "verify":
		return "status check"
	case "auto-refresh":
		return "auto-refresh"
	default:
		if strings.TrimSpace(method) == "" {
			return "unspecified"
		}
		return method
	}
}

func describeVaultStatus(state remoteState, vaultPresent bool) string {
	if !vaultPresent {
		if state.VaultConfigured {
			return "missing (stored credentials were configured but the encrypted file is gone)"
		}
		return "disabled"
	}
	if !state.VaultUpdated.IsZero() {
		return fmt.Sprintf("enabled (last updated %s)", formatTimestamp(state.VaultUpdated))
	}
	return "enabled"
}

func printMountSummary(state remoteState) {
	fmt.Println("  Mounts:")
	if len(state.Mounts) == 0 {
		fmt.Println("    (no ProtonDrive mounts recorded yet)")
		return
	}
	for _, entry := range state.Mounts {
		fmt.Println("    - " + describeMountEntry(entry))
	}
}

func describeMountEntry(entry mountState) string {
	remotePath := entry.RemotePath
	if strings.TrimSpace(remotePath) == "" {
		remotePath = "<root>"
	}
	status := "detached"
	if entry.Attached {
		status = "attached"
	}
	var systemNote string
	if entry.MountPoint != "" {
		if mounted, err := isPathMounted(entry.MountPoint); err == nil {
			if mounted {
				status = "mounted"
			} else if entry.Attached {
				systemNote = "CLI thinks it's attached but the OS reports it unmounted"
			}
		} else if entry.Attached {
			systemNote = fmt.Sprintf("system status unavailable (%v)", err)
		}
	}
	builder := strings.Builder{}
	builder.WriteString(fmt.Sprintf("%s -> %s [%s", entry.MountPoint, remotePath, status))
	if !entry.LastUpdated.IsZero() {
		builder.WriteString(fmt.Sprintf("; updated %s", formatTimestamp(entry.LastUpdated)))
	}
	builder.WriteString("]")
	if systemNote != "" {
		builder.WriteString(" (")
		builder.WriteString(systemNote)
		builder.WriteString(")")
	}
	return builder.String()
}

func formatTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return "unknown"
	}
	return ts.Local().Format(time.RFC3339)
}

func runBrowse(remote string, args []string) error {
	fs := flag.NewFlagSet("browse", flag.ContinueOnError)
	remotePathFlag := fs.String("remote-path", "", "Remote path to inspect (defaults to root)")
	files := fs.Bool("files", false, "Show files with rclone ls instead of directories")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if err := ensureRemoteAuth(remote); err != nil {
		return err
	}

	target := remotePath(remote, *remotePathFlag)
	var command []string
	if *files {
		command = []string{"ls", target}
	} else {
		command = []string{"lsd", target}
	}

	data, err := runRcloneCapture(command...)
	if err != nil {
		return err
	}
	data = strings.TrimSpace(data)
	if data == "" {
		fmt.Println("No entries found.")
	} else {
		fmt.Println(data)
	}
	return nil
}

func runSync(remote string, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	remotePathFlag := fs.String("remote-path", "", "Remote folder (defaults to root)")
	directionFlag := fs.String("direction", "", "Sync direction: upload or download (defaults to upload)")
	dryRun := fs.Bool("dry-run", false, "Show actions without applying changes")
	noProgress := fs.Bool("no-progress", false, "Disable rclone progress output")
	configName := fs.String("config", "", "Use a saved sync config name or JSON file path")
	var watchFlag optionalBoolFlag
	fs.Var(&watchFlag, "watch", "Watch the local folder for changes (upload only)")
	watchDebounceFlag := optionalDurationFlag{value: 10 * time.Second}
	fs.Var(&watchDebounceFlag, "watch-debounce", "Minimum delay between syncs while watching (default 10s)")
	var robustFlag optionalBoolFlag
	fs.Var(&robustFlag, "robust", "Automatically retry failed syncs with exponential backoff")
	var maxAttemptsFlag optionalIntFlag
	fs.Var(&maxAttemptsFlag, "max-attempts", "Maximum attempts when --robust is enabled (default 5)")
	backoffInitialFlag := optionalDurationFlag{value: defaultBackoffInitial}
	fs.Var(&backoffInitialFlag, "backoff-initial", "Initial backoff when --robust retries (default 15s)")
	backoffMaxFlag := optionalDurationFlag{value: defaultBackoffMax}
	fs.Var(&backoffMaxFlag, "backoff-max", "Maximum backoff when --robust retries (default 2m)")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	var cfg *loadedSyncConfig
	if strings.TrimSpace(*configName) != "" {
		loaded, err := loadSyncConfig(*configName)
		if err != nil {
			return err
		}
		cfg = &loaded
		fmt.Printf("Using sync config \"%s\" (%s).\n", cfg.DisplayName, describeConfigSource(cfg.Source))
	}

	if err := ensureRemoteAuth(remote); err != nil {
		return err
	}

	remaining := fs.Args()
	var positionalLocal string
	var extra []string
	if len(remaining) > 0 {
		positionalLocal = remaining[0]
		extra = remaining[1:]
	}

	localPath := positionalLocal
	if strings.TrimSpace(localPath) == "" && cfg != nil && strings.TrimSpace(cfg.Config.LocalPath) != "" {
		localPath = cfg.Config.LocalPath
		extra = remaining
	}
	if strings.TrimSpace(localPath) == "" {
		return errors.New("sync requires a local folder argument or a config with 'local_path'")
	}

	remotePathValue := strings.TrimSpace(*remotePathFlag)
	if remotePathValue == "" && cfg != nil {
		remotePathValue = strings.TrimSpace(cfg.Config.RemotePath)
	}

	dir := strings.ToLower(strings.TrimSpace(*directionFlag))
	if dir == "" && cfg != nil {
		dir = strings.ToLower(strings.TrimSpace(cfg.Config.Direction))
	}
	if dir == "" {
		dir = "upload"
	}
	if dir != "upload" && dir != "download" {
		return errors.New("direction must be 'upload' or 'download'")
	}

	watchDefault := false
	if cfg != nil && cfg.Config.Watch {
		watchDefault = true
	}
	watchEnabled := watchFlag.Value(watchDefault)
	if watchEnabled && dir != "upload" {
		return errors.New("watch mode is only supported for upload direction")
	}

	watchDebounce := watchDebounceFlag.Value(10 * time.Second)
	if !watchDebounceFlag.set && cfg != nil && strings.TrimSpace(cfg.Config.WatchDebounce) != "" {
		value := strings.TrimSpace(cfg.Config.WatchDebounce)
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("config \"%s\" has invalid watch_debounce %q: %w", cfg.DisplayName, value, err)
		}
		watchDebounce = parsed
	}
	if watchEnabled && watchDebounce <= 0 {
		watchDebounce = 10 * time.Second
	}

	robustDefault := defaultRobust
	if cfg != nil && cfg.Config.Robust != nil {
		robustDefault = *cfg.Config.Robust
	}
	robustEnabled := robustFlag.Value(robustDefault)

	maxAttemptsDefault := defaultMaxAttempts
	if cfg != nil && cfg.Config.MaxAttempts > 0 {
		maxAttemptsDefault = cfg.Config.MaxAttempts
	}
	maxAttempts := maxAttemptsFlag.Value(maxAttemptsDefault)
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	backoffInitialDefault := defaultBackoffInitial
	if cfg != nil && strings.TrimSpace(cfg.Config.BackoffInitial) != "" {
		value := strings.TrimSpace(cfg.Config.BackoffInitial)
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("config \"%s\" has invalid backoff_initial %q: %w", cfg.DisplayName, value, err)
		}
		backoffInitialDefault = parsed
	}
	backoffInitial := backoffInitialFlag.Value(backoffInitialDefault)

	backoffMaxDefault := defaultBackoffMax
	if cfg != nil && strings.TrimSpace(cfg.Config.BackoffMax) != "" {
		value := strings.TrimSpace(cfg.Config.BackoffMax)
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("config \"%s\" has invalid backoff_max %q: %w", cfg.DisplayName, value, err)
		}
		backoffMaxDefault = parsed
	}
	backoffMax := backoffMaxFlag.Value(backoffMaxDefault)
	if backoffInitial <= 0 {
		backoffInitial = defaultBackoffInitial
	}
	if backoffMax <= 0 || backoffMax < backoffInitial {
		backoffMax = backoffInitial
	}

	localAbs := expandPath(localPath)
	if dir == "upload" {
		if stat, err := os.Stat(localAbs); err != nil || !stat.IsDir() {
			return fmt.Errorf("local path '%s' must exist and be a directory", localAbs)
		}
	} else {
		if err := os.MkdirAll(localAbs, 0o755); err != nil {
			return fmt.Errorf("unable to create local folder '%s': %w", localAbs, err)
		}
	}

	var src, dst string
	target := remotePath(remote, remotePathValue)
	if dir == "upload" {
		src, dst = localAbs, target
	} else {
		src, dst = target, localAbs
	}

	cmd := []string{"sync", src, dst, "-v"}
	if !*noProgress {
		cmd = append(cmd, "--progress")
	}
	if *dryRun {
		cmd = append(cmd, "--dry-run")
	}
	if cfg != nil && len(cfg.Config.ExtraRcloneArgs) > 0 {
		cmd = append(cmd, cfg.Config.ExtraRcloneArgs...)
	}
	cmd = append(cmd, extra...)

	retryCfg := retryOptions{
		enabled:        robustEnabled,
		maxAttempts:    maxAttempts,
		initialBackoff: backoffInitial,
		maxBackoff:     backoffMax,
	}
	runOnce := func() error {
		return runRcloneWithRetry(cmd, retryCfg)
	}
	if watchEnabled {
		fmt.Printf("Watching %s for changes (debounce %s). Press Ctrl+C to stop.\n", localAbs, watchDebounce)
		return watchAndSync(localAbs, watchDebounce, runOnce)
	}
	return runOnce()
}

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
	readyTimeout := fs.Duration("ready-timeout", 30*time.Second, "Max wait for mounts to become ready when backgrounding")
	var customFlags repeatableFlag
	fs.Var(&customFlags, "rclone-flag", "Additional flag passed through to rclone mount (repeatable)")
	var robustFlag optionalBoolFlag
	fs.Var(&robustFlag, "robust", "Automatically retry failed mounts with exponential backoff")
	var maxAttemptsFlag optionalIntFlag
	fs.Var(&maxAttemptsFlag, "max-attempts", "Maximum mount attempts when --robust is enabled (default 5)")
	backoffInitialFlag := optionalDurationFlag{value: defaultBackoffInitial}
	fs.Var(&backoffInitialFlag, "backoff-initial", "Initial backoff when --robust retries mounts (default 15s)")
	backoffMaxFlag := optionalDurationFlag{value: defaultBackoffMax}
	fs.Var(&backoffMaxFlag, "backoff-max", "Maximum backoff when --robust retries mounts (default 2m)")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if err := ensureRemoteAuth(remote); err != nil {
		return err
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return errors.New("mount requires a mount point argument")
	}
	mountPoint := expandPath(remaining[0])
	extra := remaining[1:]

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("unable to create mount point '%s': %w", mountPoint, err)
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

	robustEnabled := robustFlag.Value(defaultRobust)
	maxAttempts := maxAttemptsFlag.Value(defaultMaxAttempts)
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	backoffInitial := backoffInitialFlag.Value(defaultBackoffInitial)
	backoffMax := backoffMaxFlag.Value(defaultBackoffMax)
	if backoffInitial <= 0 {
		backoffInitial = defaultBackoffInitial
	}
	if backoffMax <= 0 || backoffMax < backoffInitial {
		backoffMax = backoffInitial
	}
	retryCfg := retryOptions{
		enabled:        robustEnabled,
		maxAttempts:    maxAttempts,
		initialBackoff: backoffInitial,
		maxBackoff:     backoffMax,
	}

	target := remotePath(remote, *remotePathFlag)
	if *foreground {
		fmt.Printf("Mounting %s at %s. Press Ctrl+C to stop.\n", target, mountPoint)
		if err := runRcloneWithRetry(cmd, retryCfg); err != nil {
			return mountErrorWithHints(target, mountPoint, *readyTimeout, err, false)
		}
		return nil
	}

	fmt.Printf("Mounting %s at %s. This returns once the mount is ready.\n", target, mountPoint)
	if err := runRcloneWithRetry(cmd, retryCfg); err != nil {
		return mountErrorWithHints(target, mountPoint, *readyTimeout, err, true)
	}
	recordMountAttach(remote, mountPoint, target)
	fmt.Printf("Mount ready at %s. Use 'protondrive unmount %s' to detach.\n", mountPoint, mountPoint)
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
	}

	hints := []string{
		"Ensure the mount point exists and is empty.",
		"Rerun with --foreground to inspect rclone's log output.",
		"Verify that fusermount/fusermount3 is installed and accessible.",
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
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return errors.New("unmount requires a mount point argument")
	}
	mountPoint := expandPath(remaining[0])

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
		if _, err := exec.LookPath(candidate[0]); err != nil {
			continue
		}
		tried = append(tried, strings.Join(candidate, " "))
		cmd := exec.Command(candidate[0], candidate[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			fmt.Printf("Unmounted %s.\n", mountPoint)
			recordMountDetach(remote, mountPoint)
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
				{"diskutil", "unmountForce", mountPoint},
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

func runConfigs(remote string, args []string) error {
	fs := flag.NewFlagSet("configs", flag.ContinueOnError)
	force := fs.Bool("force", false, "Allow overwriting an existing file when using 'init'")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	remaining := fs.Args()
	if len(remaining) == 0 || remaining[0] == "list" {
		return printSyncConfigList()
	}

	switch remaining[0] {
	case "show":
		if len(remaining) < 2 {
			return errors.New("usage: protondrive configs show <name-or-path>")
		}
		return showSyncConfig(remaining[1])
	case "init":
		if len(remaining) < 2 {
			return errors.New("usage: protondrive configs init <template-name>")
		}
		dest, err := writeBuiltinConfig(remaining[1], *force)
		if err != nil {
			return err
		}
		fmt.Printf("Template copied to %s\n", dest)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (expected list, show, init)", remaining[0])
	}
}

func printSyncConfigList() error {
	builtins, err := customconfigs.List()
	if err != nil {
		return fmt.Errorf("unable to list built-in templates: %w", err)
	}
	fmt.Println("Built-in templates:")
	if len(builtins) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, tpl := range builtins {
			fmt.Printf("  - %s (%s)\n", tpl.ID, tpl.Description)
		}
	}
	fmt.Println()

	customConfigs, dir, err := listCustomSyncConfigs()
	if err != nil {
		return fmt.Errorf("unable to list custom configs: %w", err)
	}
	fmt.Printf("Custom config directory: %s\n", dir)
	if len(customConfigs) == 0 {
		fmt.Println("  (no JSON configs found yet)")
	} else {
		for _, summary := range customConfigs {
			fmt.Printf("  - %s (%s)\n", summary.Name, summary.Description)
			fmt.Printf("    %s\n", summary.File)
		}
	}
	fmt.Println("\nUse 'protondrive configs init <template>' to copy a built-in template, then edit it to match your paths.")
	return nil
}

func showSyncConfig(identifier string) error {
	cfg, err := loadSyncConfig(identifier)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg.Config, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	fmt.Printf("\nSource: %s\n", describeConfigSource(cfg.Source))
	return nil
}

func writeBuiltinConfig(name string, force bool) (string, error) {
	template, found, err := customconfigs.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("unable to load built-in templates: %w", err)
	}
	if !found {
		return "", fmt.Errorf("built-in template %q not found; run 'protondrive configs list' for options", name)
	}
	dir, err := ensureSyncConfigDir()
	if err != nil {
		return "", err
	}
	filename := configFileName(template.ID)
	dest := filepath.Join(dir, filename)
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return "", fmt.Errorf("%s already exists (re-run with --force to overwrite)", dest)
		}
	}
	if err := os.WriteFile(dest, template.Raw, 0o600); err != nil {
		return "", err
	}
	return dest, nil
}

func configureRemote(remote, email, password, twofa string, quiet bool) error {
	if !quiet {
		fmt.Printf("Configuring rclone remote '%s'...\n", remote)
	}
	exec.Command("rclone", "config", "delete", remote).Run()

	obscured, err := runRcloneCapture("obscure", password)
	if err != nil {
		return fmt.Errorf("failed to process password: %w", err)
	}

	cmd := []string{
		"config", "create", remote, "protondrive",
		fmt.Sprintf("username=%s", email),
		fmt.Sprintf("password=%s", strings.TrimSpace(obscured)),
	}
	if strings.TrimSpace(twofa) != "" {
		cmd = append(cmd, fmt.Sprintf("2fa=%s", twofa))
	}
	if _, err := runRcloneCapture(cmd...); err != nil {
		return fmt.Errorf("rclone config create failed: %w", err)
	}
	if !quiet {
		fmt.Println("Remote saved successfully.")
	}
	return nil
}

func verifyRemote(remote string) error {
	_, err := runRcloneCapture("lsd", remotePath(remote, ""))
	return err
}

func ensureRemoteAuth(remote string) error {
	if err := verifyRemote(remote); err != nil {
		recordAuthEvent(remote, "verify", false, strings.TrimSpace(err.Error()))
		if !isAuthError(err) {
			return err
		}
		if !hasStoredCredentials(remote) {
			return fmt.Errorf("%w; re-run 'protondrive configure --store-credentials' to enable auto-refresh", err)
		}
		fmt.Println("Remote authentication failed. Attempting to refresh credentials...")
		if err := tryAutoRefresh(remote); err != nil {
			recordAuthEvent(remote, "auto-refresh", false, strings.TrimSpace(err.Error()))
			return fmt.Errorf("automatic refresh failed: %w", err)
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
	if strings.Contains(msg, "username and password are required") {
		return true
	}
	if strings.Contains(msg, "couldn't initialize a new proton drive instance") {
		return true
	}
	if strings.Contains(msg, "401") && strings.Contains(msg, "unauthorized") {
		return true
	}
	if strings.Contains(msg, "403") && strings.Contains(msg, "forbidden") {
		return true
	}
	if strings.Contains(msg, "invalid_grant") {
		return true
	}
	if strings.Contains(msg, "failed to create file system") && strings.Contains(msg, "proton drive") {
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
	if strings.Contains(msg, "context deadline exceeded") {
		return true
	}
	if strings.Contains(msg, "connection reset by peer") {
		return true
	}
	if strings.Contains(msg, "tls handshake timeout") {
		return true
	}
	if strings.Contains(msg, "temporarily unavailable") {
		return true
	}
	if strings.Contains(msg, "broken pipe") {
		return true
	}
	if strings.Contains(msg, "use of closed network connection") {
		return true
	}
	return false
}

func tryAutoRefresh(remote string) error {
	passphrase := strings.TrimSpace(os.Getenv(vaultPassphraseEnv))
	var err error
	if passphrase == "" {
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
	if err := configureRemote(remote, creds.Email, creds.Password, creds.TwoFA, true); err != nil {
		return err
	}
	fmt.Println("Credentials refreshed from the local vault.")
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
	Email    string    `json:"email"`
	Password string    `json:"password"`
	TwoFA    string    `json:"twofa"`
	SavedAt  time.Time `json:"saved_at"`
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
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func loadEncryptedCredentials(remote, passphrase string) (storedCredentials, error) {
	path, err := credentialFilePath(remote)
	if err != nil {
		return storedCredentials{}, err
	}
	data, err := os.ReadFile(path)
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
	return sanitizedRemoteName(remote) + ".creds"
}

func remoteStateFilename(remote string) string {
	return sanitizedRemoteName(remote) + ".state"
}

func sanitizedRemoteName(remote string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	name := normalizeRemote(remote)
	if strings.TrimSpace(name) == "" {
		name = remoteDefault
	}
	return replacer.Replace(name)
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
	return filepath.Join(dir, credentialFilename(remote)), nil
}

func syncConfigDirPath() (string, error) {
	dir, err := credentialDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync-configs"), nil
}

func ensureSyncConfigDir() (string, error) {
	dir, err := syncConfigDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func loadSyncConfig(identifier string) (loadedSyncConfig, error) {
	name := strings.TrimSpace(identifier)
	if name == "" {
		return loadedSyncConfig{}, errors.New("sync config name cannot be empty")
	}

	if strings.Contains(name, "/") || strings.Contains(name, "\\") || filepath.Ext(name) != "" {
		path := expandPath(name)
		cfg, err := readSyncConfigFile(path)
		if err != nil {
			return loadedSyncConfig{}, err
		}
		return loadedSyncConfig{
			Config:      cfg,
			Source:      path,
			DisplayName: cfg.displayName(filepath.Base(path)),
		}, nil
	}

	dir, err := syncConfigDirPath()
	if err != nil {
		return loadedSyncConfig{}, err
	}

	for _, candidate := range uniqueStrings(configFileCandidates(dir, name)) {
		if cfg, err := readSyncConfigFile(candidate); err == nil {
			return loadedSyncConfig{
				Config:      cfg,
				Source:      candidate,
				DisplayName: cfg.displayName(filepath.Base(candidate)),
			}, nil
		}
	}

	template, found, err := customconfigs.Lookup(name)
	if err != nil {
		return loadedSyncConfig{}, fmt.Errorf("unable to load built-in templates: %w", err)
	}
	if found {
		cfg, err := parseSyncConfig(template.Raw)
		if err != nil {
			return loadedSyncConfig{}, fmt.Errorf("template %s is invalid: %w", template.Name, err)
		}
		if strings.TrimSpace(cfg.Name) == "" {
			cfg.Name = template.Name
		}
		return loadedSyncConfig{
			Config:      cfg,
			Source:      "builtin:" + template.ID,
			DisplayName: cfg.displayName(template.Name),
		}, nil
	}

	return loadedSyncConfig{}, fmt.Errorf("sync config %q not found. Place JSON files in %s or run 'protondrive configs list' to see built-in templates", name, dir)
}

func readSyncConfigFile(path string) (syncConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return syncConfig{}, err
	}
	cfg, err := parseSyncConfig(data)
	if err != nil {
		return syncConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return cfg, nil
}

func parseSyncConfig(data []byte) (syncConfig, error) {
	var cfg syncConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return syncConfig{}, fmt.Errorf("invalid sync config JSON: %w", err)
	}
	return cfg, nil
}

func (c syncConfig) displayName(fallback string) string {
	if strings.TrimSpace(c.Name) != "" {
		return strings.TrimSpace(c.Name)
	}
	return fallback
}

func describeConfigSource(source string) string {
	if strings.HasPrefix(source, "builtin:") {
		return fmt.Sprintf("built-in template %s", strings.TrimPrefix(source, "builtin:"))
	}
	if strings.TrimSpace(source) != "" {
		return source
	}
	return "custom config"
}

func configFileCandidates(dir, name string) []string {
	var candidates []string
	clean := strings.TrimSpace(name)
	if clean != "" {
		candidates = append(candidates, filepath.Join(dir, clean))
		if filepath.Ext(clean) == "" {
			candidates = append(candidates, filepath.Join(dir, clean+".json"))
		}
	}
	slug := slugifyConfigName(clean)
	candidates = append(candidates, filepath.Join(dir, slug), filepath.Join(dir, slug+".json"))
	return candidates
}

func slugifyConfigName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "config"
	}
	var builder strings.Builder
	prevDash := false

	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			prevDash = false
			continue
		}
		switch r {
		case ' ', '-', '_', '.', '/', '\\':
			if !prevDash {
				builder.WriteRune('-')
				prevDash = true
			}
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "config"
	}
	return result
}

func configFileName(name string) string {
	base := slugifyConfigName(name)
	if !strings.HasSuffix(base, ".json") {
		base += ".json"
	}
	return base
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var result []string
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func listCustomSyncConfigs() ([]syncConfigSummary, string, error) {
	dir, err := syncConfigDirPath()
	if err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, dir, nil
		}
		return nil, "", err
	}
	summaries := make([]syncConfigSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		summary, err := readSyncConfigSummary(path)
		if err != nil {
			summary = syncConfigSummary{
				Name:        strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
				Description: fmt.Sprintf("invalid config: %v", err),
				File:        path,
			}
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})
	return summaries, dir, nil
}

func readSyncConfigSummary(path string) (syncConfigSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return syncConfigSummary{}, err
	}
	var header struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return syncConfigSummary{}, err
	}
	name := strings.TrimSpace(header.Name)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	desc := strings.TrimSpace(header.Description)
	if desc == "" {
		desc = "(no description)"
	}
	return syncConfigSummary{Name: name, Description: desc, File: path}, nil
}

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
	MountPoint  string    `json:"mount_point"`
	RemotePath  string    `json:"remote_path"`
	Attached    bool      `json:"attached"`
	LastUpdated time.Time `json:"last_updated"`
}

func remoteStateFilePath(remote string) (string, error) {
	dir, err := credentialDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, remoteStateFilename(remote)), nil
}

func loadRemoteState(remote string) (remoteState, error) {
	path, err := remoteStateFilePath(remote)
	if err != nil {
		return remoteState{}, err
	}
	data, err := os.ReadFile(path)
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

func saveRemoteState(remote string, state remoteState) error {
	dir, err := ensureCredentialDir()
	if err != nil {
		return err
	}
	state.Remote = normalizedRemoteName(remote)
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, remoteStateFilename(remote))
	return os.WriteFile(path, payload, 0o600)
}

func updateRemoteState(remote string, mutator func(*remoteState)) error {
	state, err := loadRemoteState(remote)
	if err != nil {
		return err
	}
	mutator(&state)
	return saveRemoteState(remote, state)
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

func recordMountAttach(remote, mountPoint, remotePath string) {
	abs := filepath.Clean(mountPoint)
	now := time.Now()
	err := updateRemoteState(remote, func(state *remoteState) {
		for i := range state.Mounts {
			if sameMountPoint(state.Mounts[i].MountPoint, abs) {
				state.Mounts[i].MountPoint = abs
				state.Mounts[i].RemotePath = remotePath
				state.Mounts[i].Attached = true
				state.Mounts[i].LastUpdated = now
				return
			}
		}
		state.Mounts = append(state.Mounts, mountState{
			MountPoint:  abs,
			RemotePath:  remotePath,
			Attached:    true,
			LastUpdated: now,
		})
	})
	if err != nil {
		logStateWarning(err)
	}
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
	target := filepath.Clean(mountPoint)
	switch runtime.GOOS {
	case "linux", "android":
		return checkProcMounts(target)
	case "darwin":
		return checkDarwinMounts(target)
	default:
		return false, fmt.Errorf("mount detection is not implemented on %s", runtime.GOOS)
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
		mountPath := decodeProcMountField(fields[1])
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
	out, err := exec.Command("mount").Output()
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
		mountPath := strings.TrimSpace(right[:idx])
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

	raw, err := json.Marshal(creds)
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

func parseCommandFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandUsage(fs)
			return flag.ErrHelp
		}
		return err
	}
	return nil
}

func printCommandUsage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stdout, "Usage of 'protondrive %s':\n", fs.Name())
	fs.SetOutput(os.Stdout)
	fs.PrintDefaults()
	fmt.Println()
}

func ensureRclone() error {
	if _, err := exec.LookPath("rclone"); err != nil {
		return errors.New("rclone not found in PATH. Install it from https://rclone.org/install/")
	}
	return nil
}

func runRcloneCapture(args ...string) (string, error) {
	cmd := exec.Command("rclone", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("rclone %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func streamRclone(args ...string) error {
	cmd := exec.Command("rclone", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type retryOptions struct {
	enabled        bool
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

func runRcloneWithRetry(cmd []string, opts retryOptions) error {
	command := fmt.Sprintf("rclone %s", strings.Join(cmd, " "))
	maxAttempts := opts.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	initial := opts.initialBackoff
	if initial <= 0 {
		initial = defaultBackoffInitial
	}
	maxBackoff := opts.maxBackoff
	if maxBackoff <= 0 {
		maxBackoff = initial
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}
	if !opts.enabled || maxAttempts == 1 {
		fmt.Printf("Running: %s\n", command)
		return streamRclone(cmd...)
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		fmt.Printf("Running: %s (attempt %d/%d)\n", command, attempt, maxAttempts)
		err := streamRclone(cmd...)
		if err == nil {
			if attempt > 1 {
				fmt.Printf("Command completed after %d attempt(s).\n", attempt)
			}
			return nil
		}
		if attempt == maxAttempts {
			return fmt.Errorf("%s failed after %d attempt(s): %w", command, attempt, err)
		}
		backoff := computeBackoffDelay(initial, maxBackoff, attempt)
		fmt.Fprintf(os.Stderr, "Attempt %d/%d failed: %v. Retrying in %s...\n", attempt, maxAttempts, err, backoff)
		time.Sleep(backoff)
	}
	return nil
}

func computeBackoffDelay(initial, max time.Duration, failures int) time.Duration {
	if failures <= 0 {
		failures = 1
	}
	if initial <= 0 {
		initial = defaultBackoffInitial
	}
	if max <= 0 {
		max = initial
	}
	if max < initial {
		max = initial
	}
	delay := initial
	for i := 1; i < failures; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
}

func watchAndSync(localPath string, debounce time.Duration, run func() error) error {
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := addRecursiveWatch(watcher, localPath); err != nil {
		return err
	}

	trigger := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Create != 0 {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						addRecursiveWatch(watcher, event.Name)
					}
				}
				if event.Op&fsnotify.Chmod != 0 {
					continue
				}
				select {
				case trigger <- struct{}{}:
				default:
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(os.Stderr, "Watcher error: %v\n", err)
			}
		}
	}()

	if err := run(); err != nil {
		return err
	}

	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-trigger:
			if timer == nil {
				timer = time.NewTimer(debounce)
				timerC = timer.C
				fmt.Printf("Change detected. Waiting %s before syncing...\n", debounce)
			} else {
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(debounce)
			}
		case <-timerC:
			timerC = nil
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			fmt.Println("Syncing after filesystem changes...")
			if err := run(); err != nil {
				return err
			}
			fmt.Println("Watching for more changes...")
		}
	}
}

func addRecursiveWatch(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if err := watcher.Add(path); err != nil {
			return err
		}
		return nil
	})
}

func normalizeRemote(remote string) string {
	return strings.TrimSuffix(remote, ":")
}

func remotePath(remote, path string) string {
	base := fmt.Sprintf("%s:", normalizeRemote(remote))
	path = strings.TrimSpace(path)
	if path == "" {
		return base
	}
	return base + strings.TrimLeft(path, "/")
}

func expandPath(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}

	remainder := strings.TrimPrefix(p, "~")
	if remainder == "" {
		return home
	}
	if remainder[0] != '/' && remainder[0] != '\\' {
		// Likely "~user" which we don't try to expand.
		return p
	}
	remainder = strings.TrimLeft(remainder, "/\\")
	return filepath.Join(home, remainder)
}

func promptLine(reader *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	text, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func promptPassword(prompt string) (string, error) {
	if term.IsTerminal(int(syscall.Stdin)) {
		fmt.Fprint(os.Stderr, prompt)
		pw, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(pw)), nil
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, prompt)
	text, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func readLine(reader *bufio.Reader) (string, error) {
	text, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func printUsage() {
	fmt.Println(`ProtonDrive CLI - manage ProtonDrive via rclone

Usage:
  protondrive [--remote name] <command> [options]

Commands:
  configure    Create or update the ProtonDrive rclone remote.
  status       Show remote availability (use --details for listing folders).
  browse       List directories (default) or files (--files) under a path.
  sync         Sync a local folder with ProtonDrive (upload or download).
  mount        Mount ProtonDrive via rclone mount.
  unmount      Unmount a ProtonDrive mount point.
  configs      List, show, or copy reusable sync config templates.

Use "protondrive <command> -h" for command-specific options.`)
}
