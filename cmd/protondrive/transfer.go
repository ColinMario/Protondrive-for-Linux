package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ColinMario/Protondrive-for-Linux/internal/syncconfig"
)

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
	if fs.NArg() != 0 {
		return errors.New("browse does not accept positional arguments; use --remote-path")
	}

	backend, err := resolveBackend("browse", args)
	if err != nil {
		return err
	}
	if backend == backendProton {
		if *files && *all {
			return errors.New("use either --files or --all when browsing with the Proton backend, not both")
		}
		return runProtonBrowse(remote, *remotePathFlag, *files, *all)
	}
	if *all {
		return errors.New("--all is only supported by the Proton backend; omit it or select --backend proton")
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
	operationFlag := fs.String("operation", "", "Transfer operation: copy (safe default) or mirror (deletes at destination)")
	dryRun := fs.Bool("dry-run", false, "Show actions without applying changes")
	noProgress := fs.Bool("no-progress", false, "Disable rclone progress output")
	confirmMirror := fs.Bool("confirm-mirror", false, "Confirm destructive mirror semantics (required unless --dry-run)")
	maxDelete := fs.Int("max-delete", -1, "Maximum deletions allowed per mirror run (default 25)")
	backupDir := fs.String("backup-dir", "", "Mirror backup directory for replaced/deleted destination files")
	noBackupDir := fs.Bool("no-backup-dir", false, "Disable the automatic mirror backup directory")
	sourceSentinel := fs.String("source-sentinel", "", "Relative file that must exist at the source before mirroring")
	allowEmptySource := fs.Bool("allow-empty-source", false, "Allow an empty mirror source (can empty the destination)")
	allowRootMirror := fs.Bool("allow-root-mirror", false, "Allow mirroring a filesystem, home, or remote root")
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
		"operation":                true,
		"config":                   true,
		"conflict-strategy":        true,
		"file-conflict-strategy":   true,
		"folder-conflict-strategy": true,
		"watch-debounce":           true,
		"dry-run":                  false,
		"no-progress":              false,
		"confirm-mirror":           false,
		"max-delete":               true,
		"backup-dir":               true,
		"no-backup-dir":            false,
		"source-sentinel":          true,
		"allow-empty-source":       false,
		"allow-root-mirror":        false,
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

	operation := strings.ToLower(strings.TrimSpace(*operationFlag))
	if operation == "" && cfg != nil {
		operation = strings.ToLower(strings.TrimSpace(cfg.Config.Operation))
	}
	if operation == "" {
		operation = syncconfig.OperationCopy
	}
	if operation != syncconfig.OperationCopy && operation != syncconfig.OperationMirror {
		return errors.New("operation must be 'copy' or 'mirror'")
	}
	if operation == syncconfig.OperationMirror && !*confirmMirror && !*dryRun {
		return errors.New("mirror deletes destination files that are absent at the source; inspect with --dry-run, then repeat with --confirm-mirror")
	}
	if operation != syncconfig.OperationMirror {
		configHasMirrorSafety := cfg != nil && (cfg.Config.MaxDelete != nil || strings.TrimSpace(cfg.Config.BackupDir) != "" || strings.TrimSpace(cfg.Config.SourceSentinel) != "" || cfg.Config.AllowEmptySource)
		if *confirmMirror || *maxDelete >= 0 || strings.TrimSpace(*backupDir) != "" || *noBackupDir || strings.TrimSpace(*sourceSentinel) != "" || *allowEmptySource || *allowRootMirror || configHasMirrorSafety {
			return errors.New("mirror safety flags require '--operation mirror'")
		}
	}

	effectiveMaxDelete := defaultMirrorMaxDelete
	if cfg != nil && cfg.Config.MaxDelete != nil {
		effectiveMaxDelete = *cfg.Config.MaxDelete
	}
	if *maxDelete >= 0 {
		effectiveMaxDelete = *maxDelete
	}
	if effectiveMaxDelete < 0 {
		return errors.New("max-delete must be zero or greater")
	}
	effectiveBackupDir := strings.TrimSpace(*backupDir)
	if effectiveBackupDir == "" && cfg != nil {
		effectiveBackupDir = strings.TrimSpace(cfg.Config.BackupDir)
	}
	effectiveSentinel := strings.TrimSpace(*sourceSentinel)
	if effectiveSentinel == "" && cfg != nil {
		effectiveSentinel = strings.TrimSpace(cfg.Config.SourceSentinel)
	}
	effectiveAllowEmpty := *allowEmptySource
	if cfg != nil && cfg.Config.AllowEmptySource {
		effectiveAllowEmpty = true
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

	localAbs, err := filepath.Abs(expandPath(localPath))
	if err != nil {
		return fmt.Errorf("unable to resolve local path %q: %w", localPath, err)
	}
	if dir == "upload" {
		if stat, err := os.Stat(localAbs); err != nil || !stat.IsDir() {
			return fmt.Errorf("local path '%s' must exist and be a directory", localAbs)
		}
	} else {
		if err := os.MkdirAll(localAbs, 0o755); err != nil { // #nosec G301 -- user-selected download folder should be normally accessible
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
		if operation == syncconfig.OperationMirror {
			return fmt.Errorf("mirror operations require '--backend %s'; the Proton backend performs non-destructive upload/download transfers", backendRclone)
		}
		if watchEnabled && !hasProtonSyncFlags(*conflictStrategy, *fileConflictStrategy, *folderConflictStrategy, *skipThumbnails) {
			return errors.New("proton watch mode requires an explicit conflict strategy so unattended conflicts cannot prompt or choose implicitly")
		}
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
		return fmt.Errorf("proton conflict and thumbnail flags require '--backend %s'", backendProton)
	}
	if err := ensureRemoteAuth(remote); err != nil {
		return err
	}

	if err := validateRcloneExtraArgs(operation, extra); err != nil {
		return err
	}
	if cfg != nil {
		if err := validateRcloneExtraArgs(operation, cfg.Config.ExtraRcloneArgs); err != nil {
			return fmt.Errorf("config %q: %w", cfg.DisplayName, err)
		}
	}
	if operation == syncconfig.OperationMirror && !*allowRootMirror {
		if err := validateMirrorRoots(dir, localAbs, remotePathValue); err != nil {
			return err
		}
	}

	rcloneCommand := "copy"
	if operation == syncconfig.OperationMirror {
		rcloneCommand = "sync"
	}
	cmd := []string{rcloneCommand, src, dst, "-v"}
	if !*noProgress {
		cmd = append(cmd, "--progress")
	}
	if *dryRun {
		cmd = append(cmd, "--dry-run")
	}
	if operation == syncconfig.OperationMirror {
		cmd = append(cmd, "--max-delete", strconv.Itoa(effectiveMaxDelete))
		if !*noBackupDir {
			if effectiveBackupDir == "" {
				effectiveBackupDir = defaultMirrorBackupDir(remote, remotePathValue, dir, localAbs, time.Now().UTC())
			} else if dir == "upload" && !strings.Contains(effectiveBackupDir, ":") {
				effectiveBackupDir = remotePath(remote, effectiveBackupDir)
			} else if dir == "download" {
				effectiveBackupDir, err = filepath.Abs(expandPath(effectiveBackupDir))
				if err != nil {
					return fmt.Errorf("unable to resolve mirror backup directory: %w", err)
				}
			}
			if err := validateMirrorBackupDestination(dst, effectiveBackupDir, dir == "download"); err != nil {
				return err
			}
			cmd = append(cmd, "--backup-dir", effectiveBackupDir)
		}
	}
	if cfg != nil && len(cfg.Config.ExtraRcloneArgs) > 0 {
		cmd = append(cmd, cfg.Config.ExtraRcloneArgs...)
	}
	cmd = append(cmd, extra...)

	runOnce := func() error {
		if operation == syncconfig.OperationMirror {
			if err := validateMirrorSource(src, dir == "upload", effectiveSentinel, effectiveAllowEmpty); err != nil {
				return err
			}
		}
		fmt.Printf("Running: rclone %s\n", strings.Join(redactCommandArgs(cmd), " "))
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
			// The delimiter belongs to this wrapper. Forward only the arguments
			// after it so rclone does not interpret "--" as a positional path.
			positional = append(positional, args[i+1:]...)
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

func validateRcloneExtraArgs(operation string, args []string) error {
	for _, arg := range args {
		name := strings.TrimLeft(strings.SplitN(strings.TrimSpace(arg), "=", 2)[0], "-")
		switch name {
		case "dry-run", "n", "max-delete", "backup-dir", "ignore-errors", "inplace":
			return fmt.Errorf("--%s is managed by protondrive and cannot be overridden through rclone passthrough arguments", name)
		case "delete-excluded":
			return errors.New("--delete-excluded is not accepted because it expands mirror deletion scope beyond the wrapper's source selection")
		case "delete-before", "delete-during", "delete-after":
			if operation != syncconfig.OperationMirror {
				return fmt.Errorf("--%s can delete destination data and requires '--operation mirror --confirm-mirror'", name)
			}
		}
	}
	return nil
}

func validateMirrorRoots(direction, localAbs, remotePathValue string) error {
	if direction == "upload" {
		if strings.TrimSpace(remotePathValue) == "" || strings.Trim(strings.TrimSpace(remotePathValue), "/") == "" {
			return errors.New("refusing to mirror into the remote root; choose --remote-path or add --allow-root-mirror after a dry-run")
		}
		if dangerousLocalRoot(localAbs) {
			return errors.New("refusing to mirror a filesystem or home-directory root; choose a narrower source or add --allow-root-mirror after a dry-run")
		}
		return nil
	}
	if dangerousLocalRoot(localAbs) {
		return errors.New("refusing to mirror into a filesystem or home-directory root; choose a narrower destination or add --allow-root-mirror after a dry-run")
	}
	return nil
}

func dangerousLocalRoot(value string) bool {
	clean := canonicalMountPath(value)
	volume := filepath.VolumeName(clean)
	root := string(os.PathSeparator)
	if volume != "" {
		root = volume + string(os.PathSeparator)
	}
	if sameMountPoint(clean, root) {
		return true
	}
	home, err := os.UserHomeDir()
	return err == nil && sameMountPoint(clean, canonicalMountPath(home))
}

func defaultMirrorBackupDir(remote, remotePathValue, direction, localAbs string, now time.Time) string {
	stamp := now.UTC().Format("20060102T150405Z")
	if direction == "download" {
		return filepath.Join(filepath.Dir(localAbs), ".protondrive-backups", filepath.Base(localAbs), stamp)
	}
	remoteKey := strings.Trim(strings.TrimSpace(filepath.ToSlash(remotePathValue)), "/")
	if remoteKey == "" {
		remoteKey = "remote-root"
	}
	return remotePath(remote, path.Join(".protondrive-backups", remoteKey, stamp))
}

func validateMirrorBackupDestination(destination, backup string, local bool) error {
	if strings.TrimSpace(backup) == "" {
		return errors.New("mirror backup directory cannot be empty")
	}
	if local {
		destinationAbs := canonicalMountPath(destination)
		backupAbs := canonicalMountPath(backup)
		rel, err := filepath.Rel(destinationAbs, backupAbs)
		if err != nil {
			return err
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return fmt.Errorf("mirror backup directory %q must be outside destination %q", backup, destination)
		}
		return nil
	}

	destinationRemote, destinationPath, destinationOK := strings.Cut(destination, ":")
	backupRemote, backupPath, backupOK := strings.Cut(backup, ":")
	if !destinationOK || !backupOK || destinationRemote != backupRemote {
		return nil
	}
	destinationPath = path.Clean("/" + strings.TrimLeft(destinationPath, "/"))
	backupPath = path.Clean("/" + strings.TrimLeft(backupPath, "/"))
	if backupPath == destinationPath || destinationPath == "/" || strings.HasPrefix(backupPath, strings.TrimSuffix(destinationPath, "/")+"/") {
		return fmt.Errorf("mirror backup directory %q must be outside destination %q on remote %s", backup, destination, destinationRemote)
	}
	return nil
}

func validateMirrorSource(source string, local bool, sentinel string, allowEmpty bool) error {
	sentinel = strings.TrimSpace(sentinel)
	if sentinel != "" {
		cleanSentinel := filepath.ToSlash(filepath.Clean(sentinel))
		if filepath.IsAbs(sentinel) || cleanSentinel == ".." || strings.HasPrefix(cleanSentinel, "../") {
			return errors.New("source sentinel must be a relative path inside the mirror source")
		}
	}
	if local {
		if sentinel != "" {
			target := filepath.Join(source, filepath.FromSlash(sentinel))
			rel, err := filepath.Rel(source, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return errors.New("source sentinel escapes the local mirror source")
			}
			info, err := os.Lstat(target)
			if err != nil {
				return fmt.Errorf("mirror source sentinel %q is unavailable: %w", sentinel, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("mirror source sentinel %q must be a regular file and not a symlink", sentinel)
			}
		}
		hasContent := false
		err := filepath.WalkDir(source, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(source, current)
			if err != nil {
				return err
			}
			if sentinel != "" && sameMountPoint(rel, filepath.FromSlash(sentinel)) {
				return nil
			}
			hasContent = true
			return filepath.SkipAll
		})
		if err != nil {
			return fmt.Errorf("unable to inspect mirror source %q: %w", source, err)
		}
		if !hasContent && !allowEmpty {
			return errors.New("refusing to mirror an empty source; restore the source or pass --allow-empty-source after a dry-run")
		}
		return nil
	}
	if sentinel != "" {
		statOutput, err := runRcloneCapture("lsjson", remoteChildPath(source, sentinel), "--stat")
		if err != nil {
			return fmt.Errorf("remote mirror source sentinel %q is unavailable: %w", sentinel, err)
		}
		var stat struct {
			IsDir bool `json:"IsDir"`
		}
		if err := json.Unmarshal([]byte(statOutput), &stat); err != nil {
			return fmt.Errorf("remote mirror source sentinel %q returned invalid metadata: %w", sentinel, err)
		}
		if stat.IsDir {
			return fmt.Errorf("remote mirror source sentinel %q must be a file", sentinel)
		}
	}
	if allowEmpty {
		return nil
	}
	sizeOutput, err := runRcloneCapture("size", source, "--json")
	if err != nil {
		return fmt.Errorf("unable to inspect remote mirror source: %w", err)
	}
	var size struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal([]byte(sizeOutput), &size); err != nil {
		return fmt.Errorf("unable to parse remote mirror source size: %w", err)
	}
	minimum := int64(1)
	if sentinel != "" {
		minimum = 2
	}
	if size.Count < minimum {
		return errors.New("refusing to mirror an empty remote source; restore the source or pass --allow-empty-source after a dry-run")
	}
	return nil
}

func remoteChildPath(base, child string) string {
	remote, rest, ok := strings.Cut(base, ":")
	if !ok {
		return path.Join(base, filepath.ToSlash(child))
	}
	return remote + ":" + path.Join(rest, filepath.ToSlash(child))
}

func redactCommandArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	sensitive := map[string]bool{
		"password": true, "pass": true, "2fa": true, "twofa": true,
		"access-token": true, "refresh-token": true, "client-access-token": true,
		"client-refresh-token": true, "password-command": true,
	}
	for i := 0; i < len(redacted); i++ {
		name, value, inline := strings.Cut(redacted[i], "=")
		key := strings.TrimLeft(strings.ToLower(name), "-")
		if !sensitive[key] {
			continue
		}
		if inline {
			redacted[i] = name + "=<redacted>"
		} else if value == "" && i+1 < len(redacted) {
			redacted[i+1] = "<redacted>"
			i++
		}
	}
	return redacted
}
