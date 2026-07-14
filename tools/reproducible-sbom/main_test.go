package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeDocumentIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	artifactPath := filepath.Join(dir, "artifact.tar.gz")
	documentPath := filepath.Join(dir, "artifact.spdx.json")
	artifact := []byte("stable release artifact")
	if err := os.WriteFile(artifactPath, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	original := `{"z":"preserved","creationInfo":{"created":"now","creators":["Tool: syft"]},"documentNamespace":"random"}`
	if err := os.WriteFile(documentPath, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := normalizeDocument(artifactPath, documentPath, "2026-07-14T17:00:00+02:00"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeDocument(artifactPath, documentPath, "2026-07-14T15:00:00Z"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("normalization was not deterministic:\nfirst: %s\nsecond: %s", first, second)
	}

	var document map[string]any
	if err := json.Unmarshal(second, &document); err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(artifact)
	if got, want := document["documentNamespace"], namespacePrefix+hex.EncodeToString(wantDigest[:]); got != want {
		t.Fatalf("documentNamespace = %q, want %q", got, want)
	}
	creationInfo := document["creationInfo"].(map[string]any)
	if got := creationInfo["created"]; got != "2026-07-14T15:00:00Z" {
		t.Fatalf("creationInfo.created = %q", got)
	}
	if got := document["z"]; got != "preserved" {
		t.Fatalf("unrelated field changed: %q", got)
	}
	info, err := os.Stat(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("normalized document mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestNormalizeDocumentRejectsMalformedInput(t *testing.T) {
	dir := t.TempDir()
	artifactPath := filepath.Join(dir, "artifact")
	documentPath := filepath.Join(dir, "document.json")
	if err := os.WriteFile(artifactPath, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(documentPath, []byte(`{"documentNamespace":"random"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := normalizeDocument(artifactPath, documentPath, "2026-07-14T15:00:00Z"); err == nil {
		t.Fatal("missing creationInfo was accepted")
	}
	if err := normalizeDocument(artifactPath, documentPath, "not-a-date"); err == nil {
		t.Fatal("invalid commit date was accepted")
	}
}
