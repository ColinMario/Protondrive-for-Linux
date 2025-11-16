package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("unable to determine home directory: %v", err)
	}

	tmp := filepath.Join(os.TempDir(), "protondrive-path")

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "noTilde", input: tmp, want: tmp},
		{name: "tildeOnly", input: "~", want: home},
		{name: "tildeSlash", input: "~/", want: home},
		{name: "tildeNested", input: "~/ProtonDrive/sub", want: filepath.Join(home, "ProtonDrive", "sub")},
		{name: "tildeBackslash", input: "~\\ProtonDrive", want: filepath.Join(home, "ProtonDrive")},
		{name: "tildeUsername", input: "~someone", want: "~someone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expandPath(tt.input); got != tt.want {
				t.Fatalf("expandPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestComputeBackoffDelay(t *testing.T) {
	initial := 10 * time.Second
	max := 1 * time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: initial},
		{failures: 1, want: initial},
		{failures: 2, want: 2 * initial},
		{failures: 3, want: 4 * initial},
		{failures: 4, want: max},
		{failures: 5, want: max},
	}
	for _, tc := range cases {
		if got := computeBackoffDelay(initial, max, tc.failures); got != tc.want {
			t.Fatalf("computeBackoffDelay(%s, %s, %d) = %s, want %s", initial, max, tc.failures, got, tc.want)
		}
	}
}
