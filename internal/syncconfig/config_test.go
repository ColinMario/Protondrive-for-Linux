package syncconfig

import (
	"strings"
	"testing"
)

func TestParseLegacyDefaultsToCopy(t *testing.T) {
	cfg, err := Parse([]byte(`{"local_path":"~/source","direction":"upload"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Operation != OperationCopy {
		t.Fatalf("operation = %q", cfg.Operation)
	}
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", cfg.SchemaVersion, CurrentSchemaVersion)
	}
}

func TestParseRejectsUnknownAndTrailingFields(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":1,"local_path":"/tmp/source","direction":"upload","operation":"copy","directionn":"download"}`,
		`{"schema_version":1,"local_path":"/tmp/source","direction":"upload","operation":"copy"} {}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("expected error for %s", raw)
		}
	}
}

func TestParseRejectsUnsafeCopyOptions(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":1,"local_path":"/tmp/source","direction":"upload","operation":"copy","extra_rclone_args":["--delete-after"]}`,
		`{"schema_version":1,"local_path":"/tmp/source","direction":"upload","operation":"copy","source_sentinel":"ready"}`,
	} {
		_, err := Parse([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "operation 'mirror'") {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestMirrorGuardValidation(t *testing.T) {
	minusOne := -1
	cfg := Config{SchemaVersion: 1, LocalPath: "/tmp/source", Direction: "upload", Operation: OperationMirror, MaxDelete: &minusOne}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative max_delete to fail")
	}
	cfg.MaxDelete = nil
	cfg.SourceSentinel = "../outside"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected escaping sentinel to fail")
	}
}

func TestMirrorRejectsWrapperSafetyOverrides(t *testing.T) {
	for _, flag := range []string{
		"--dry-run=false",
		"-n=false",
		"--max-delete=-1",
		"--backup-dir=",
		"--ignore-errors",
		"--inplace",
		"--delete-excluded",
	} {
		cfg := Config{
			SchemaVersion:   CurrentSchemaVersion,
			LocalPath:       "/tmp/source",
			Direction:       "upload",
			Operation:       OperationMirror,
			ExtraRcloneArgs: []string{flag},
		}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected %q to be rejected", flag)
		}
	}
}
