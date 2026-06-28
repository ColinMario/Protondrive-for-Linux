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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
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
	remoteDefault      = "protondrive"
	vaultPassphraseEnv = "PROTONDRIVE_VAULT_PASSPHRASE"
	backendEnv         = "PROTONDRIVE_BACKEND"
	protonDriveBinEnv  = "PROTONDRIVE_PROTON_BIN"
	rcloneBinEnv       = "PROTONDRIVE_RCLONE_BIN"

	backendAuto   = "auto"
	backendProton = "proton"
	backendRclone = "rclone"

	mountMethodAuto   = "auto"
	mountMethodFuse   = "fuse"
	mountMethodWebDAV = "webdav"

	protonDriveDefaultBin = "proton-drive"
	rcloneDefaultBin      = "rclone"

	protonCLISecretService = "ch.proton.drive/drive-sdk-cli"
	protonCLISecretName    = "auth-session"
)

var procMountReplacer = strings.NewReplacer(
	"\\040", " ",
	"\\011", "\t",
	"\\012", "\n",
	"\\134", "\\",
)

type runtimeOptions struct {
	Remote         string
	Backend        string
	ProtonDriveBin string
	RcloneBin      string
}

var currentOptions = defaultRuntimeOptions()

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

type syncConfig struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	LocalPath       string   `json:"local_path"`
	RemotePath      string   `json:"remote_path"`
	Direction       string   `json:"direction"`
	Watch           bool     `json:"watch"`
	WatchDebounce   string   `json:"watch_debounce"`
	ExtraRcloneArgs []string `json:"extra_rclone_args"`
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
	options, args, err := parseGlobalArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		printUsage()
		os.Exit(2)
	}
	currentOptions = options

	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	cmd := args[0]
	switch cmd {
	case "configure":
		err = runConfigure(options.Remote, args[1:])
	case "status":
		err = runStatus(options.Remote, args[1:])
	case "browse":
		err = runBrowse(options.Remote, args[1:])
	case "sync":
		err = runSync(options.Remote, args[1:])
	case "mount":
		err = runMount(options.Remote, args[1:])
	case "unmount":
		err = runUnmount(options.Remote, args[1:])
	case "configs":
		err = runConfigs(options.Remote, args[1:])
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

func defaultRuntimeOptions() runtimeOptions {
	backend := strings.TrimSpace(os.Getenv(backendEnv))
	if backend == "" {
		backend = backendAuto
	}
	protonBin := strings.TrimSpace(os.Getenv(protonDriveBinEnv))
	if protonBin == "" {
		protonBin = protonDriveDefaultBin
	}
	rcloneBin := strings.TrimSpace(os.Getenv(rcloneBinEnv))
	if rcloneBin == "" {
		rcloneBin = rcloneDefaultBin
	}
	return runtimeOptions{
		Remote:         remoteDefault,
		Backend:        backend,
		ProtonDriveBin: protonBin,
		RcloneBin:      rcloneBin,
	}
}

func parseGlobalArgs(args []string) (runtimeOptions, []string, error) {
	options := defaultRuntimeOptions()
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
				return runtimeOptions{}, nil, errors.New("missing value for --remote")
			}
			options.Remote = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--remote="):
			options.Remote = strings.TrimPrefix(arg, "--remote=")
			i++
		case arg == "--backend":
			if i+1 >= len(args) {
				return runtimeOptions{}, nil, errors.New("missing value for --backend")
			}
			options.Backend = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--backend="):
			options.Backend = strings.TrimPrefix(arg, "--backend=")
			i++
		case arg == "--proton-drive-bin":
			if i+1 >= len(args) {
				return runtimeOptions{}, nil, errors.New("missing value for --proton-drive-bin")
			}
			options.ProtonDriveBin = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--proton-drive-bin="):
			options.ProtonDriveBin = strings.TrimPrefix(arg, "--proton-drive-bin=")
			i++
		case arg == "--rclone-bin":
			if i+1 >= len(args) {
				return runtimeOptions{}, nil, errors.New("missing value for --rclone-bin")
			}
			options.RcloneBin = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--rclone-bin="):
			options.RcloneBin = strings.TrimPrefix(arg, "--rclone-bin=")
			i++
		default:
			return runtimeOptions{}, nil, fmt.Errorf("unknown global flag: %s", arg)
		}
	}
	backend, err := normalizeBackend(options.Backend)
	if err != nil {
		return runtimeOptions{}, nil, err
	}
	options.Backend = backend
	if strings.TrimSpace(options.Remote) == "" {
		options.Remote = remoteDefault
	}
	if strings.TrimSpace(options.ProtonDriveBin) == "" {
		options.ProtonDriveBin = protonDriveDefaultBin
	}
	if strings.TrimSpace(options.RcloneBin) == "" {
		options.RcloneBin = rcloneDefaultBin
	}
	return options, args[i:], nil
}

func normalizeBackend(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", backendAuto:
		return backendAuto, nil
	case backendProton, "proton-drive", "official":
		return backendProton, nil
	case backendRclone:
		return backendRclone, nil
	default:
		return "", fmt.Errorf("backend must be one of %s, %s, or %s", backendAuto, backendProton, backendRclone)
	}
}

