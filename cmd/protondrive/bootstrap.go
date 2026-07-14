package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/term"
)

type protonCLIAsset struct {
	Platform string
	URL      string
	SHA512   string
}

type bootstrapOptions struct {
	InstallProton         bool
	InstallRclone         bool
	InstallDir            string
	Force                 bool
	Yes                   bool
	AllowUnverifiedRclone bool
}

func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	installProton := fs.Bool("proton-drive", false, "Install Proton's official proton-drive CLI into the managed user bin directory")
	installRclone := fs.Bool("rclone", false, "Install rclone into the managed user bin directory")
	installAll := fs.Bool("all", false, "Install both proton-drive and rclone")
	installDir := fs.String("install-dir", defaultManagedBinDir(), "Directory for managed dependency binaries")
	force := fs.Bool("force", false, "Replace an existing managed binary")
	yes := fs.Bool("yes", false, "Do not prompt before downloading executable dependencies")
	allowUnverifiedRclone := fs.Bool("allow-unverified-rclone", false, "Allow rclone download if a release checksum cannot be verified")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandUsage(fs)
			return flag.ErrHelp
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("bootstrap does not accept positional arguments")
	}

	opts := bootstrapOptions{
		InstallProton:         *installProton || *installAll,
		InstallRclone:         *installRclone || *installAll,
		InstallDir:            expandPath(*installDir),
		Force:                 *force,
		Yes:                   *yes,
		AllowUnverifiedRclone: *allowUnverifiedRclone,
	}
	if !*installProton && !*installRclone && !*installAll {
		opts.InstallProton = true
		opts.InstallRclone = true
	}

	if err := confirmBootstrap(opts); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.InstallDir, 0o755); err != nil { // #nosec G301 -- managed bin directory must be traversable to execute helpers
		return fmt.Errorf("failed to create managed bin directory: %w", err)
	}

	fmt.Printf("Managed dependency directory: %s\n", opts.InstallDir)
	if opts.InstallProton {
		if err := bootstrapProtonDrive(opts.InstallDir, opts.Force); err != nil {
			return err
		}
	}
	if opts.InstallRclone {
		if err := bootstrapRclone(opts.InstallDir, opts.Force, opts.AllowUnverifiedRclone); err != nil {
			return err
		}
	}

	fmt.Println("Bootstrap complete.")
	fmt.Printf("Future runs will use managed dependencies automatically when %s or %s are not available on PATH.\n", protonDriveDefaultBin, rcloneDefaultBin)
	if insideFlatpak() {
		fmt.Println("Flatpak mode detected: managed tools are stored in user data and can be launched through flatpak-spawn when needed.")
	}
	return nil
}

func confirmBootstrap(opts bootstrapOptions) error {
	if opts.Yes {
		return nil
	}
	tools := make([]string, 0, 2)
	if opts.InstallProton {
		tools = append(tools, "Proton Drive CLI")
	}
	if opts.InstallRclone {
		tools = append(tools, "rclone")
	}
	if !term.IsTerminal(int(syscall.Stdin)) {
		return errors.New("refusing to download executable dependencies without confirmation; rerun with --yes")
	}
	fmt.Fprintf(os.Stderr, "Download and install %s into %s? [y/N] ", strings.Join(tools, " and "), opts.InstallDir)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("bootstrap cancelled")
	}
}

func bootstrapProtonDrive(installDir string, force bool) error {
	target := filepath.Join(installDir, protonDriveDefaultBin)
	if isExecutable(target) && !force {
		fmt.Printf("proton-drive already installed: %s\n", target)
		return nil
	}

	platform, err := protonCLIPlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	fmt.Printf("Resolving Proton Drive CLI download for %s...\n", platform)
	indexHTML, err := fetchURLBytes(protonCLIDownloadIndex, 2<<20)
	if err != nil {
		return fmt.Errorf("failed to read Proton Drive CLI download index: %w", err)
	}
	assets := parseProtonCLIAssets(string(indexHTML))
	asset, ok := assets[platform]
	if !ok {
		return fmt.Errorf("proton-drive CLI download index did not contain an asset for %s", platform)
	}
	if err := validateProtonCLIAssetURL(asset.URL); err != nil {
		return err
	}
	if err := downloadVerifiedBinary(asset.URL, target, asset.SHA512, "sha512"); err != nil {
		return fmt.Errorf("failed to install proton-drive: %w", err)
	}
	fmt.Printf("Installed proton-drive: %s\n", target)
	return nil
}

