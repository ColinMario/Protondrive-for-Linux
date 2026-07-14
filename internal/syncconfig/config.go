// Package syncconfig owns the persisted transfer configuration contract.
package syncconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const CurrentSchemaVersion = 1

const (
	OperationCopy   = "copy"
	OperationMirror = "mirror"
)

// Config describes a reusable upload or download. Copy is the safe default;
// mirror must always be selected explicitly because it deletes at the target.
type Config struct {
	SchemaVersion    int      `json:"schema_version,omitempty"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	LocalPath        string   `json:"local_path"`
	RemotePath       string   `json:"remote_path"`
	Direction        string   `json:"direction"`
	Operation        string   `json:"operation,omitempty"`
	Watch            bool     `json:"watch"`
	WatchDebounce    string   `json:"watch_debounce"`
	MaxDelete        *int     `json:"max_delete,omitempty"`
	BackupDir        string   `json:"backup_dir,omitempty"`
	SourceSentinel   string   `json:"source_sentinel,omitempty"`
	AllowEmptySource bool     `json:"allow_empty_source,omitempty"`
	ExtraRcloneArgs  []string `json:"extra_rclone_args"`
}

// DisplayName returns the configured human name or a caller-provided fallback.
func (c Config) DisplayName(fallback string) string {
	if strings.TrimSpace(c.Name) != "" {
		return strings.TrimSpace(c.Name)
	}
	return fallback
}

// Parse decodes exactly one JSON object, rejects unknown fields, applies safe
// legacy defaults, and validates values that could change transfer semantics.
func Parse(data []byte) (Config, error) {
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("invalid sync config JSON: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Config{}, err
	}
	if cfg.SchemaVersion == 0 {
		// Version-less files from <=0.2.5 remain readable, but inherit the
		// non-destructive copy operation instead of the old mirror behavior.
		cfg.Operation = defaultString(cfg.Operation, OperationCopy)
		cfg.SchemaVersion = CurrentSchemaVersion
	} else if cfg.SchemaVersion != CurrentSchemaVersion {
		return Config{}, fmt.Errorf("unsupported sync config schema_version %d (supported: %d)", cfg.SchemaVersion, CurrentSchemaVersion)
	}
	cfg.Operation = strings.ToLower(strings.TrimSpace(defaultString(cfg.Operation, OperationCopy)))
	cfg.Direction = strings.ToLower(strings.TrimSpace(defaultString(cfg.Direction, "upload")))
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid trailing sync config JSON: %w", err)
	}
	return errors.New("sync config must contain exactly one JSON object")
}

// Validate checks the durable configuration contract. Runtime-only values such
// as an explicit local path supplied on the command line are validated later.
func (c Config) Validate() error {
	if c.SchemaVersion < 0 || c.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("unsupported sync config schema_version %d", c.SchemaVersion)
	}
	switch c.Direction {
	case "upload", "download":
	default:
		return errors.New("direction must be 'upload' or 'download'")
	}
	switch c.Operation {
	case OperationCopy, OperationMirror:
	default:
		return errors.New("operation must be 'copy' or 'mirror'")
	}
	if c.Watch && c.Direction != "upload" {
		return errors.New("watch mode is only supported for upload direction")
	}
	if strings.TrimSpace(c.WatchDebounce) != "" {
		duration, err := time.ParseDuration(strings.TrimSpace(c.WatchDebounce))
		if err != nil {
			return fmt.Errorf("invalid watch_debounce %q: %w", c.WatchDebounce, err)
		}
		if duration <= 0 {
			return errors.New("watch_debounce must be greater than zero")
		}
	}
	if c.MaxDelete != nil && *c.MaxDelete < 0 {
		return errors.New("max_delete must be zero or greater")
	}
	if c.Operation != OperationMirror {
		if c.MaxDelete != nil || strings.TrimSpace(c.BackupDir) != "" || strings.TrimSpace(c.SourceSentinel) != "" || c.AllowEmptySource {
			return errors.New("max_delete, backup_dir, source_sentinel, and allow_empty_source require operation 'mirror'")
		}
	}
	if sentinel := strings.TrimSpace(c.SourceSentinel); sentinel != "" {
		if filepath.IsAbs(sentinel) || sentinel == ".." || strings.HasPrefix(filepath.ToSlash(filepath.Clean(sentinel)), "../") {
			return errors.New("source_sentinel must be a relative path inside the source")
		}
	}
	for _, arg := range c.ExtraRcloneArgs {
		arg = strings.TrimSpace(arg)
		if arg == "" || !strings.HasPrefix(arg, "-") {
			return fmt.Errorf("extra_rclone_args entries must be flags, got %q", arg)
		}
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		switch name {
		case "config", "password-command", "rc", "rc-addr":
			return fmt.Errorf("extra rclone flag --%s is not allowed in persisted configs", name)
		case "dry-run", "n", "max-delete", "backup-dir", "ignore-errors", "inplace":
			return fmt.Errorf("extra rclone flag --%s is managed by the wrapper and cannot be overridden", name)
		case "delete-excluded":
			return errors.New("extra rclone flag --delete-excluded is not allowed because it expands mirror deletion scope")
		case "delete-before", "delete-during", "delete-after":
			if c.Operation != OperationMirror {
				return fmt.Errorf("destructive rclone flag --%s requires operation 'mirror'", name)
			}
		}
	}
	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
