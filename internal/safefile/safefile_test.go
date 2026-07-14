package safefile

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteAndUpdateAreAtomicAndPreserveMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Write(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Update(path, 0o644, func(current []byte) ([]byte, os.FileMode, error) {
		return append(current, []byte("-second")...), 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first-second" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestConcurrentUpdatesDoNotLoseWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter")
	if err := Write(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Update(path, 0o600, func(current []byte) ([]byte, os.FileMode, error) {
				return append(current, 'x'), 0, nil
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 20 {
		t.Fatalf("length = %d, want 20", len(data))
	}
}

func TestWriteWithBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := Write(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteWithBackup(path, []byte("new"), 0o600, ".bak"); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old" {
		t.Fatalf("backup = %q", backup)
	}
}