func bootstrapRclone(installDir string, force, allowUnverified bool) error {
	target := filepath.Join(installDir, rcloneDefaultBin)
	if isExecutable(target) && !force {
		fmt.Printf("rclone already installed: %s\n", target)
		return nil
	}

	goos, goarch, err := rclonePlatform(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	version, err := fetchRcloneVersion()
	if err != nil {
		return err
	}
	archiveName := fmt.Sprintf("rclone-%s-%s-%s.zip", version, goos, goarch)
	archiveURL := fmt.Sprintf("%s/%s/%s", rcloneGitHubReleaseURL, version, archiveName)
	checksumURL := fmt.Sprintf("%s/%s/SHA256SUMS", rcloneGitHubReleaseURL, version)

	fmt.Printf("Resolving rclone %s download for %s/%s...\n", version, goos, goarch)
	expectedSHA256, checksumErr := fetchRcloneChecksum(checksumURL, archiveName)
	if checksumErr != nil && !allowUnverified {
		return fmt.Errorf("failed to verify rclone release checksum: %w; rerun with --allow-unverified-rclone only if you accept that risk", checksumErr)
	}
	if checksumErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: installing rclone without checksum verification: %v\n", checksumErr)
	}

	archivePath, err := downloadTempFile(archiveURL, installDir, "rclone-*.zip")
	if err != nil {
		return fmt.Errorf("failed to download rclone archive: %w", err)
	}
	defer os.Remove(archivePath)

	if expectedSHA256 != "" {
		if err := verifyFileChecksum(archivePath, expectedSHA256, "sha256"); err != nil {
			return fmt.Errorf("rclone archive checksum verification failed: %w", err)
		}
	}
	if err := extractBinaryFromZip(archivePath, rcloneDefaultBin, target); err != nil {
		return fmt.Errorf("failed to extract rclone: %w", err)
	}
	fmt.Printf("Installed rclone: %s\n", target)
	return nil
}

func protonCLIPlatform(goos, goarch string) (string, error) {
	var osName string
	switch goos {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "macos"
	default:
		return "", fmt.Errorf("proton-drive CLI bootstrap is not supported on %s/%s", goos, goarch)
	}
	switch goarch {
	case "amd64":
		return osName + "/x64", nil
	case "arm64":
		return osName + "/arm64", nil
	default:
		return "", fmt.Errorf("proton-drive CLI bootstrap is not supported on %s/%s", goos, goarch)
	}
}

func parseProtonCLIAssets(html string) map[string]protonCLIAsset {
	assets := make(map[string]protonCLIAsset)
	rowPattern := regexp.MustCompile(`(?is)<tr>\s*<td>\s*([^<]+?)\s*</td>\s*<td>\s*<a\s+href="([^"]+)"[^>]*>.*?</a>\s*</td>\s*<td>\s*<code>\s*([0-9a-f]{128})\s*</code>\s*</td>\s*</tr>`)
	for _, match := range rowPattern.FindAllStringSubmatch(html, -1) {
		platform := strings.TrimSpace(match[1])
		assets[platform] = protonCLIAsset{
			Platform: platform,
			URL:      strings.TrimSpace(match[2]),
			SHA512:   strings.ToLower(strings.TrimSpace(match[3])),
		}
	}
	return assets
}

func validateProtonCLIAssetURL(rawURL string) error {
	parsed, err := validateHTTPSURL(rawURL)
	if err != nil {
		return fmt.Errorf("invalid Proton Drive CLI asset URL: %w", err)
	}
	if !strings.EqualFold(parsed.Hostname(), "proton.me") || !strings.HasPrefix(parsed.EscapedPath(), "/download/drive/cli/") {
		return fmt.Errorf("refusing Proton Drive CLI asset outside proton.me/download/drive/cli: %s", rawURL)
	}
	return nil
}

func rclonePlatform(goos, goarch string) (string, string, error) {
	switch goos {
	case "linux":
	case "darwin":
		goos = "osx"
	default:
		return "", "", fmt.Errorf("rclone bootstrap is not supported on %s/%s", goos, goarch)
	}
	switch goarch {
	case "amd64", "arm64":
		return goos, goarch, nil
	default:
		return "", "", fmt.Errorf("rclone bootstrap is not supported on %s/%s", goos, goarch)
	}
}

