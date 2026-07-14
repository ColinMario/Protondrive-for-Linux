package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	statusExitNotConfigured = 3
	statusExitAuthFailed    = 4
	statusExitBackendFailed = 5
)

type statusExitError struct {
	code    int
	message string
	silent  bool
}

func (e statusExitError) Error() string { return e.message }
func (e statusExitError) ExitCode() int { return e.code }
func (e statusExitError) Silent() bool  { return e.silent }

type statusReport struct {
	Healthy       bool         `json:"healthy"`
	Configured    bool         `json:"configured"`
	Authenticated bool         `json:"authenticated"`
	Backend       string       `json:"backend"`
	Remote        string       `json:"remote"`
	Version       string       `json:"backend_version,omitempty"`
	Message       string       `json:"message"`
	Listing       string       `json:"listing,omitempty"`
	VaultPresent  bool         `json:"vault_present,omitempty"`
	State         *remoteState `json:"state,omitempty"`
}

func runStatus(remote string, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	details := fs.Bool("details", false, "List ProtonDrive folders if configured")
	jsonOutput := fs.Bool("json", false, "Print a machine-readable health report")
	informational := fs.Bool("informational", false, "Always exit zero after printing status")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("status does not accept positional arguments")
	}

	backend, err := resolveBackend("status", args)
	if err != nil {
		return emitStatusFailure(statusReport{Backend: currentOptions.Backend, Remote: normalizedRemoteName(remote), Message: err.Error()}, statusExitBackendFailed, *jsonOutput, *informational)
	}
	if backend == backendProton {
		return runProtonStatus(remote, *details, *jsonOutput, *informational)
	}

	report := statusReport{Backend: backendRclone, Remote: normalizedRemoteName(remote)}
	output, err := runRcloneCapture("listremotes")
	if err != nil {
		report.Message = err.Error()
		return emitStatusFailure(report, statusExitBackendFailed, *jsonOutput, *informational)
	}
	target := fmt.Sprintf("%s:", normalizeRemote(remote))
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == target {
			report.Configured = true
			break
		}
	}
	if !report.Configured {
		report.Message = fmt.Sprintf("Remote '%s' is not configured.", remote)
		return emitStatusFailure(report, statusExitNotConfigured, *jsonOutput, *informational)
	}
	report.Message = fmt.Sprintf("Remote '%s' is configured.", remote)
	if !*jsonOutput {
		fmt.Println(report.Message)
	}

	report.VaultPresent = hasStoredCredentials(remote)
	state := mustLoadRemoteState(remote)
	if err := ensureRemoteAuthForStatus(remote); err != nil {
		report.Message = strings.TrimSpace(err.Error())
		if *details {
			report.State = &state
			if !*jsonOutput {
				printStatusDetails(remote, state, report.VaultPresent)
			}
		}
		code := statusExitBackendFailed
		if isAuthError(err) {
			code = statusExitAuthFailed
		}
		return emitStatusFailure(report, code, *jsonOutput, *informational)
	}
	report.Authenticated = true
	state = mustLoadRemoteState(remote)
	if *details {
		report.State = &state
		data, err := runRcloneCapture("lsd", remotePath(remote, ""))
		if err != nil {
			report.Message = strings.TrimSpace(err.Error())
			code := statusExitBackendFailed
			if isAuthError(err) {
				code = statusExitAuthFailed
			}
			return emitStatusFailure(report, code, *jsonOutput, *informational)
		}
		report.Listing = strings.TrimSpace(data)
		if !*jsonOutput {
			printStatusDetails(remote, state, report.VaultPresent)
			fmt.Println("Listing top-level folders:")
			if report.Listing == "" {
				fmt.Println("(empty)")
			} else {
				fmt.Println(report.Listing)
			}
		}
	}
	report.Healthy = true
	if *jsonOutput {
		return writeStatusJSON(report)
	}
	return nil
}

func runProtonStatus(remote string, details, jsonOutput, informational bool) error {
	report := statusReport{Backend: backendProton, Remote: normalizedRemoteName(remote), Configured: true}
	backendVersion, err := runProtonDriveCapture("version")
	if err != nil {
		report.Message = err.Error()
		return emitStatusFailure(report, statusExitBackendFailed, jsonOutput, informational)
	}
	report.Version = strings.TrimSpace(backendVersion)
	if !jsonOutput {
		fmt.Printf("Official Proton Drive CLI detected: %s\n", report.Version)
	}

	root, err := runProtonDriveCapture("filesystem", "list", "/")
	if err != nil {
		recordAuthEvent(remote, "proton-status", false, strings.TrimSpace(err.Error()))
		code := statusExitBackendFailed
		if isProtonCLIAuthError(err) {
			code = statusExitAuthFailed
			report.Message = "Official Proton Drive CLI is installed, but it is not authenticated. Run 'protondrive --backend proton configure'."
		} else {
			report.Message = "Official Proton Drive CLI is installed, but its Drive health check failed: " + strings.TrimSpace(err.Error())
		}
		return emitStatusFailure(report, code, jsonOutput, informational)
	}

	recordAuthEvent(remote, "proton-status", true, "")
	report.Authenticated = true
	report.Healthy = true
	report.Message = "Official Proton Drive CLI is authenticated."
	if details {
		state := mustLoadRemoteState(remote)
		report.State = &state
		report.VaultPresent = hasStoredCredentials(remote)
		data, err := runProtonDriveCapture("filesystem", "list", "-t", "folder", "/my-files")
		if err != nil {
			report.Healthy = false
			report.Message = strings.TrimSpace(err.Error())
			code := statusExitBackendFailed
			if isProtonCLIAuthError(err) {
				code = statusExitAuthFailed
			}
			return emitStatusFailure(report, code, jsonOutput, informational)
		}
		report.Listing = strings.TrimSpace(root) + "\n" + strings.TrimSpace(data)
	}
	if jsonOutput {
		return writeStatusJSON(report)
	}
	fmt.Println(report.Message)
	if details {
		printStatusDetails(remote, *report.State, report.VaultPresent)
		fmt.Println("Top-level Proton Drive sections:")
		fmt.Println(strings.TrimSpace(root))
		fmt.Println("\nMy files:")
		parts := strings.SplitN(report.Listing, "\n", 2)
		if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
			fmt.Println("(empty)")
		} else {
			fmt.Println(strings.TrimSpace(parts[1]))
		}
	}
	return nil
}

func isProtonCLIAuthError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if isTransientTransportMessage(message) {
		return false
	}
	for _, marker := range []string{
		"you need to login first", "not logged in", "login required",
		"authentication required", "session expired", "token expired",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func emitStatusFailure(report statusReport, code int, jsonOutput, informational bool) error {
	report.Healthy = false
	if jsonOutput {
		if err := writeStatusJSON(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, report.Message)
	}
	if informational {
		return nil
	}
	return statusExitError{code: code, message: report.Message, silent: true}
}

func writeStatusJSON(report statusReport) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(report)
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
