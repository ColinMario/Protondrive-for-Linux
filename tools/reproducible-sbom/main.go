// Command reproducible-sbom runs Syft and replaces its two intentionally
// variable SPDX fields with values derived from the release commit and artifact.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ColinMario/Protondrive-for-Linux/internal/safefile"
)

const namespacePrefix = "https://github.com/ColinMario/Protondrive-for-Linux/sbom/sha256/"

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: reproducible-sbom <artifact> <document> <commit-date>")
		os.Exit(2)
	}
	if err := generateDocument(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "reproducible-sbom:", err)
		os.Exit(1)
	}
}

func generateDocument(artifactPath, documentPath, commitDate string) error {
	if artifactPath == "" || documentPath == "" || commitDate == "" {
		return errors.New("artifact, document, and commit date are required")
	}
	cmd := exec.Command("syft", artifactPath, "--output", "spdx-json="+documentPath, "--enrich", "all") // #nosec G204,G702 -- GoReleaser supplies the artifact and output paths; no shell is involved
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("syft failed: %w", err)
	}
	return normalizeDocument(artifactPath, documentPath, commitDate)
}

func normalizeDocument(artifactPath, documentPath, commitDate string) error {
	created, err := time.Parse(time.RFC3339, commitDate)
	if err != nil {
		return fmt.Errorf("invalid commit date %q: %w", commitDate, err)
	}
	digest, err := sha256File(artifactPath)
	if err != nil {
		return fmt.Errorf("hash artifact: %w", err)
	}

	raw, err := os.ReadFile(documentPath) // #nosec G304,G703 -- GoReleaser supplies the generated SBOM path
	if err != nil {
		return fmt.Errorf("read SPDX document: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode SPDX document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("SPDX document contains trailing JSON")
		}
		return fmt.Errorf("decode trailing SPDX data: %w", err)
	}
	creationInfo, ok := document["creationInfo"].(map[string]any)
	if !ok {
		return errors.New("SPDX document is missing creationInfo")
	}
	document["documentNamespace"] = namespacePrefix + digest
	creationInfo["created"] = created.UTC().Format(time.RFC3339)

	normalized, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode normalized SPDX document: %w", err)
	}
	normalized = append(normalized, '\n')
	mode := fs.FileMode(0o644)
	if info, statErr := os.Stat(documentPath); statErr == nil { // #nosec G703 -- GoReleaser supplies the generated SBOM path
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := safefile.Write(documentPath, normalized, mode); err != nil {
		return fmt.Errorf("write normalized SPDX document: %w", err)
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(filepath.Clean(path)) // #nosec G304 -- GoReleaser supplies the release artifact path
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
