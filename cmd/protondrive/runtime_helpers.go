package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/term"
)

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

func externalCommandAvailable(bin string) bool {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return false
	}
	if _, err := exec.LookPath(bin); err == nil {
		return true
	}
	return hostCommandAvailable(bin)
}

func externalCommand(bin string, args ...string) *exec.Cmd {
	return externalCommandWithEnvironment(bin, nil, args...)
}

// externalCommandWithEnvironment passes non-secret environment overrides to a
// local helper or through flatpak-spawn. Values forwarded through Flatpak are
// visible in flatpak-spawn's argv, so callers must never use this for secrets.
func externalCommandWithEnvironment(bin string, extraEnv map[string]string, args ...string) *exec.Cmd {
	if insideFlatpak() && filepath.IsAbs(bin) && hostCommandAvailable(bin) {
		spawnArgs := flatpakHostSpawnArgsWithEnvironment(bin, extraEnv, args...)
		return safeExecCommand("flatpak-spawn", spawnArgs...) // #nosec G204 -- explicit binary path is forwarded as argv, not shell-expanded
	}
	if _, err := exec.LookPath(bin); err == nil {
		cmd := safeExecCommand(bin, args...) // #nosec G204 -- external helper binary is user-selected wrapper configuration
		if len(extraEnv) > 0 {
			cmd.Env = sanitizedChildEnvironment(extraEnv)
		}
		return cmd
	}
	if hostCommandAvailable(bin) {
		spawnArgs := flatpakHostSpawnArgsWithEnvironment(bin, extraEnv, args...)
		return safeExecCommand("flatpak-spawn", spawnArgs...) // #nosec G204 -- host helper binary is forwarded as argv, not shell-expanded
	}
	cmd := safeExecCommand(bin, args...) // #nosec G204 -- fallback returns the intended helper command so callers get the OS error
	if len(extraEnv) > 0 {
		cmd.Env = sanitizedChildEnvironment(extraEnv)
	}
	return cmd
}

func safeExecCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...) // #nosec G204,G702 -- centralized argv execution without shell interpolation; callers validate user-selected binaries/options
	cmd.Env = sanitizedChildEnvironment(nil)
	return cmd
}

func sanitizedChildEnvironment(overrides map[string]string) []string {
	blocked := map[string]bool{vaultPassphraseEnv: true}
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, assignment := range os.Environ() {
		name, _, found := strings.Cut(assignment, "=")
		if !found || blocked[name] {
			continue
		}
		if _, overridden := overrides[name]; overridden {
			continue
		}
		result = append(result, assignment)
	}
	return append(result, environmentAssignments(overrides)...)
}

func flatpakHostSpawnArgs(bin string, args ...string) []string {
	return flatpakHostSpawnArgsWithEnvironment(bin, nil, args...)
}

func flatpakHostSpawnArgsWithEnvironment(bin string, extraEnv map[string]string, args ...string) []string {
	spawnArgs := []string{"--host"}
	seen := make(map[string]bool)
	for _, name := range []string{
		"RCLONE_CONFIG",
		"PROTON_DRIVE_UNSAFE_SECRETS",
		"PROTON_DRIVE_CACHE_DIR",
		"XDG_CONFIG_HOME",
		"XDG_CACHE_HOME",
		"XDG_DATA_HOME",
	} {
		seen[name] = true
		value, overridden := extraEnv[name]
		if !overridden {
			value, overridden = os.LookupEnv(name)
		}
		if overridden {
			spawnArgs = append(spawnArgs, "--env="+name+"="+value)
		}
	}
	var additional []string
	for name := range extraEnv {
		if !seen[name] {
			additional = append(additional, name)
		}
	}
	sort.Strings(additional)
	for _, name := range additional {
		spawnArgs = append(spawnArgs, "--env="+name+"="+extraEnv[name])
	}
	spawnArgs = append(spawnArgs, bin)
	return append(spawnArgs, args...)
}

func environmentAssignments(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	assignments := make([]string, 0, len(keys))
	for _, key := range keys {
		assignments = append(assignments, key+"="+values[key])
	}
	return assignments
}

func hostCommandAvailable(bin string) bool {
	if !insideFlatpak() {
		return false
	}
	if _, err := exec.LookPath("flatpak-spawn"); err != nil {
		return false
	}
	cmd := safeExecCommand( // #nosec G204 -- flatpak-spawn command checks host command availability with bin passed as positional parameter
		"flatpak-spawn",
		"--host",
		"sh",
		"-lc",
		`if [ -n "$1" ] && [ -x "$1" ]; then exit 0; fi; command -v -- "$1" >/dev/null 2>&1`,
		"sh",
		bin,
	)
	return cmd.Run() == nil
}

func insideFlatpak() bool {
	_, err := os.Stat("/.flatpak-info")
	return err == nil
}

func ensureRclone() error {
	if !externalCommandAvailable(currentOptions.RcloneBin) {
		return fmt.Errorf("%s not found in PATH. Run 'protondrive bootstrap --rclone --yes', install rclone from https://rclone.org/install/, or set --rclone-bin", currentOptions.RcloneBin)
	}
	return nil
}

func ensureProtonDrive() error {
	if !externalCommandAvailable(currentOptions.ProtonDriveBin) {
		return fmt.Errorf("%s not found in PATH. Run 'protondrive bootstrap --proton-drive --yes', install the official Proton Drive CLI from https://proton.me/download/drive/cli/index.html, or set --proton-drive-bin", currentOptions.ProtonDriveBin)
	}
	return nil
}

func runRcloneCapture(args ...string) (string, error) {
	cmd := externalCommand(currentOptions.RcloneBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %s", currentOptions.RcloneBin, strings.Join(redactCommandArgs(args), " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func runRcloneCaptureWithConfig(configPath string, args ...string) (string, error) {
	cmd := externalCommandWithEnvironment(currentOptions.RcloneBin, map[string]string{"RCLONE_CONFIG": configPath}, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %s", currentOptions.RcloneBin, strings.Join(redactCommandArgs(args), " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func streamRclone(args ...string) error {
	cmd := externalCommand(currentOptions.RcloneBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runProtonDriveCapture(args ...string) (string, error) {
	cmd := externalCommand(currentOptions.ProtonDriveBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s failed: %s", currentOptions.ProtonDriveBin, strings.Join(redactCommandArgs(args), " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func streamProtonDrive(args ...string) error {
	cmd := externalCommand(currentOptions.ProtonDriveBin, args...)
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
						if err := addRecursiveWatch(watcher, event.Name); err != nil {
							fmt.Fprintf(os.Stderr, "Watcher error: unable to watch %s: %v\n", event.Name, err)
						}
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

	if err := runWithRetry(run, 5, time.Second); err != nil {
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
			if err := runWithRetry(run, 5, time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "Sync failed after retries: %v. Watcher remains active.\n", err)
				fmt.Println("Watching for more changes...")
				continue
			}
			fmt.Println("Watching for more changes...")
		}
	}
}

func runWithRetry(run func() error, attempts int, baseDelay time.Duration) error {
	if attempts < 1 {
		attempts = 1
	}
	if baseDelay < 0 {
		baseDelay = 0
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt == attempts {
			break
		}
		delay := baseDelay * time.Duration(1<<(attempt-1))
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		fmt.Fprintf(os.Stderr, "Sync attempt %d/%d failed: %v. Retrying in %s...\n", attempt, attempts, lastErr, delay)
		time.Sleep(delay)
	}
	return lastErr
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
