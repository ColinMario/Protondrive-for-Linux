package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ColinMario/Protondrive-for-Linux/internal/customconfigs"
	"github.com/ColinMario/Protondrive-for-Linux/internal/syncconfig"
)

func syncConfigDirPath() (string, error) {
	dir, err := credentialDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync-configs"), nil
}

func ensureSyncConfigDir() (string, error) {
	dir, err := syncConfigDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- sync config directory is user-editable application config
		return "", err
	}
	return dir, nil
}

func loadSyncConfig(identifier string) (loadedSyncConfig, error) {
	name := strings.TrimSpace(identifier)
	if name == "" {
		return loadedSyncConfig{}, errors.New("sync config name cannot be empty")
	}

	if strings.Contains(name, "/") || strings.Contains(name, "\\") || filepath.Ext(name) != "" {
		path := expandPath(name)
		cfg, err := readSyncConfigFile(path)
		if err != nil {
			return loadedSyncConfig{}, err
		}
		return loadedSyncConfig{
			Config:      cfg,
			Source:      path,
			DisplayName: cfg.DisplayName(filepath.Base(path)),
		}, nil
	}

	dir, err := syncConfigDirPath()
	if err != nil {
		return loadedSyncConfig{}, err
	}

	for _, candidate := range uniqueStrings(configFileCandidates(dir, name)) {
		if cfg, err := readSyncConfigFile(candidate); err == nil {
			return loadedSyncConfig{
				Config:      cfg,
				Source:      candidate,
				DisplayName: cfg.DisplayName(filepath.Base(candidate)),
			}, nil
		}
	}

	template, found, err := customconfigs.Lookup(name)
	if err != nil {
		return loadedSyncConfig{}, fmt.Errorf("unable to load built-in templates: %w", err)
	}
	if found {
		cfg, err := parseSyncConfig(template.Raw)
		if err != nil {
			return loadedSyncConfig{}, fmt.Errorf("template %s is invalid: %w", template.Name, err)
		}
		if strings.TrimSpace(cfg.Name) == "" {
			cfg.Name = template.Name
		}
		return loadedSyncConfig{
			Config:      cfg,
			Source:      "builtin:" + template.ID,
			DisplayName: cfg.DisplayName(template.Name),
		}, nil
	}

	return loadedSyncConfig{}, fmt.Errorf("sync config %q not found. Place JSON files in %s or run 'protondrive configs list' to see built-in templates", name, dir)
}

func readSyncConfigFile(path string) (syncConfig, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- sync config path may be explicitly provided by the user
	if err != nil {
		return syncConfig{}, err
	}
	cfg, err := parseSyncConfig(data)
	if err != nil {
		return syncConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return cfg, nil
}

func parseSyncConfig(data []byte) (syncConfig, error) {
	return syncconfig.Parse(data)
}

func describeConfigSource(source string) string {
	if strings.HasPrefix(source, "builtin:") {
		return fmt.Sprintf("built-in template %s", strings.TrimPrefix(source, "builtin:"))
	}
	if strings.TrimSpace(source) != "" {
		return source
	}
	return "custom config"
}

func configFileCandidates(dir, name string) []string {
	var candidates []string
	clean := strings.TrimSpace(name)
	if clean != "" {
		candidates = append(candidates, filepath.Join(dir, clean))
		if filepath.Ext(clean) == "" {
			candidates = append(candidates, filepath.Join(dir, clean+".json"))
		}
	}
	slug := slugifyConfigName(clean)
	candidates = append(candidates, filepath.Join(dir, slug), filepath.Join(dir, slug+".json"))
	return candidates
}

func slugifyConfigName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "config"
	}
	var builder strings.Builder
	prevDash := false

	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			prevDash = false
			continue
		}
		switch r {
		case ' ', '-', '_', '.', '/', '\\':
			if !prevDash {
				builder.WriteRune('-')
				prevDash = true
			}
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "config"
	}
	return result
}

func configFileName(name string) string {
	base := slugifyConfigName(name)
	if !strings.HasSuffix(base, ".json") {
		base += ".json"
	}
	return base
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var result []string
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func listCustomSyncConfigs() ([]syncConfigSummary, string, error) {
	dir, err := syncConfigDirPath()
	if err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, dir, nil
		}
		return nil, "", err
	}
	summaries := make([]syncConfigSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		summary, err := readSyncConfigSummary(path)
		if err != nil {
			summary = syncConfigSummary{
				Name:        strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
				Description: fmt.Sprintf("invalid config: %v", err),
				File:        path,
			}
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})
	return summaries, dir, nil
}

func readSyncConfigSummary(path string) (syncConfigSummary, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- summary reads JSON files discovered inside the sync config directory
	if err != nil {
		return syncConfigSummary{}, err
	}
	cfg, err := parseSyncConfig(data)
	if err != nil {
		return syncConfigSummary{}, err
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	desc := strings.TrimSpace(cfg.Description)
	if desc == "" {
		desc = "(no description)"
	}
	return syncConfigSummary{Name: name, Description: desc, File: path}, nil
}