func fetchRcloneVersion() (string, error) {
	body, err := fetchURLBytes(rcloneVersionURL, 256<<10)
	if err != nil {
		return "", fmt.Errorf("failed to read rclone version: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(body)))
	if len(fields) < 2 || fields[0] != "rclone" || !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(fields[1]) {
		return "", fmt.Errorf("unexpected rclone version response: %s", strings.TrimSpace(string(body)))
	}
	return fields[1], nil
}

func fetchRcloneChecksum(checksumURL, archiveName string) (string, error) {
	body, err := fetchURLBytes(checksumURL, 2<<20)
	if err != nil {
		return "", err
	}
	sum, err := rcloneChecksumFromText(string(body), archiveName)
	if err != nil {
		return "", fmt.Errorf("%w in %s", err, checksumURL)
	}
	return sum, nil
}

func rcloneChecksumFromText(text, archiveName string) (string, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == archiveName {
			sum := strings.ToLower(strings.TrimSpace(fields[0]))
			if len(sum) != 64 {
				return "", fmt.Errorf("invalid SHA256 checksum for %s", archiveName)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("checksum for %s not found", archiveName)
}

func fetchURLBytes(rawURL string, limit int64) ([]byte, error) {
	if _, err := validateHTTPSURL(rawURL); err != nil {
		return nil, err
	}
	resp, err := httpClient().Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("get %s returned %s", rawURL, resp.Status)
	}
	var reader io.Reader = resp.Body
	if limit > 0 {
		reader = io.LimitReader(resp.Body, limit+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeded %d bytes", limit)
	}
	return body, nil
}

func downloadVerifiedBinary(rawURL, target, expected, algorithm string) error {
	temp, err := downloadTempFile(rawURL, filepath.Dir(target), filepath.Base(target)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := verifyFileChecksum(temp, expected, algorithm); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0o755); err != nil { // #nosec G302 -- downloaded helper binaries must be executable
		return err
	}
	return os.Rename(temp, target)
}

func downloadTempFile(rawURL, dir, pattern string) (string, error) {
	if _, err := validateHTTPSURL(rawURL); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- helper download directory must be traversable for executable installation
		return "", err
	}
	temp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	defer temp.Close()

	resp, err := httpClient().Get(rawURL)
	if err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("get %s returned %s", rawURL, resp.Status)
	}
	limited := io.LimitReader(resp.Body, maxDependencyDownloadBytes+1)
	written, err := io.Copy(temp, limited)
	if err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	if written > maxDependencyDownloadBytes {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("download exceeded %d bytes", maxDependencyDownloadBytes)
	}
	return tempPath, nil
}

func verifyFileChecksum(filePath, expected, algorithm string) error {
	file, err := os.Open(filePath) // #nosec G304 -- checksum verification opens the temporary file just downloaded by this process
	if err != nil {
		return err
	}
	defer file.Close()

	var actual string
	switch algorithm {
	case "sha256":
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			return err
		}
		actual = hex.EncodeToString(hash.Sum(nil))
	case "sha512":
		hash := sha512.New()
		if _, err := io.Copy(hash, file); err != nil {
			return err
		}
		actual = hex.EncodeToString(hash.Sum(nil))
	default:
		return fmt.Errorf("unsupported checksum algorithm %q", algorithm)
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%s mismatch: got %s, want %s", algorithm, actual, strings.ToLower(expected))
	}
	return nil
}

func extractBinaryFromZip(archivePath, binaryName, target string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	for _, file := range reader.File {
		if file.FileInfo().IsDir() || filepath.Base(file.Name) != binaryName {
			continue
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		defer src.Close()
		if file.UncompressedSize64 > uint64(maxDependencyDownloadBytes) {
			return fmt.Errorf("%s in %s exceeded %d bytes", binaryName, archivePath, maxDependencyDownloadBytes)
		}

		temp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+"-*")
		if err != nil {
			return err
		}
		tempPath := temp.Name()
		written, copyErr := io.Copy(temp, io.LimitReader(src, maxDependencyDownloadBytes+1))
		closeErr := temp.Close()
		if copyErr != nil {
			_ = os.Remove(tempPath)
			return copyErr
		}
		if written > maxDependencyDownloadBytes {
			_ = os.Remove(tempPath)
			return fmt.Errorf("%s in %s exceeded %d bytes while extracting", binaryName, archivePath, maxDependencyDownloadBytes)
		}
		if closeErr != nil {
			_ = os.Remove(tempPath)
			return closeErr
		}
		if err := os.Chmod(tempPath, 0o755); err != nil { // #nosec G302 -- extracted helper binaries must be executable
			_ = os.Remove(tempPath)
			return err
		}
		return os.Rename(tempPath, target)
	}
	return fmt.Errorf("%s not found in %s", binaryName, archivePath)
}

func httpClient() *http.Client {
	return &http.Client{
		Timeout: dependencyDownloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many download redirects")
			}
			if !strings.EqualFold(req.URL.Scheme, "https") {
				return fmt.Errorf("refusing download redirect to non-HTTPS URL %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

func validateHTTPSURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(parsed.Scheme, "https") || strings.TrimSpace(parsed.Host) == "" || parsed.User != nil {
		return nil, fmt.Errorf("download URL must be absolute HTTPS without embedded credentials: %s", parsed.Redacted())
	}
	return parsed, nil
}

func defaultManagedBinDir() string {
	if override := strings.TrimSpace(os.Getenv(managedBinDirEnv)); override != "" {
		return expandPath(override)
	}
	if runtime.GOOS == "linux" {
		if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
			return filepath.Join(expandPath(dataHome), "protondrive", "bin")
		}
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return filepath.Join(home, ".local", "share", "protondrive", "bin")
		}
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return filepath.Join(home, "Library", "Application Support", "protondrive", "bin")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".local", "share", "protondrive", "bin")
	}
	return filepath.Join(".", ".protondrive", "bin")
}

func managedBinaryPath(bin string) (string, bool) {
	base := filepath.Base(strings.TrimSpace(bin))
	switch base {
	case protonDriveDefaultBin, rcloneDefaultBin:
	default:
		return "", false
	}
	return filepath.Join(defaultManagedBinDir(), base), true
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