func normalizeMountMethod(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", mountMethodAuto:
		return mountMethodAuto, nil
	case mountMethodFuse, "rclone":
		return mountMethodFuse, nil
	case mountMethodWebDAV, "web-dav":
		return mountMethodWebDAV, nil
	default:
		return "", fmt.Errorf("mount method must be one of %s, %s, or %s", mountMethodAuto, mountMethodFuse, mountMethodWebDAV)
	}
}

func chooseMountMethod(method string, foreground bool) string {
	if method != mountMethodAuto {
		return method
	}
	if runtime.GOOS == "darwin" && !foreground {
		return mountMethodWebDAV
	}
	return mountMethodFuse
}

func resolveBackend(command string, args []string) (string, error) {
	if command == "mount" && currentOptions.Backend == backendProton {
		return "", fmt.Errorf("the official Proton Drive CLI does not support mounting; use '--backend %s' for rclone mount support", backendRclone)
	}
	if currentOptions.Backend != backendAuto {
		return requireBackend(currentOptions.Backend)
	}

	if normalizeRemote(currentOptions.Remote) != remoteDefault {
		return requireBackend(backendRclone)
	}

	switch command {
	case "configure":
		if configureArgsRequireRclone(args) {
			return requireBackend(backendRclone)
		}
		if isBackendAvailable(backendProton) {
			return backendProton, nil
		}
		return requireBackend(backendRclone)
	case "status", "browse":
		if isBackendAvailable(backendProton) {
			return backendProton, nil
		}
		return requireBackend(backendRclone)
	case "mount":
		return requireBackend(backendRclone)
	default:
		if isBackendAvailable(backendProton) {
			return backendProton, nil
		}
		return requireBackend(backendRclone)
	}
}

func resolveSyncBackend(args []string, cfg *loadedSyncConfig, dryRun, noProgress bool, extra []string) (string, error) {
	if currentOptions.Backend != backendAuto {
		return requireBackend(currentOptions.Backend)
	}
	if normalizeRemote(currentOptions.Remote) != remoteDefault {
		return requireBackend(backendRclone)
	}
	if dryRun || noProgress || len(extra) > 0 || syncArgsRequireRclone(args) {
		return requireBackend(backendRclone)
	}
	if cfg != nil && len(cfg.Config.ExtraRcloneArgs) > 0 {
		return requireBackend(backendRclone)
	}
	if isBackendAvailable(backendProton) {
		return backendProton, nil
	}
	return requireBackend(backendRclone)
}

func configureArgsRequireRclone(args []string) bool {
	for _, arg := range args {
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		switch name {
		case "email", "password", "password-stdin", "twofa", "twofa-stdin", "non-interactive", "headless", "store-credentials", "vault-passphrase", "vault-passphrase-stdin", "from-proton-cli-session", "from-rclone-session":
			return true
		}
	}
	return false
}

func syncArgsRequireRclone(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return true
		}
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		switch name {
		case "dry-run", "no-progress":
			return true
		}
	}
	return false
}

func isBackendAvailable(name string) bool {
	bin := binaryForBackend(name)
	if strings.TrimSpace(bin) == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

func requireBackend(name string) (string, error) {
	switch name {
	case backendProton:
		if err := ensureProtonDrive(); err != nil {
			return "", err
		}
	case backendRclone:
		if err := ensureRclone(); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported backend %q", name)
	}
	return name, nil
}

func binaryForBackend(name string) string {
	switch name {
	case backendProton:
		return currentOptions.ProtonDriveBin
	case backendRclone:
		return currentOptions.RcloneBin
	default:
		return ""
	}
}

func runConfigure(remote string, args []string) error {
	backend, err := resolveBackend("configure", args)
	if err != nil {
		return err
	}
	if backend == backendProton {
		return runProtonConfigure(remote, args)
	}
	return runRcloneConfigure(remote, args)
}

func runProtonConfigure(remote string, args []string) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	headless := fs.Bool("headless", false, "Use browserless rclone authentication and write a Proton CLI session")
	skipVerify := fs.Bool("skip-verify", false, "Skip listing Proton Drive after browser login")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("configure does not accept positional arguments")
	}
	if *headless {
		return fmt.Errorf("browserless Proton CLI session creation uses rclone's password auth engine; omit '--backend %s' or use '--backend %s configure --headless --email ... --password-stdin'", backendProton, backendRclone)
	}

	fmt.Printf("Starting browser sign-in with %s...\n", currentOptions.ProtonDriveBin)
	if err := streamProtonDrive("auth", "login"); err != nil {
		recordAuthEvent(remote, "proton-auth-login", false, strings.TrimSpace(err.Error()))
		return err
	}
	if !*skipVerify {
		if _, err := runProtonDriveCapture("filesystem", "list", "/"); err != nil {
			recordAuthEvent(remote, "proton-auth-login", false, strings.TrimSpace(err.Error()))
			return fmt.Errorf("login completed but Proton Drive listing failed: %w", err)
		}
	}
	recordAuthEvent(remote, "proton-auth-login", true, "")
	fmt.Println("Official Proton Drive CLI authentication is ready.")
	return nil
}

