package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ColinMario/Protondrive-for-Linux/internal/syncconfig"
)

const (
	remoteDefault      = "protondrive"
	vaultPassphraseEnv = "PROTONDRIVE_VAULT_PASSPHRASE" // #nosec G101 -- environment variable name, not a hardcoded credential
	backendEnv         = "PROTONDRIVE_BACKEND"
	protonDriveBinEnv  = "PROTONDRIVE_PROTON_BIN"
	rcloneBinEnv       = "PROTONDRIVE_RCLONE_BIN"
	managedBinDirEnv   = "PROTONDRIVE_MANAGED_BIN_DIR"

	backendAuto   = "auto"
	backendProton = "proton"
	backendRclone = "rclone"

	mountMethodAuto   = "auto"
	mountMethodFuse   = "fuse"
	mountMethodWebDAV = "webdav"

	persistentMountManagerAuto    = "auto"
	persistentMountManagerSystemd = "systemd"
	persistentMountManagerOpenRC  = "openrc"

	protonDriveDefaultBin = "proton-drive"
	rcloneDefaultBin      = "rclone"

	protonCLISecretService = "ch.proton.drive/drive-sdk-cli" // #nosec G101 -- Secret Service identifier, not a hardcoded credential
	protonCLISecretName    = "auth-session"

	protonCLIDownloadIndex = "https://proton.me/download/drive/cli/index.html"
	rcloneVersionURL       = "https://downloads.rclone.org/version.txt"
	rcloneGitHubReleaseURL = "https://github.com/rclone/rclone/releases/download"

	maxDependencyDownloadBytes = 512 << 20
	dependencyDownloadTimeout  = 30 * time.Minute
	defaultMirrorMaxDelete     = 25
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
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

type syncConfig = syncconfig.Config

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
		if silent, ok := err.(interface{ Silent() bool }); !ok || !silent.Silent() {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
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
	case "bootstrap":
		err = runBootstrap(args[1:])
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
	case "version":
		err = runVersion(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		if silent, ok := err.(interface{ Silent() bool }); !ok || !silent.Silent() {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		if coded, ok := err.(interface{ ExitCode() int }); ok {
			os.Exit(coded.ExitCode())
		}
		os.Exit(1)
	}
}

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Print machine-readable version information")
	if err := parseCommandFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("version does not accept positional arguments")
	}
	info := struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Date    string `json:"build_date"`
		Go      string `json:"go_version"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}{version, commit, buildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH}
	if *jsonOutput {
		payload, err := json.Marshal(info)
		if err != nil {
			return err
		}
		fmt.Println(string(payload))
		return nil
	}
	fmt.Printf("protondrive %s (commit %s, built %s, %s %s/%s)\n", info.Version, info.Commit, info.Date, info.Go, info.OS, info.Arch)
	return nil
}

func defaultRuntimeOptions() runtimeOptions {
	backend := strings.TrimSpace(os.Getenv(backendEnv))
	if backend == "" {
		backend = backendAuto
	}
	return runtimeOptions{
		Remote:         remoteDefault,
		Backend:        backend,
		ProtonDriveBin: defaultExternalBinary(protonDriveBinEnv, protonDriveDefaultBin),
		RcloneBin:      defaultExternalBinary(rcloneBinEnv, rcloneDefaultBin),
	}
}

func defaultExternalBinary(envName, defaultName string) string {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value
	}
	if _, err := exec.LookPath(defaultName); err == nil {
		return defaultName
	}
	if hostCommandAvailable(defaultName) {
		return defaultName
	}
	if managed, ok := managedBinaryPath(defaultName); ok && isExecutable(managed) {
		return managed
	}
	return defaultName
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
	remote, err := validateRemoteName(options.Remote)
	if err != nil {
		return runtimeOptions{}, nil, err
	}
	options.Remote = remote
	if strings.TrimSpace(options.ProtonDriveBin) == "" {
		options.ProtonDriveBin = protonDriveDefaultBin
	}
	if strings.TrimSpace(options.RcloneBin) == "" {
		options.RcloneBin = rcloneDefaultBin
	}
	return options, args[i:], nil
}

func validateRemoteName(value string) (string, error) {
	name := strings.TrimSpace(value)
	name = strings.TrimSuffix(name, ":")
	if name == "" {
		return "", errors.New("remote name cannot be empty")
	}
	if strings.ContainsAny(name, ":[]\r\n") {
		return "", errors.New("remote name must not contain colons, brackets, or line breaks")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("remote name must not contain control characters")
		}
	}
	return name, nil
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
	if cfg != nil {
		if len(cfg.Config.ExtraRcloneArgs) > 0 || strings.EqualFold(strings.TrimSpace(cfg.Config.Operation), syncconfig.OperationMirror) {
			return requireBackend(backendRclone)
		}
	}
	if isBackendAvailable(backendProton) {
		return backendProton, nil
	}
	if err := ensureProtonDrive(); err != nil {
		return "", fmt.Errorf("%w; auto sync does not fall back to rclone because rclone sync mirrors deletions. Install proton-drive or explicitly pass '--backend %s' if mirror semantics are intended", err, backendRclone)
	}
	return backendProton, nil
}

func configureArgsRequireRclone(args []string) bool {
	for _, arg := range args {
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		switch name {
		case "email", "password", "password-stdin", "mailbox-password", "mailbox-password-stdin", "twofa", "twofa-stdin", "non-interactive", "headless", "store-credentials", "vault-passphrase", "vault-passphrase-stdin", "from-proton-cli-session", "from-rclone-session":
			return true
		}
	}
	return false
}

func syncArgsRequireRclone(args []string) bool {
	for i, arg := range args {
		if arg == "--" {
			return true
		}
		parts := strings.SplitN(arg, "=", 2)
		name := strings.TrimLeft(parts[0], "-")
		switch name {
		case "dry-run", "no-progress", "confirm-mirror", "max-delete", "backup-dir", "no-backup-dir", "source-sentinel", "allow-empty-source", "allow-root-mirror":
			return true
		case "operation":
			value := ""
			if len(parts) == 2 {
				value = parts[1]
			} else if i+1 < len(args) {
				value = args[i+1]
			}
			if strings.EqualFold(strings.TrimSpace(value), syncconfig.OperationMirror) {
				return true
			}
		}
	}
	return false
}

func isBackendAvailable(name string) bool {
	bin := binaryForBackend(name)
	if strings.TrimSpace(bin) == "" {
		return false
	}
	return externalCommandAvailable(bin)
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
	mailboxPassword := fs.String("mailbox-password", "", "Optional mailbox password for two-password Proton accounts")
	mailboxPasswordStdin := fs.Bool("mailbox-password-stdin", false, "Read mailbox password from stdin")
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
	if fs.NArg() != 0 {
		return errors.New("configure does not accept positional arguments")
	}
	if *passwordStdin && strings.TrimSpace(*password) != "" {
		return errors.New("use either --password or --password-stdin, not both")
	}
	if *mailboxPasswordStdin && strings.TrimSpace(*mailboxPassword) != "" {
		return errors.New("use either --mailbox-password or --mailbox-password-stdin, not both")
	}
	if *twofaStdin && strings.TrimSpace(*twofa) != "" {
		return errors.New("use either --twofa or --twofa-stdin, not both")
	}
	if *vaultPassStdin && strings.TrimSpace(*vaultPass) != "" {
		return errors.New("use either --vault-passphrase or --vault-passphrase-stdin, not both")
	}
	if !*storeCreds && (*vaultPassStdin || strings.TrimSpace(*vaultPass) != "") {
		return errors.New("vault passphrase flags require --store-credentials")
	}
	if *fromProtonCLISession && *fromRcloneSession {
		return errors.New("use only one session import direction at a time")
	}
	if *fromProtonCLISession || *fromRcloneSession {
		if *headless || strings.TrimSpace(*email) != "" || strings.TrimSpace(*password) != "" || *passwordStdin ||
			strings.TrimSpace(*mailboxPassword) != "" || *mailboxPasswordStdin || strings.TrimSpace(*twofa) != "" || *twofaStdin || *storeCreds {
			return errors.New("session import flags cannot be combined with headless, password, 2FA, or credential-vault setup")
		}
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

	mailboxPasswordValue := strings.TrimSpace(*mailboxPassword)
	if *mailboxPasswordStdin {
		mailboxPasswordValue, err = readLine(reader)
		if err != nil {
			return err
		}
	} else if mailboxPasswordValue == "" && !*nonInteractive {
		mailboxPasswordValue, err = promptPassword("Mailbox password (leave empty if unused): ")
		if err != nil {
			return err
		}
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
	if !*headless && *skipVerify && twofaValue != "" {
		return errors.New("a one-time 2FA code is only used during immediate verification; remove --skip-verify or omit --twofa")
	}

	configPath, configPathErr := rcloneConfigFilePath()
	if configPathErr != nil {
		return configPathErr
	}
	configSnapshot, err := captureRcloneConfigSnapshot(configPath)
	if err != nil {
		return err
	}
	if err := configureRemote(remote, *email, passValue, mailboxPasswordValue, false); err != nil {
		return err
	}

	if *headless {
		fmt.Println("Initializing browserless rclone session...")
		if err := verifyRemoteWithOneTimeCode(remote, configPath, twofaValue); err != nil {
			recordAuthEvent(remote, "headless-rclone-login", false, strings.TrimSpace(err.Error()))
			return rollbackRcloneConfigError(configPath, configSnapshot, fmt.Errorf("browserless rclone login failed: %w", err))
		}
		recordAuthEvent(remote, "headless-rclone-login", true, "")
		fmt.Println("Browserless rclone session verified.")

		if err := configureProtonCLISessionFromRcloneRemote(remote, !*skipVerify); err != nil {
			recordAuthEvent(remote, "headless-proton-cli-session", false, strings.TrimSpace(err.Error()))
			return rollbackRcloneConfigError(configPath, configSnapshot, err)
		}
		recordAuthEvent(remote, "headless-proton-cli-session", true, "")
	} else if !*skipVerify {
		fmt.Println("Verifying connection...")
		if err := verifyRemoteWithOneTimeCode(remote, configPath, twofaValue); err != nil {
			recordAuthEvent(remote, "configure", false, strings.TrimSpace(err.Error()))
			return rollbackRcloneConfigError(configPath, configSnapshot, fmt.Errorf("verification failed: %w", err))
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
			Email:           *email,
			Password:        passValue,
			MailboxPassword: mailboxPasswordValue,
			SavedAt:         time.Now(),
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

func printUsage() {
	fmt.Println(`ProtonDrive CLI - manage Proton Drive from Linux/POSIX shells

Usage:
  protondrive [--backend auto|proton|rclone] [--remote name] <command> [options]

Backends:
  auto      Prefer the official proton-drive CLI where supported, fall back to rclone when needed.
  proton    Use Proton's official proton-drive CLI (auth, browse, upload/download workflows).
  rclone    Use rclone's Proton Drive backend (required for mounts and explicit copy/mirror controls).

Global options:
  --backend name          Backend selection (default: auto; env PROTONDRIVE_BACKEND).
  --remote name           rclone remote name (default: protondrive; selecting a custom remote uses rclone).
  --proton-drive-bin path Official Proton Drive CLI binary (default: proton-drive; env PROTONDRIVE_PROTON_BIN).
  --rclone-bin path       rclone binary (default: rclone; env PROTONDRIVE_RCLONE_BIN).

Commands:
  bootstrap    Download verified proton-drive/rclone helper binaries into a managed user directory.
  configure    Sign in with Proton's CLI or create/update an rclone remote.
  status       Show backend availability and authentication status.
  browse       List directories (default) or files (--files) under a path.
  sync         Copy a folder safely, or explicitly mirror it with deletion safeguards.
  mount        Mount ProtonDrive via rclone (Linux FUSE, macOS WebDAV fallback, optional --persist).
  unmount      Unmount a ProtonDrive mount point (optional --remove-persist).
  configs      List, show, or copy reusable sync config templates.
  version      Print build version, commit, date, Go version, OS, and architecture.

Use "protondrive <command> -h" for command-specific options.`)
}
