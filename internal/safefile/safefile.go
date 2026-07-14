// Package safefile provides crash-safe, serialized updates for application files.
package safefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLockTimeout = 5 * time.Second
	lockPollInterval   = 25 * time.Millisecond
	staleLockAge       = 10 * time.Minute
)

// Write atomically replaces path while holding a cooperative lock. Existing
// contents remain available at path until the replacement has been synced.
func Write(path string, data []byte, perm fs.FileMode) error {
	return WithLock(path, defaultLockTimeout, func() error {
		return writeAtomic(path, data, perm)
	})
}

// Remove serializes removal with writers. Missing files are treated as success.
func Remove(path string) error {
	return WithLock(path, defaultLockTimeout, func() error {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// Update serializes a read-modify-write operation. Missing files are supplied
// as nil. The callback may return a replacement mode of zero to use fallbackPerm.
func Update(path string, fallbackPerm fs.FileMode, update func(current []byte) ([]byte, fs.FileMode, error)) error {
	return WithLock(path, defaultLockTimeout, func() error {
		current, err := os.ReadFile(path) // #nosec G304 -- this package intentionally updates caller-selected application files
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mode := fallbackPerm
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		next, requestedMode, err := update(current)
		if err != nil {
			return err
		}
		if requestedMode != 0 {
			mode = requestedMode
		}
		return writeAtomic(path, next, mode)
	})
}

// UpdateWithBackup is Update plus a durable copy of the previous file. The
// backup is created only when path already exists.
func UpdateWithBackup(path string, fallbackPerm fs.FileMode, backupSuffix string, update func(current []byte) ([]byte, fs.FileMode, error)) error {
	return WithLock(path, defaultLockTimeout, func() error {
		current, err := os.ReadFile(path) // #nosec G304 -- this package intentionally updates caller-selected application files
		exists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mode := fallbackPerm
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		next, requestedMode, err := update(current)
		if err != nil {
			return err
		}
		if requestedMode != 0 {
			mode = requestedMode
		}
		if strings.TrimSpace(backupSuffix) == "" {
			backupSuffix = ".bak"
		}
		if exists {
			if err := writeAtomic(path+backupSuffix, current, mode); err != nil {
				return fmt.Errorf("write backup: %w", err)
			}
		}
		return writeAtomic(path, next, mode)
	})
}

// WriteWithBackup atomically replaces path and keeps the previous bytes in
// path+backupSuffix. Both writes are serialized by the same lock.
func WriteWithBackup(path string, data []byte, perm fs.FileMode, backupSuffix string) error {
	return WithLock(path, defaultLockTimeout, func() error {
		if strings.TrimSpace(backupSuffix) == "" {
			backupSuffix = ".bak"
		}
		previous, err := os.ReadFile(path) // #nosec G304 -- this package intentionally backs up caller-selected application files
		if err == nil {
			previousMode := perm
			if info, statErr := os.Stat(path); statErr == nil {
				previousMode = info.Mode().Perm()
			}
			if err := writeAtomic(path+backupSuffix, previous, previousMode); err != nil {
				return fmt.Errorf("write backup: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return writeAtomic(path, data, perm)
	})
}

// WithLock uses a portable lock file so concurrent wrapper processes cannot
// interleave updates. A very old lock is removed to recover from hard crashes.
func WithLock(path string, timeout time.Duration, fn func() error) error {
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lockPath := path + ".lock"
	deadline := time.Now().Add(timeout)
	for {
		lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- lock path is derived from the caller-selected target
		if err == nil {
			_, writeErr := fmt.Fprintf(lock, "%d\n%d\n", os.Getpid(), time.Now().Unix())
			closeErr := lock.Close()
			if writeErr != nil {
				_ = os.Remove(lockPath)
				return writeErr
			}
			if closeErr != nil {
				_ = os.Remove(lockPath)
				return closeErr
			}
			defer os.Remove(lockPath)
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if stale, staleErr := lockIsStale(lockPath); staleErr == nil && stale {
			_ = os.Remove(lockPath)
			continue
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for lock %s", lockPath)
		}
		time.Sleep(lockPollInterval)
	}
}

func lockIsStale(path string) (bool, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- stale-lock inspection uses the internally derived lock path
	if err != nil {
		return false, err
	}
	lines := strings.Fields(string(data))
	if len(lines) >= 2 {
		if unix, parseErr := strconv.ParseInt(lines[1], 10, 64); parseErr == nil {
			return time.Since(time.Unix(unix, 0)) > staleLockAge, nil
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return time.Since(info.ModTime()) > staleLockAge, nil
}

func writeAtomic(path string, data []byte, perm fs.FileMode) (returnErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(perm); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if dirHandle, err := os.Open(dir); err == nil { // #nosec G304 -- directory is the parent of the caller-selected target
		defer dirHandle.Close()
		_ = dirHandle.Sync()
	}
	return nil
}