func runRcloneConfigure(remote string, args []string) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	email := fs.String("email", "", "ProtonMail email address")
	password := fs.String("password", "", "ProtonMail password (use with caution)")
	passwordStdin := fs.Bool("password-stdin", false, "Read password from stdin")
	twofa := fs.String("twofa", "", "Optional 2FA code")
	twofaStdin := fs.Bool("twofa-stdin", false, "Read 2FA code from stdin")
	nonInteractive := fs.Bool("non-interactive", false, "Fail instead of prompting")
	headless := fs.Bool("headless", false, "Browserless setup: authenticate via rclone and write the official Proton CLI session to the OS secret store")
	skipVerify := fs.Bool("skip-verify", false, "Skip connection test after configuring")
	fromProtonCLISession := fs.Bool("from-proton-cli-session", false, "Import the official proton-drive CLI session into an rclone Proton Drive remote")
	fromRcloneSession := fs.Bool("from-rclone-session", false, "Export an existing rclone cached Proton Drive session into the official Proton CLI secret store")
	storeCreds := fs.Bool("store-credentials", false, "Encrypt credentials locally for automatic reauth")
	vaultPass := fs.String("vault-passphrase", "", "Passphrase used for --store-credentials (use with caution)")
	vaultPassStdin := fs.Bool("vault-passphrase-stdin", false, "Read vault passphrase from stdin")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *headless {
		*nonInteractive = true
	}

	if *fromProtonCLISession {
		if err := configureRemoteFromProtonCLISession(remote, !*skipVerify); err != nil {
			recordAuthEvent(remote, "proton-cli-session-import", false, strings.TrimSpace(err.Error()))
			return err
		}
		recordAuthEvent(remote, "proton-cli-session-import", true, "")
		return nil
	}
	if *fromRcloneSession {
		if err := configureProtonCLISessionFromRcloneRemote(remote, !*skipVerify); err != nil {
			recordAuthEvent(remote, "rclone-session-export", false, strings.TrimSpace(err.Error()))
			return err
		}
		recordAuthEvent(remote, "rclone-session-export", true, "")
		return nil
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

	if *headless {
		fmt.Println("Initializing browserless rclone session...")
		if err := verifyRemote(remote); err != nil {
			recordAuthEvent(remote, "headless-rclone-login", false, strings.TrimSpace(err.Error()))
			return fmt.Errorf("browserless rclone login failed: %w", err)
		}
		recordAuthEvent(remote, "headless-rclone-login", true, "")
		fmt.Println("Browserless rclone session verified.")

		if err := configureProtonCLISessionFromRcloneRemote(remote, !*skipVerify); err != nil {
			recordAuthEvent(remote, "headless-proton-cli-session", false, strings.TrimSpace(err.Error()))
			return err
		}
		recordAuthEvent(remote, "headless-proton-cli-session", true, "")
	} else if !*skipVerify {
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

	backend, err := resolveBackend("status", args)
	if err != nil {
		return err
	}
	if backend == backendProton {
		return runProtonStatus(remote, *details)
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

func runProtonStatus(remote string, details bool) error {
	version, err := runProtonDriveCapture("version")
	if err != nil {
		return err
	}
	fmt.Printf("Official Proton Drive CLI detected: %s\n", strings.TrimSpace(version))

	root, err := runProtonDriveCapture("filesystem", "list", "/")
	if err != nil {
		recordAuthEvent(remote, "proton-status", false, strings.TrimSpace(err.Error()))
		fmt.Println("Official Proton Drive CLI is installed, but it is not authenticated or cannot list Drive.")
		fmt.Printf("Run 'protondrive --backend %s configure' to sign in through Proton's browser login.\n", backendProton)
		return nil
	}

	recordAuthEvent(remote, "proton-status", true, "")
	fmt.Println("Official Proton Drive CLI is authenticated.")

	if details {
		printStatusDetails(remote, mustLoadRemoteState(remote), hasStoredCredentials(remote))
		fmt.Println("Top-level Proton Drive sections:")
		fmt.Println(strings.TrimSpace(root))
		fmt.Println("\nMy files:")
		data, err := runProtonDriveCapture("filesystem", "list", "-t", "folder", "/my-files")
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

func mustLoadRemoteState(remote string) remoteState {
	state, err := loadRemoteState(remote)
	if err != nil {
		logStateWarning(err)
		return remoteState{Remote: normalizedRemoteName(remote)}
	}
	return state
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
	if strings.TrimSpace(entry.Method) != "" {
		builder.WriteString(fmt.Sprintf("; method %s", entry.Method))
	}
	if strings.TrimSpace(entry.URL) != "" {
		builder.WriteString(fmt.Sprintf("; url %s", entry.URL))
	}
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
	remotePathFlag := fs.String("remote-path", "", "Remote path to inspect (defaults to /my-files for the Proton backend)")
	files := fs.Bool("files", false, "Show files instead of directories")
	all := fs.Bool("all", false, "Show all entry types when using the Proton backend")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	backend, err := resolveBackend("browse", args)
	if err != nil {
		return err
	}
	if backend == backendProton {
		return runProtonBrowse(remote, *remotePathFlag, *files, *all)
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

func runProtonBrowse(remote, path string, files, all bool) error {
	target := protonDrivePath(path, true)
	command := []string{"filesystem", "list"}
	if !all {
		entryType := "folder"
		if files {
			entryType = "file"
		}
		command = append(command, "-t", entryType)
	}
	command = append(command, target)

	data, err := runProtonDriveCapture(command...)
	if err != nil {
		recordAuthEvent(remote, "proton-browse", false, strings.TrimSpace(err.Error()))
		return err
	}
	recordAuthEvent(remote, "proton-browse", true, "")
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
	conflictStrategy := fs.String("conflict-strategy", "", "Proton backend conflict strategy: merge, keep-both, replace, or skip")
	fileConflictStrategy := fs.String("file-conflict-strategy", "", "Proton backend file conflict strategy")
	folderConflictStrategy := fs.String("folder-conflict-strategy", "", "Proton backend folder conflict strategy")
	skipThumbnails := fs.Bool("skip-thumbnails", false, "Proton backend: skip image thumbnail generation on upload")
	var watchFlag optionalBoolFlag
	fs.Var(&watchFlag, "watch", "Watch the local folder for changes (upload only)")
	watchDebounceFlag := optionalDurationFlag{value: 10 * time.Second}
	fs.Var(&watchDebounceFlag, "watch-debounce", "Minimum delay between syncs while watching (default 10s)")
	parseArgs := normalizeInterspersedFlags(args, map[string]bool{
		"remote-path":              true,
		"direction":                true,
		"config":                   true,
		"conflict-strategy":        true,
		"file-conflict-strategy":   true,
		"folder-conflict-strategy": true,
		"watch-debounce":           true,
		"dry-run":                  false,
		"no-progress":              false,
		"watch":                    false,
		"skip-thumbnails":          false,
	})
	if err := parseCommandFlags(fs, parseArgs); err != nil {
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

	backend, err := resolveSyncBackend(args, cfg, *dryRun, *noProgress, extra)
	if err != nil {
		return err
	}
	if backend == backendProton {
		options := protonSyncOptions{
			RemotePath:             remotePathValue,
			Direction:              dir,
			ConflictStrategy:       *conflictStrategy,
			FileConflictStrategy:   *fileConflictStrategy,
			FolderConflictStrategy: *folderConflictStrategy,
			SkipThumbnails:         *skipThumbnails,
			DryRun:                 *dryRun,
			NoProgress:             *noProgress,
			ExtraRcloneArgs:        extra,
		}
		if cfg != nil {
			options.ExtraRcloneArgs = append(options.ExtraRcloneArgs, cfg.Config.ExtraRcloneArgs...)
		}
		runOnce := func() error {
			return runProtonSync(remote, localAbs, options)
		}
		if watchEnabled {
			fmt.Printf("Watching %s for changes (debounce %s). Press Ctrl+C to stop.\n", localAbs, watchDebounce)
			return watchAndSync(localAbs, watchDebounce, runOnce)
		}
		return runOnce()
	}

	if hasProtonSyncFlags(*conflictStrategy, *fileConflictStrategy, *folderConflictStrategy, *skipThumbnails) {
		return fmt.Errorf("Proton conflict and thumbnail flags require '--backend %s'", backendProton)
	}
	if err := ensureRemoteAuth(remote); err != nil {
		return err
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

	runOnce := func() error {
		fmt.Printf("Running: rclone %s\n", strings.Join(cmd, " "))
		return streamRclone(cmd...)
	}
	if watchEnabled {
		fmt.Printf("Watching %s for changes (debounce %s). Press Ctrl+C to stop.\n", localAbs, watchDebounce)
		return watchAndSync(localAbs, watchDebounce, runOnce)
	}
	return runOnce()
}

func normalizeInterspersedFlags(args []string, flagsWithValues map[string]bool) []string {
	var flagArgs []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		name, hasInlineValue, ok := parseLongFlagName(arg)
		takesValue, known := flagsWithValues[name]
		if !ok || !known {
			positional = append(positional, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		if takesValue && !hasInlineValue && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return append(flagArgs, positional...)
}

func parseLongFlagName(arg string) (name string, hasInlineValue bool, ok bool) {
	if !strings.HasPrefix(arg, "--") || arg == "--" {
		return "", false, false
	}
	value := strings.TrimPrefix(arg, "--")
	if value == "" {
		return "", false, false
	}
	name, _, hasInlineValue = strings.Cut(value, "=")
	if strings.TrimSpace(name) == "" {
		return "", false, false
	}
	return name, hasInlineValue, true
}

type protonSyncOptions struct {
	RemotePath             string
	Direction              string
	ConflictStrategy       string
	FileConflictStrategy   string
	FolderConflictStrategy string
	SkipThumbnails         bool
	DryRun                 bool
	NoProgress             bool
	ExtraRcloneArgs        []string
}

func runProtonSync(remote, localAbs string, options protonSyncOptions) error {
	if options.DryRun {
		return fmt.Errorf("--dry-run is not supported by the official Proton Drive CLI backend; use '--backend %s' for rclone dry-runs", backendRclone)
	}
	if options.NoProgress {
		return fmt.Errorf("--no-progress is rclone-specific; omit it or use '--backend %s'", backendRclone)
	}
	if len(options.ExtraRcloneArgs) > 0 {
		return fmt.Errorf("extra rclone arguments are not supported by the official Proton Drive CLI backend; use '--backend %s'", backendRclone)
	}
	if options.Direction == "download" && options.SkipThumbnails {
		return errors.New("--skip-thumbnails is only valid for upload with the official Proton Drive CLI backend")
	}
	if err := validateConflictStrategy("conflict-strategy", options.ConflictStrategy, true); err != nil {
		return err
	}
	if err := validateConflictStrategy("file-conflict-strategy", options.FileConflictStrategy, options.Direction == "upload"); err != nil {
		return err
	}
	if err := validateConflictStrategy("folder-conflict-strategy", options.FolderConflictStrategy, true); err != nil {
		return err
	}

	target := protonDrivePath(options.RemotePath, true)
	cmd := []string{"filesystem"}
	switch options.Direction {
	case "upload":
		cmd = append(cmd, "upload")
		cmd = appendProtonConflictFlags(cmd, options)
		if options.SkipThumbnails {
			cmd = append(cmd, "--skip-thumbnails")
		}
		cmd = append(cmd, localAbs, target)
		fmt.Printf("Running: %s %s\n", currentOptions.ProtonDriveBin, strings.Join(cmd, " "))
	case "download":
		cmd = append(cmd, "download")
		cmd = appendProtonConflictFlags(cmd, options)
		cmd = append(cmd, target, localAbs)
		fmt.Printf("Running: %s %s\n", currentOptions.ProtonDriveBin, strings.Join(cmd, " "))
	default:
		return errors.New("direction must be 'upload' or 'download'")
	}

	if err := streamProtonDrive(cmd...); err != nil {
		recordAuthEvent(remote, "proton-sync", false, strings.TrimSpace(err.Error()))
		return err
	}
	recordAuthEvent(remote, "proton-sync", true, "")
	return nil
}

func appendProtonConflictFlags(cmd []string, options protonSyncOptions) []string {
	if strings.TrimSpace(options.ConflictStrategy) != "" {
		cmd = append(cmd, "--conflict-strategy", strings.TrimSpace(options.ConflictStrategy))
	}
	if strings.TrimSpace(options.FileConflictStrategy) != "" {
		cmd = append(cmd, "--file-conflict-strategy", strings.TrimSpace(options.FileConflictStrategy))
	}
	if strings.TrimSpace(options.FolderConflictStrategy) != "" {
		cmd = append(cmd, "--folder-conflict-strategy", strings.TrimSpace(options.FolderConflictStrategy))
	}
	return cmd
}

func validateConflictStrategy(name, value string, allowMerge bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	switch value {
	case "keep-both", "replace", "skip":
		return nil
	case "merge":
		if allowMerge {
			return nil
		}
	}
	if allowMerge {
		return fmt.Errorf("%s must be one of merge, keep-both, replace, or skip", name)
	}
	return fmt.Errorf("%s must be one of keep-both, replace, or skip for downloads", name)
}

func hasProtonSyncFlags(conflict, fileConflict, folderConflict string, skipThumbnails bool) bool {
	return strings.TrimSpace(conflict) != "" ||
		strings.TrimSpace(fileConflict) != "" ||
		strings.TrimSpace(folderConflict) != "" ||
		skipThumbnails
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
	mountMethodFlag := fs.String("mount-method", mountMethodAuto, "Mount method: auto, fuse, or webdav (macOS auto uses webdav to avoid macFUSE)")
	persist := fs.Bool("persist", false, "Install and start a persistent Linux systemd user mount service")
	persistName := fs.String("persist-name", "", "Name suffix for the persistent systemd user mount service")
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
	mountPoint := expandPath(remaining[0])
	extra := remaining[1:]

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("unable to create mount point '%s': %w", mountPoint, err)
	}

	mountMethod, err := normalizeMountMethod(*mountMethodFlag)
	if err != nil {
		return err
	}
	mountMethod = chooseMountMethod(mountMethod, *foreground)
	if *persist {
		if *foreground {
			return errors.New("--persist manages the mount as a foreground systemd service; do not combine it with --foreground")
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
			EnableLinger: *enableLinger,
			RcloneBin:    currentOptions.RcloneBin,
		}
		return installPersistentMount(options)
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
	recordMountAttach(remote, mountPoint, target, mountMethodFuse, 0, "")
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
	if _, err := exec.LookPath("mount_webdav"); err != nil {
		return errors.New("mount_webdav not found; macOS WebDAV mounting is unavailable")
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

	fmt.Printf("Serving %s over local WebDAV at %s for macOS mount.\n", target, url)
	cmd := exec.Command(currentOptions.RcloneBin, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start rclone WebDAV server: %w", err)
	}
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
	}

	if err := waitForWebDAV(url, readyTimeout, serverDone); err != nil {
		cleanupServer()
		return err
	}

	mountCmd := exec.Command("mount_webdav", url, mountPoint)
	mountCmd.Stdout = os.Stdout
	mountCmd.Stderr = os.Stderr
	if err := mountCmd.Run(); err != nil {
		cleanupServer()
		return mountErrorWithHints(target, mountPoint, readyTimeout, err, true)
	}

	recordMountAttach(remote, mountPoint, target, mountMethodWebDAV, serverPID, url)
	fmt.Printf("Mount ready at %s via local WebDAV. Use 'protondrive unmount %s' to detach.\n", mountPoint, mountPoint)
	return nil
}

func reserveLocalTCPAddr() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("unable to reserve localhost port for WebDAV mount: %w", err)
	}
	defer listener.Close()
	return listener.Addr().String(), nil
}

func waitForWebDAV(url string, timeout time.Duration, serverDone <-chan error) error {
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
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("rclone WebDAV server did not become ready within %s: %w", timeout, lastErr)
	}
	return fmt.Errorf("rclone WebDAV server did not become ready within %s", timeout)
}

func stopProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		_ = proc.Kill()
		return err
	}
	return nil
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
	EnableLinger bool
	RcloneBin    string
}

func installPersistentMount(options persistentMountOptions) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("--persist currently installs systemd user units on Linux only (current OS: %s)", runtime.GOOS)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found; persistent mounts require systemd --user")
	}

	serviceName := persistentMountServiceName(options.Remote, options.MountPoint, options.PersistName)
	unitDir, scriptDir, err := persistentMountDirs()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
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

	startArgs := persistentMountStartArgs(exe, rcloneBin, options)
	if err := os.WriteFile(startScript, []byte(shellScript(startArgs)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(stopScript, []byte(unmountShellScript(options.MountPoint)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(systemdMountUnit(serviceName, startScript, stopScript, options.MountPoint)), 0o644); err != nil {
		return err
	}

	if err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runCommand("systemctl", "--user", "enable", "--now", serviceName); err != nil {
		return err
	}
	if options.EnableLinger {
		enableUserLinger()
	}

	fmt.Printf("Persistent mount service installed and started: %s\n", serviceName)
	fmt.Printf("Unit: %s\n", unitPath)
	fmt.Printf("Mount point: %s\n", options.MountPoint)
	if !options.EnableLinger {
		fmt.Println("For boot-time mounts before login, run again with --enable-linger or enable lingering with loginctl.")
	}
	return nil
}

func removePersistentMount(remote, mountPoint, persistName string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("--remove-persist currently manages Linux systemd user units only (current OS: %s)", runtime.GOOS)
	}
	serviceName := persistentMountServiceName(remote, mountPoint, persistName)
	unitDir, scriptDir, err := persistentMountDirs()
	if err != nil {
		return err
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", serviceName).Run()
	_ = os.Remove(filepath.Join(unitDir, serviceName))
	baseName := strings.TrimSuffix(serviceName, ".service")
	_ = os.Remove(filepath.Join(scriptDir, baseName+".sh"))
	_ = os.Remove(filepath.Join(scriptDir, baseName+"-stop.sh"))
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Printf("Persistent mount service removed: %s\n", serviceName)
	return nil
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
	name := strings.TrimSpace(persistName)
	if name == "" {
		name = normalizedRemoteName(remote) + "-" + filepath.Base(filepath.Clean(mountPoint))
	}
	return "protondrive-mount-" + slugifyConfigName(name) + ".service"
}

func persistentMountDirs() (unitDir string, scriptDir string, err error) {
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

func shellScript(args []string) string {
	var builder strings.Builder
	builder.WriteString("#!/bin/sh\nset -eu\n")
	if rcloneConfig := strings.TrimSpace(os.Getenv("RCLONE_CONFIG")); rcloneConfig != "" {
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

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func systemdQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func enableUserLinger() {
	userName := strings.TrimSpace(os.Getenv("USER"))
	if userName == "" {
		fmt.Fprintln(os.Stderr, "Warning: unable to enable lingering because USER is empty")
		return
	}
	if _, err := exec.LookPath("loginctl"); err != nil {
		fmt.Fprintln(os.Stderr, "Warning: loginctl not found; cannot enable lingering automatically")
		return
	}
	if err := runCommand("loginctl", "enable-linger", userName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: unable to enable lingering automatically: %v\n", err)
		return
	}
	fmt.Printf("Enabled systemd lingering for user %s.\n", userName)
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
	removePersist := fs.Bool("remove-persist", false, "Disable and remove the persistent Linux systemd user mount service")
	persistName := fs.String("persist-name", "", "Name suffix for the persistent systemd user mount service")
	parseArgs := normalizeInterspersedFlags(args, map[string]bool{
		"force":          false,
		"remove-persist": false,
		"persist-name":   true,
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
	mountPoint := expandPath(remaining[0])

	if *removePersist {
		if err := removePersistentMount(remote, mountPoint, *persistName); err != nil {
			return err
		}
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
		if _, err := exec.LookPath(candidate[0]); err != nil {
			continue
		}
		tried = append(tried, strings.Join(candidate, " "))
		cmd := exec.Command(candidate[0], candidate[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			fmt.Printf("Unmounted %s.\n", mountPoint)
			if err := stopRecordedMountProcess(remote, mountPoint); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: unable to stop recorded mount helper: %v\n", err)
			}
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

func stopRecordedMountProcess(remote, mountPoint string) error {
	state, err := loadRemoteState(remote)
	if err != nil {
		return err
	}
	abs := filepath.Clean(mountPoint)
	for _, entry := range state.Mounts {
		if sameMountPoint(entry.MountPoint, abs) && entry.ProcessID > 0 {
			return stopProcess(entry.ProcessID)
		}
	}
	return nil
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
	exec.Command(currentOptions.RcloneBin, "config", "delete", remote).Run()

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

type protonCLISessionSnapshot struct {
	CachePassword   string `json:"cachePassword"`
	UserKeyPassword string `json:"userKeyPassword"`
	Session         struct {
		UID          string `json:"uid"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	} `json:"session"`
}

func configureRemoteFromProtonCLISession(remote string, verify bool) error {
	snapshot, err := loadProtonCLISessionSnapshot()
	if err != nil {
		return err
	}
	if strings.TrimSpace(snapshot.UserKeyPassword) == "" ||
		strings.TrimSpace(snapshot.Session.UID) == "" ||
		strings.TrimSpace(snapshot.Session.AccessToken) == "" ||
		strings.TrimSpace(snapshot.Session.RefreshToken) == "" {
		return errors.New("official Proton Drive CLI session is incomplete; run 'protondrive --backend proton configure' first")
	}

	obscuredPlaceholder, err := runRcloneCapture("obscure", "proton-cli-session-placeholder")
	if err != nil {
		return fmt.Errorf("failed to prepare rclone placeholder password: %w", err)
	}
	configPath, err := rcloneConfigFilePath()
	if err != nil {
		return err
	}
	values := map[string]string{
		"type":                   "protondrive",
		"username":               "proton-cli-session",
		"password":               strings.TrimSpace(obscuredPlaceholder),
		"client_uid":             snapshot.Session.UID,
		"client_access_token":    snapshot.Session.AccessToken,
		"client_refresh_token":   snapshot.Session.RefreshToken,
		"client_salted_key_pass": base64.StdEncoding.EncodeToString([]byte(snapshot.UserKeyPassword)),
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
			return fmt.Errorf("imported session could not be verified: %w", err)
		}
		fmt.Println("Imported rclone session verified.")
	}
	return nil
}

func configureProtonCLISessionFromRcloneRemote(remote string, verify bool) error {
	snapshot, err := protonCLISessionFromRcloneRemote(remote)
	if err != nil {
		return err
	}
	target, err := writeProtonCLISessionSnapshot(snapshot)
	if err != nil {
		return err
	}
	fmt.Printf("Wrote official Proton Drive CLI session to %s.\n", target)
	if verify {
		if err := ensureProtonDrive(); err != nil {
			return fmt.Errorf("Proton CLI session was written but could not be verified: %w", err)
		}
		fmt.Println("Verifying Proton CLI session...")
		if _, err := runProtonDriveCapture("filesystem", "list", "/"); err != nil {
			return fmt.Errorf("Proton CLI session was written but verification failed: %w", err)
		}
		fmt.Println("Browserless Proton CLI session verified.")
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
	var raw []byte
	var err error
	switch runtime.GOOS {
	case "darwin":
		raw, err = exec.Command("security", "find-generic-password", "-s", protonCLISecretService, "-a", protonCLISecretName, "-w").Output()
	case "linux":
		raw, err = loadProtonCLISessionFromSecretTool()
	default:
		err = fmt.Errorf("importing the official Proton Drive CLI session is not implemented on %s", runtime.GOOS)
	}
	if err != nil {
		return protonCLISessionSnapshot{}, fmt.Errorf("unable to read official Proton Drive CLI session; run 'protondrive --backend proton configure' first: %w", err)
	}
	var snapshot protonCLISessionSnapshot
	if err := json.Unmarshal(bytes.TrimSpace(raw), &snapshot); err != nil {
		return protonCLISessionSnapshot{}, fmt.Errorf("official Proton Drive CLI session has unexpected format: %w", err)
	}
	return snapshot, nil
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
	var lastErr error
	for _, args := range candidates {
		out, err := exec.Command("secret-tool", args...).Output()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			return out, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("session secret not found")
	}
	return nil, lastErr
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
		}
		args := []string{"add-generic-password", "-U", "-s", protonCLISecretService, "-a", protonCLISecretName}
		if protonDriveBin, err := exec.LookPath(currentOptions.ProtonDriveBin); err == nil {
			args = append(args, "-T", protonDriveBin)
		}
		args = append(args, "-w", string(payload))
		cmd := exec.Command("security", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("unable to write Proton CLI session to macOS Keychain: %s", strings.TrimSpace(string(output)))
		}
		return fmt.Sprintf("macOS Keychain (%s/%s)", protonCLISecretService, protonCLISecretName), nil
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return "", errors.New("secret-tool not found; install libsecret-tools to write the Proton CLI session, or set PROTON_DRIVE_UNSAFE_SECRETS=true with PROTON_DRIVE_CACHE_DIR for a plaintext file session")
		}
		cmd := exec.Command("secret-tool", "store", "--label", "Proton Drive CLI session", "service", protonCLISecretService, "name", protonCLISecretName)
		cmd.Stdin = bytes.NewReader(payload)
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("unable to write Proton CLI session to Secret Service: %s", strings.TrimSpace(string(output)))
		}
		return fmt.Sprintf("Secret Service (%s/%s)", protonCLISecretService, protonCLISecretName), nil
	default:
		return "", fmt.Errorf("writing the Proton CLI session is not implemented on %s", runtime.GOOS)
	}
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
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		return err
	}
	script := fmt.Sprintf(`const value = await Bun.file(process.argv[2]).text();
await Bun.secrets.set({ service: %q, name: %q, value });
`, protonCLISecretService, protonCLISecretName)
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return err
	}
	cmd := exec.Command(bunPath, scriptPath, payloadPath)
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
	if err := os.WriteFile(path, payload, 0o600); err != nil {
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

func readRcloneConfigSection(configPath, section string) (map[string]string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
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
	if strings.TrimSpace(section) == "" {
		return errors.New("rclone config section cannot be empty")
	}
	var lines []string
	if data, err := os.ReadFile(configPath); err == nil {
		lines = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	header := "[" + section + "]"
	var out []string
	inTarget := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inTarget = trimmed == header
			if inTarget {
				continue
			}
		}
		if inTarget {
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
	for _, key := range []string{
		"type",
		"username",
		"password",
		"client_uid",
		"client_access_token",
		"client_refresh_token",
		"client_salted_key_pass",
		"enable_caching",
	} {
		out = append(out, fmt.Sprintf("%s = %s", key, sanitizeConfigValue(values[key])))
	}
	payload := strings.Join(out, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(configPath, []byte(payload), 0o600); err != nil {
		return err
	}
	return os.Chmod(configPath, 0o600)
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
	Method      string    `json:"method,omitempty"`
	ProcessID   int       `json:"process_id,omitempty"`
	URL         string    `json:"url,omitempty"`
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

func recordMountAttach(remote, mountPoint, remotePath, method string, processID int, url string) {
	abs := filepath.Clean(mountPoint)
	now := time.Now()
	err := updateRemoteState(remote, func(state *remoteState) {
		for i := range state.Mounts {
			if sameMountPoint(state.Mounts[i].MountPoint, abs) {
				state.Mounts[i].MountPoint = abs
				state.Mounts[i].RemotePath = remotePath
				state.Mounts[i].Method = method
				state.Mounts[i].ProcessID = processID
				state.Mounts[i].URL = url
				state.Mounts[i].Attached = true
				state.Mounts[i].LastUpdated = now
				return
			}
		}
		state.Mounts = append(state.Mounts, mountState{
			MountPoint:  abs,
			RemotePath:  remotePath,
			Method:      method,
			ProcessID:   processID,
			URL:         url,
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
	if _, err := exec.LookPath(currentOptions.RcloneBin); err != nil {
		return fmt.Errorf("%s not found in PATH. Install rclone from https://rclone.org/install/ or set --rclone-bin", currentOptions.RcloneBin)
	}
	return nil
}

func ensureProtonDrive() error {
	if _, err := exec.LookPath(currentOptions.ProtonDriveBin); err != nil {
		return fmt.Errorf("%s not found in PATH. Install the official Proton Drive CLI from https://proton.me/download/drive/cli/index.html or set --proton-drive-bin", currentOptions.ProtonDriveBin)
	}
	return nil
}

func runRcloneCapture(args ...string) (string, error) {
	cmd := exec.Command(currentOptions.RcloneBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %s", currentOptions.RcloneBin, strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func streamRclone(args ...string) error {
	cmd := exec.Command(currentOptions.RcloneBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runProtonDriveCapture(args ...string) (string, error) {
	cmd := exec.Command(currentOptions.ProtonDriveBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %s", currentOptions.ProtonDriveBin, strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func streamProtonDrive(args ...string) error {
	cmd := exec.Command(currentOptions.ProtonDriveBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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

func protonDrivePath(remotePathValue string, defaultMyFiles bool) string {
	remotePathValue = strings.TrimSpace(remotePathValue)
	if remotePathValue == "" {
		if defaultMyFiles {
			return "/my-files"
		}
		return "/"
	}
	remotePathValue = filepath.ToSlash(remotePathValue)
	if strings.HasPrefix(remotePathValue, "/") {
		return path.Clean(remotePathValue)
	}
	clean := path.Clean("/" + strings.TrimLeft(remotePathValue, "/"))
	switch {
	case clean == "/my-files", strings.HasPrefix(clean, "/my-files/"):
		return clean
	case clean == "/shared-with-me", strings.HasPrefix(clean, "/shared-with-me/"):
		return clean
	case clean == "/shared-by-me", strings.HasPrefix(clean, "/shared-by-me/"):
		return clean
	case clean == "/trash", strings.HasPrefix(clean, "/trash/"):
		return clean
	case !defaultMyFiles:
		return clean
	default:
		return path.Clean("/my-files/" + strings.TrimLeft(remotePathValue, "/"))
	}
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
	fmt.Println(`ProtonDrive CLI - manage Proton Drive from Linux/POSIX shells

Usage:
  protondrive [--backend auto|proton|rclone] [--remote name] <command> [options]

Backends:
  auto      Prefer the official proton-drive CLI where supported, fall back to rclone when needed.
  proton    Use Proton's official proton-drive CLI (auth, browse, upload/download workflows).
  rclone    Use rclone's Proton Drive backend (required for mount and exact rclone sync flags).

Global options:
  --backend name          Backend selection (default: auto; env PROTONDRIVE_BACKEND).
  --remote name           rclone remote name (default: protondrive; selecting a custom remote uses rclone).
  --proton-drive-bin path Official Proton Drive CLI binary (default: proton-drive; env PROTONDRIVE_PROTON_BIN).
  --rclone-bin path       rclone binary (default: rclone; env PROTONDRIVE_RCLONE_BIN).

Commands:
  configure    Sign in with Proton's CLI or create/update an rclone remote.
  status       Show backend availability and authentication status.
  browse       List directories (default) or files (--files) under a path.
  sync         Sync a local folder with ProtonDrive (upload or download).
  mount        Mount ProtonDrive via rclone (Linux FUSE, macOS WebDAV fallback, optional --persist).
  unmount      Unmount a ProtonDrive mount point (optional --remove-persist).
  configs      List, show, or copy reusable sync config templates.

Use "protondrive <command> -h" for command-specific options.`)
}
